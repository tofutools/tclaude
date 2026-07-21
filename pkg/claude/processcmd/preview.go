package processcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/common"
)

var (
	errPreviewStale   = errors.New("process preview binding is stale")
	errPreviewBlocked = errors.New("process preview is blocked")
)

type previewBindingResponse struct {
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

type previewGraphTotalsResponse struct {
	Nodes int `json:"nodes"`
	Edges int `json:"edges"`
}

type previewGraphSummaryResponse struct {
	Current   previewGraphTotalsResponse `json:"current"`
	Candidate previewGraphTotalsResponse `json:"candidate"`
	Changed   bool                       `json:"changed"`
}

type previewLineageEntryResponse struct {
	Ordinal            uint64  `json:"ordinal"`
	PredecessorOrdinal *uint64 `json:"predecessorOrdinal,omitempty"`
	TemplateRef        string  `json:"templateRef"`
}

type previewLineageResponse struct {
	OriginalTemplateRef string                        `json:"originalTemplateRef"`
	CurrentTemplateRef  string                        `json:"currentTemplateRef"`
	Epochs              []previewLineageEntryResponse `json:"epochs"`
}

type previewAuthorityCountsResponse struct {
	Total    int `json:"total"`
	Active   int `json:"active"`
	Terminal int `json:"terminal"`
}

type previewBlockerResponse struct {
	Code  string `json:"code"`
	Token string `json:"token,omitempty"`
}

type previewGuidanceResponse struct {
	Action            string   `json:"action"`
	Permission        string   `json:"permission"`
	Token             string   `json:"token"`
	Preconditions     []string `json:"preconditions"`
	RepreviewRequired bool     `json:"repreviewRequired"`
}

type previewBlockedResponse struct {
	Status          string                         `json:"status"`
	BaseBinding     previewBindingResponse         `json:"baseBinding"`
	CurrentBinding  previewBindingResponse         `json:"currentBinding"`
	Classification  string                         `json:"classification,omitempty"`
	GraphSummary    previewGraphSummaryResponse    `json:"graphSummary"`
	Lineage         previewLineageResponse         `json:"lineage"`
	AuthorityCounts previewAuthorityCountsResponse `json:"authorityCounts"`
	Blockers        []previewBlockerResponse       `json:"blockers,omitempty"`
	Guidance        *previewGuidanceResponse       `json:"guidance,omitempty"`
}

type previewParams struct {
	RunID         string   `pos:"true" help:"Schema-8 process run id"`
	StoreRoot     string   `long:"store-root" help:"Filesystem process store root (schema 8 requires the canonical daemon store)"`
	CandidateFile string   `long:"candidate-file" help:"Exact candidate ProcessTemplate YAML file"`
	ReasonFile    string   `long:"reason-file" optional:"true" help:"Optional exact reason file (an empty file is a present empty reason)"`
	BaseRevision  uint64   `long:"base-revision" help:"Exact preview base revision"`
	BaseDigest    string   `long:"base-digest" help:"Exact preview base digest"`
	Handoff       []string `long:"handoff" optional:"true" help:"Opaque handoff: TOKEN=retain or TOKEN=transfer:LOCAL:RESERVATION:NODE"`
}

func previewCmd() *cobra.Command {
	return boa.CmdT[previewParams]{
		Use: "preview", Short: "Preview a schema-8 template epoch without applying it",
		ParamEnrich: common.DefaultParamEnricher(), Args: cobra.ExactArgs(1),
		PreExecuteFunc: func(p *previewParams, _ *cobra.Command, _ []string) error {
			if err := requireProcessesEnabled(); err != nil {
				return err
			}
			if strings.TrimSpace(p.StoreRoot) == "" || strings.TrimSpace(p.CandidateFile) == "" {
				return fmt.Errorf("--store-root and --candidate-file are required")
			}
			return requireCanonicalProcessStore(p.StoreRoot)
		},
		RunFunc: func(p *previewParams, cmd *cobra.Command, _ []string) { exitWithError(runPreview(cmd, p, os.Stdout)) },
	}.ToCobra()
}

func runPreview(cmd *cobra.Command, p *previewParams, out io.Writer) error {
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
		CandidateSource string    `json:"candidateSource"`
		Reason          *string   `json:"reason,omitempty"`
		Handoffs        []handoff `json:"handoffs"`
	}{CandidateSource: string(source), Handoffs: handoffs}
	body.BaseBinding.Revision, body.BaseBinding.Digest = p.BaseRevision, p.BaseDigest
	if p.ReasonFile != "" {
		reason, readErr := readBoundedProcessFile(p.ReasonFile, store.EpochV8MaxReasonBytes)
		if readErr != nil {
			return readErr
		}
		value := string(reason)
		body.Reason = &value
	}
	var response json.RawMessage
	if err := agent.DaemonRequest("POST", "/v1/process/runs/"+p.RunID+"/unlock/preview", body, &response, agent.DaemonOpts{Timeout: schema8DaemonTimeout}); err != nil {
		var daemonErr *agent.DaemonError
		if errors.As(err, &daemonErr) {
			if domainErr, handled := renderPreviewDomainError(out, daemonErr); handled {
				return domainErr
			}
		}
		return err
	}
	var pretty any
	if err := json.Unmarshal(response, &pretty); err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(pretty)
}

func renderPreviewDomainError(out io.Writer, daemonErr *agent.DaemonError) (error, bool) {
	if daemonErr == nil {
		return nil, false
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	switch daemonErr.Status {
	case http.StatusConflict:
		var response struct {
			Status         string                 `json:"status"`
			CurrentBinding previewBindingResponse `json:"currentBinding"`
		}
		if err := decodeStrictPreviewResponse(daemonErr.Raw, &response); err != nil || response.Status != "stale" || !validPreviewBinding(response.CurrentBinding) {
			return nil, false
		}
		if err := encoder.Encode(response); err != nil {
			return err, true
		}
		return errPreviewStale, true
	case http.StatusUnprocessableEntity:
		var response previewBlockedResponse
		if err := decodeStrictPreviewResponse(daemonErr.Raw, &response); err != nil || !validBlockedPreviewResponse(response) {
			return nil, false
		}
		if err := encoder.Encode(response); err != nil {
			return err, true
		}
		return errPreviewBlocked, true
	default:
		return nil, false
	}
}

func decodeStrictPreviewResponse(raw []byte, value any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("preview response contains trailing JSON")
	}
	return nil
}

func validBlockedPreviewResponse(response previewBlockedResponse) bool {
	if response.Status != "blocked" || response.Classification != "" || response.Guidance != nil ||
		!validPreviewBinding(response.BaseBinding) || !validPreviewBinding(response.CurrentBinding) ||
		response.GraphSummary.Current.Nodes <= 0 || response.GraphSummary.Candidate.Nodes <= 0 ||
		response.Lineage.OriginalTemplateRef == "" || response.Lineage.CurrentTemplateRef == "" || len(response.Lineage.Epochs) == 0 ||
		response.AuthorityCounts.Total <= 0 || response.AuthorityCounts.Active+response.AuthorityCounts.Terminal != response.AuthorityCounts.Total ||
		len(response.Blockers) == 0 {
		return false
	}
	for _, entry := range response.Lineage.Epochs {
		if entry.TemplateRef == "" {
			return false
		}
	}
	for _, blocker := range response.Blockers {
		if !validPreviewBlockerCode(blocker.Code) || blocker.Token != "" && !isLowerHexDigest(blocker.Token) {
			return false
		}
	}
	return true
}

func validPreviewBlockerCode(code string) bool {
	switch epochv8.BlockerCode(code) {
	case epochv8.BlockerStaleBinding,
		epochv8.BlockerHandoffMissing,
		epochv8.BlockerHandoffDuplicate,
		epochv8.BlockerHandoffUnknown,
		epochv8.BlockerClaimed,
		epochv8.BlockerActiveCommand,
		epochv8.BlockerActiveWait,
		epochv8.BlockerActiveTimer,
		epochv8.BlockerActiveObligation,
		epochv8.BlockerActiveContact,
		epochv8.BlockerDispatchedSideEffect,
		epochv8.BlockerActiveOutcome,
		epochv8.BlockerActiveParallel,
		epochv8.BlockerActiveJoin,
		epochv8.BlockerActivePropagation,
		epochv8.BlockerActiveDetachment,
		epochv8.BlockerActiveRetry,
		epochv8.BlockerActiveRollbackForward,
		epochv8.BlockerActiveAuthority,
		epochv8.BlockerNotTransferable:
		return true
	default:
		return false
	}
}

func validPreviewBinding(binding previewBindingResponse) bool {
	return isLowerHexDigest(binding.Digest)
}

func isLowerHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func readBoundedProcessFile(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("file exceeds %d-byte limit", limit)
	}
	return data, nil
}
