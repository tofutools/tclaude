package processcmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/common"
)

type resolveParams struct {
	RunID       string `pos:"true" help:"Process run id"`
	NodeID      string `pos:"true" help:"Node with a pending human obligation"`
	StoreRoot   string `long:"store-root" help:"Filesystem process store root"`
	Verdict     string `long:"verdict" help:"Resolution verdict"`
	Actor       string `long:"actor" help:"Human actor ref (human:<id>)"`
	EvidenceRef string `long:"evidence" optional:"true" help:"Evidence artifact/reference"`
	Feedback    string `long:"feedback" optional:"true" help:"Feedback for a retry loop"`
}

func resolveCmd() *cobra.Command {
	return boa.CmdT[resolveParams]{
		Use:         "resolve",
		Short:       "Resolve a pending human process obligation",
		ParamEnrich: common.DefaultParamEnricher(),
		Args:        cobra.ExactArgs(2),
		PreExecuteFunc: func(p *resolveParams, _ *cobra.Command, _ []string) error {
			if err := requireProcessesEnabled(); err != nil {
				return err
			}
			if strings.TrimSpace(p.StoreRoot) == "" {
				return fmt.Errorf("--store-root is required")
			}
			if strings.TrimSpace(p.Verdict) == "" {
				return fmt.Errorf("--verdict is required")
			}
			actor := state.ActorRef(strings.TrimSpace(p.Actor))
			if !state.ValidateActorRef(actor) || !strings.HasPrefix(string(actor), "human:") {
				return fmt.Errorf("--actor must be a valid human:<id> actor ref")
			}
			return nil
		},
		RunFunc: func(p *resolveParams, cmd *cobra.Command, _ []string) {
			exitWithError(runResolve(cmd, p, os.Stdout))
		},
	}.ToCobra()
}

func runResolve(cmd *cobra.Command, p *resolveParams, out io.Writer) error {
	fs, err := openStore(p.StoreRoot, true)
	if err != nil {
		return err
	}
	kind, err := fs.RunStateSchemaKind(cmd.Context(), p.RunID)
	if err != nil {
		return err
	}
	if kind == store.RunSchemaResetRequired {
		return fmt.Errorf("%w: process run %q", store.ErrRunResetRequired, p.RunID)
	}
	if kind == store.RunSchemaEpochV8 {
		return fmt.Errorf("schema-8 process obligations are not released")
	}
	if err := ensureRunVerifies(cmd.Context(), fs, p.RunID, out); err != nil {
		return err
	}
	snapshot, err := fs.LoadRun(cmd.Context(), p.RunID)
	if err != nil {
		return err
	}
	commandID := ""
	for _, obligation := range snapshot.State.Obligations {
		if obligation.NodeID != p.NodeID || obligation.Kind != state.WaitKindHuman || obligation.Status != state.WaitStatusPending {
			continue
		}
		if commandID != "" && commandID != obligation.CommandID {
			return fmt.Errorf("node %q has multiple pending human obligations", p.NodeID)
		}
		commandID = obligation.CommandID
	}
	if commandID == "" {
		return fmt.Errorf("node %q has no pending human obligation", p.NodeID)
	}
	executor := processexec.New(fs, nil)
	_, err = executor.RecordOutstandingObservation(cmd.Context(), p.RunID, commandID, processexec.Observation{
		Actor:       state.ActorRef(strings.TrimSpace(p.Actor)),
		Verdict:     strings.TrimSpace(p.Verdict),
		Feedback:    strings.TrimSpace(p.Feedback),
		EvidenceRef: strings.TrimSpace(p.EvidenceRef),
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Resolved process obligation for run %s node %s (%s)\n", p.RunID, p.NodeID, commandID)
	return nil
}
