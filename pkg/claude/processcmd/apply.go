package processcmd

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/common"
)

type applyParams struct {
	RunID         string   `pos:"true" help:"Schema-8 process run id"`
	StoreRoot     string   `long:"store-root" help:"Canonical daemon process store root"`
	CandidateFile string   `long:"candidate-file" help:"Exact candidate ProcessTemplate YAML file"`
	ReasonFile    string   `long:"reason-file" optional:"true" help:"Optional exact reason file (an empty file is a present empty reason)"`
	BaseRevision  uint64   `long:"base-revision" help:"Exact preview base revision"`
	BaseDigest    string   `long:"base-digest" help:"Exact preview base digest"`
	ApplyToken    string   `long:"apply-token" help:"Opaque token returned by the exact valid preview"`
	Handoff       []string `long:"handoff" optional:"true" help:"Opaque handoff: TOKEN=retain or TOKEN=transfer:LOCAL:RESERVATION:NODE"`
	AskHuman      string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (for example 30s). Capped at 300s; timeout means deny."`
}

func applyCmd() *cobra.Command {
	return boa.CmdT[applyParams]{
		Use: "apply", Short: "Atomically apply an exact schema-8 unlock preview",
		ParamEnrich: common.DefaultParamEnricher(), Args: cobra.ExactArgs(1),
		PreExecuteFunc: func(p *applyParams, _ *cobra.Command, _ []string) error {
			if err := requireProcessesEnabled(); err != nil {
				return err
			}
			if strings.TrimSpace(p.StoreRoot) == "" || strings.TrimSpace(p.CandidateFile) == "" ||
				strings.TrimSpace(p.BaseDigest) == "" || strings.TrimSpace(p.ApplyToken) == "" {
				return fmt.Errorf("--store-root, --candidate-file, --base-digest, and --apply-token are required")
			}
			return requireCanonicalProcessStore(p.StoreRoot)
		},
		RunFunc: func(p *applyParams, cmd *cobra.Command, _ []string) { exitWithError(runApply(cmd, p, os.Stdout)) },
	}.ToCobra()
}

func runApply(cmd *cobra.Command, p *applyParams, out io.Writer) error {
	if err := requireCanonicalProcessStore(p.StoreRoot); err != nil {
		return err
	}
	ask, err := agent.ParseAskHuman(p.AskHuman)
	if err != nil {
		return err
	}
	source, err := readBoundedProcessFile(p.CandidateFile, model.MaxProcessTemplateSourceBytes)
	if err != nil {
		return err
	}
	type target struct {
		LocalID       string `json:"localId"`
		ReservationID string `json:"reservationId"`
		NodeID        string `json:"nodeId"`
	}
	type handoff struct {
		Token  string  `json:"token"`
		Action string  `json:"action"`
		Target *target `json:"target,omitempty"`
	}
	handoffs := make([]handoff, 0, len(p.Handoff))
	for _, encoded := range p.Handoff {
		token, value, ok := strings.Cut(encoded, "=")
		if !ok {
			return fmt.Errorf("invalid --handoff %q", encoded)
		}
		parts := strings.Split(value, ":")
		switch {
		case len(parts) == 1 && parts[0] == "retain":
			handoffs = append(handoffs, handoff{Token: token, Action: "retain_owner_epoch"})
		case len(parts) == 4 && parts[0] == "transfer":
			handoffs = append(handoffs, handoff{Token: token, Action: "transfer_verified_unclaimed", Target: &target{parts[1], parts[2], parts[3]}})
		default:
			return fmt.Errorf("invalid --handoff %q", encoded)
		}
	}
	body := struct {
		BaseBinding struct {
			Revision uint64 `json:"revision"`
			Digest   string `json:"digest"`
		} `json:"baseBinding"`
		ApplyToken      string    `json:"applyToken"`
		CandidateSource string    `json:"candidateSource"`
		Reason          *string   `json:"reason,omitempty"`
		Handoffs        []handoff `json:"handoffs"`
	}{ApplyToken: p.ApplyToken, CandidateSource: string(source), Handoffs: handoffs}
	body.BaseBinding.Revision, body.BaseBinding.Digest = p.BaseRevision, p.BaseDigest
	if p.ReasonFile != "" {
		reason, readErr := readBoundedProcessFile(p.ReasonFile, store.EpochV8MaxReasonBytes)
		if readErr != nil {
			return readErr
		}
		value := string(reason)
		body.Reason = &value
	}
	var response struct {
		Status      string `json:"status"`
		Disposition string `json:"disposition"`
		EpochID     string `json:"epochId"`
		ReasonCode  string `json:"reasonCode"`
		Actor       string `json:"actor"`
		AppliedAt   string `json:"appliedAt"`
	}
	if err := agent.DaemonRequest(http.MethodPost, "/v1/process/runs/"+url.PathEscape(p.RunID)+"/unlock/apply", body, &response,
		agent.DaemonOpts{Timeout: schema8DaemonTimeout, AskHuman: ask}); err != nil {
		return err
	}
	if response.Status != "applied" && response.Status != "already_applied" {
		return fmt.Errorf("daemon returned an invalid schema-8 apply status")
	}
	fmt.Fprintf(out, "Schema-8 unlock %s for run %s at epoch %s (%s by %s at %s)\n",
		response.Status, p.RunID, response.EpochID, response.ReasonCode, response.Actor, response.AppliedAt)
	return nil
}
