package processcmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/common"
)

type observeParams struct {
	RunID       string `pos:"true" help:"Process run id containing the outstanding command"`
	CommandID   string `pos:"true" help:"Issued performer command id to observe"`
	StoreRoot   string `long:"store-root" help:"Filesystem process store root"`
	Verdict     string `long:"verdict" help:"Observed performer verdict"`
	EvidenceRef string `long:"evidence" optional:"true" help:"Evidence artifact/reference for the observation"`
	Actor       string `long:"actor" optional:"true" help:"Actor ref (default human:<current user>)"`
}

func observeCmd() *cobra.Command {
	return boa.CmdT[observeParams]{
		Use:         "observe",
		Short:       "Resolve an issued performer command",
		Long:        "Record a human-confirmed observation for a performer command whose external result could not be rediscovered after a daemon restart.",
		ParamEnrich: common.DefaultParamEnricher(),
		Args:        cobra.ExactArgs(2),
		PreExecuteFunc: func(p *observeParams, _ *cobra.Command, _ []string) error {
			if err := requireProcessesEnabled(); err != nil {
				return err
			}
			if strings.TrimSpace(p.StoreRoot) == "" {
				return fmt.Errorf("--store-root is required")
			}
			if strings.TrimSpace(p.Verdict) == "" {
				return fmt.Errorf("--verdict is required")
			}
			return nil
		},
		RunFunc: func(p *observeParams, cmd *cobra.Command, _ []string) {
			exitWithError(runObserve(cmd, p, os.Stdout))
		},
	}.ToCobra()
}

func runObserve(cmd *cobra.Command, p *observeParams, out io.Writer) error {
	fs, err := openStore(p.StoreRoot, true)
	if err != nil {
		return err
	}
	if err := ensureRunVerifies(cmd.Context(), fs, p.RunID, out); err != nil {
		return err
	}
	actor := state.ActorRef(strings.TrimSpace(p.Actor))
	if actor == "" {
		actor = defaultActor()
	}
	executor := processexec.New(fs, nil)
	observed, err := executor.RecordOutstandingObservation(cmd.Context(), p.RunID, p.CommandID, processexec.Observation{
		Actor:       actor,
		Verdict:     strings.TrimSpace(p.Verdict),
		EvidenceRef: strings.TrimSpace(p.EvidenceRef),
	})
	if err != nil {
		return err
	}
	if observed.Status == state.RunStatusPaused && observed.Pause != nil &&
		observed.Pause.Kind == state.PauseKindNeedsReconcile && observed.Pause.CommandID == p.CommandID {
		if err := resumeObservedRun(cmd, fs, p.RunID, p.CommandID, observed); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Observed process command %s for run %s\n", p.CommandID, p.RunID)
	return nil
}

func resumeObservedRun(cmd *cobra.Command, fs *store.FS, runID, commandID string, current *state.State) error {
	for attempt := 0; attempt < 8; attempt++ {
		if current.Status != state.RunStatusPaused || current.Pause == nil ||
			current.Pause.Kind != state.PauseKindNeedsReconcile || current.Pause.CommandID != commandID {
			return nil
		}
		at := processNow().UTC()
		_, err := fs.Append(cmd.Context(), runID, current.LastLogSeq, []evidence.LogEntry{
			runLogEntry(evidence.EntryKindStatus, state.Event{Type: state.EventRunResumed}, "", at),
		})
		if err == nil {
			return nil
		}
		if !store.IsConflict(err) {
			return err
		}
		current, err = fs.LoadRunState(cmd.Context(), runID)
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("process run %q remained busy while resuming observed command %q", runID, commandID)
}
