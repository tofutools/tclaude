package processcmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/common"
)

type unblockParams struct {
	RunID        string `pos:"true" help:"Process run id containing the blocked node"`
	NodeID       string `pos:"true" help:"Blocked stage child or its parent mirror"`
	StoreRoot    string `long:"store-root" help:"Filesystem process store root"`
	Decision     string `long:"decision" help:"Resolution decision: retry, skip, or cancel"`
	Reason       string `long:"reason" help:"Reason for the resolution decision"`
	EvidenceRef  string `long:"evidence" help:"Evidence artifact/reference supporting the decision"`
	Actor        string `long:"actor" optional:"true" help:"Actor ref (default human:<current user>)"`
	BaseRevision uint64 `long:"base-revision" optional:"true" help:"Exact schema-8 outer binding revision"`
	BaseDigest   string `long:"base-digest" optional:"true" help:"Exact schema-8 outer binding digest"`
}

func unblockCmd() *cobra.Command {
	return boa.CmdT[unblockParams]{
		Use:         "unblock",
		Short:       "Resolve a poison-blocked process node",
		Long:        "Record an audited retry, skip, or cancel decision and atomically clear a poisoned stage child plus its parent mirror.",
		ParamEnrich: common.DefaultParamEnricher(),
		Args:        cobra.ExactArgs(2),
		PreExecuteFunc: func(p *unblockParams, _ *cobra.Command, _ []string) error {
			if err := requireProcessesEnabled(); err != nil {
				return err
			}
			if strings.TrimSpace(p.StoreRoot) == "" {
				return fmt.Errorf("--store-root is required")
			}
			return nil
		},
		RunFunc: func(p *unblockParams, cmd *cobra.Command, _ []string) {
			exitWithError(runUnblockDispatch(cmd, p, os.Stdout))
		},
	}.ToCobra()
}

func runUnblockDispatch(cmd *cobra.Command, p *unblockParams, out io.Writer) error {
	canonical := requireCanonicalProcessStore(p.StoreRoot) == nil
	kind, probeErr := localRunSchema(cmd.Context(), p.StoreRoot, p.RunID)
	if !canonical {
		if probeErr == nil && kind == store.RunSchemaEpochV8 {
			return requireCanonicalProcessStore(p.StoreRoot)
		}
		return runUnblock(cmd, p, out)
	}
	if probeErr == nil && kind != store.RunSchemaEpochV8 {
		return runUnblock(cmd, p, out)
	}
	if strings.TrimSpace(p.Actor) != "" {
		return fmt.Errorf("schema-8 settlement actor identity is derived by the daemon; --actor is supported only for legacy runs")
	}
	if p.BaseDigest == "" {
		return fmt.Errorf("schema-8 unblock requires --base-revision and --base-digest from preview guidance")
	}
	body := struct {
		BaseBinding struct {
			Revision uint64 `json:"revision"`
			Digest   string `json:"digest"`
		} `json:"baseBinding"`
		Token       string `json:"token"`
		Decision    string `json:"decision"`
		Reason      string `json:"reason"`
		EvidenceRef string `json:"evidenceRef"`
	}{Token: p.NodeID, Decision: p.Decision, Reason: p.Reason, EvidenceRef: p.EvidenceRef}
	body.BaseBinding.Revision, body.BaseBinding.Digest = p.BaseRevision, p.BaseDigest
	var response struct {
		Settled           bool   `json:"settled"`
		Decision          string `json:"decision"`
		RepreviewRequired bool   `json:"repreviewRequired"`
	}
	if err := agent.DaemonRequest("POST", "/v1/process/runs/"+p.RunID+"/unblock", body, &response, agent.DaemonOpts{Timeout: schema8DaemonTimeout}); err != nil {
		return err
	}
	fmt.Fprintf(out, "Resolved schema-8 settlement in run %s with decision %s; re-preview required\n", p.RunID, response.Decision)
	return nil
}

func runUnblock(cmd *cobra.Command, p *unblockParams, out io.Writer) error {
	fs, err := openStore(p.StoreRoot, true)
	if err != nil {
		return err
	}
	actor := state.ActorRef(strings.TrimSpace(p.Actor))
	if actor == "" {
		actor = defaultActor()
	}
	schema, err := fs.RunStateSchemaVersion(cmd.Context(), strings.TrimSpace(p.RunID))
	if err != nil {
		return err
	}
	if schema == epochv8.StateSchemaVersion {
		checkpoint, resolution, resolveErr := processexec.NewEpochV8External(fs).ResolveAuditedSettlement(
			cmd.Context(), p.RunID, p.NodeID, p.Decision, string(actor), p.Reason, p.EvidenceRef,
		)
		if resolveErr != nil {
			return resolveErr
		}
		fmt.Fprintf(out, "Resolved blocked node %s in run %s with decision %s at seq %d\n", resolution.NodeID, p.RunID, resolution.Decision, pathv1.CurrentLastLogSeq(checkpoint))
		return nil
	}
	executor := processexec.New(fs, nil)
	request := processexec.BlockResolutionRequest{
		RunID:       p.RunID,
		NodeID:      p.NodeID,
		Decision:    state.BlockDecision(p.Decision),
		Actor:       actor,
		Reason:      p.Reason,
		EvidenceRef: p.EvidenceRef,
	}
	snapshot, err := fs.LoadRun(cmd.Context(), strings.TrimSpace(p.RunID))
	if err != nil {
		return err
	}
	request, err = processexec.BindBlockResolution(snapshot, request)
	if err != nil {
		return err
	}
	resolved, err := executor.ResolveBlocked(cmd.Context(), request)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Resolved blocked node %s in run %s with decision %s at seq %d\n", request.NodeID, p.RunID, strings.ToLower(strings.TrimSpace(p.Decision)), resolved.LastLogSeq)
	return nil
}
