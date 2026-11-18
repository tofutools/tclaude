package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// An agent a run spawned for a step lives as long as the step's fallback
// lifetime says (model.PerformerFallback.Lifetime, tk_766694fe):
//
//   - "step": retired once its step is done for good, past every retry, and
//     in a loop once the loop has exited (engine.Definition.StepFinished);
//   - "run": kept for later steps (a sameAs names it), retired when the run
//     ends, however it ends;
//   - "keep": never retired by the run.
//
// Before lifetimes, a spawned performer stayed a member of its group forever.
// The retire is the ordinary cascade (as the AWB pickup cleanup's), taken only
// while the agent is settled and has no process work open anywhere, so an
// agent mid-turn, or one a later step of any run picked up, is left alone and
// looked at again on the next sweep. Its worktree and branch are kept, the
// Retire dialog's default.

// processSpawnedRetireActor is who the retire is recorded as.
const processSpawnedRetireActor = "system:process-runtime"

// processSpawnedReaperMu keeps two sweeps (the tick and a run's end) from
// retiring one agent twice.
var processSpawnedReaperMu sync.Mutex

// processSpawnedSettledFn is the settled check, swappable in tests.
var processSpawnedSettledFn = liveAWBReadyAgentSettled

var errProcessSpawnedBusy = errors.New("spawned performer is no longer settled")

// reapProcessSpawnedPerformers retires every live spawned performer whose
// lifetime is up. It reads one page of live spawned dispatches at a time.
func reapProcessSpawnedPerformers() {
	processSpawnedReaperMu.Lock()
	defer processSpawnedReaperMu.Unlock()
	runs := map[string]*processSpawnedRun{}
	done := map[string]bool{}
	afterRun, afterCommand := "", ""
	for {
		rows, err := db.ListLiveSpawnedProcessDispatches(afterRun, afterCommand, db.MaxProcessDispatchReadPage)
		if err != nil {
			slog.Warn("process performers: spawned performer scan failed", "error", err)
			return
		}
		for _, row := range rows {
			if done[row.AgentID] {
				continue
			}
			run := runs[row.RunID]
			if run == nil {
				run = loadProcessSpawnedRun(row.RunID)
				runs[row.RunID] = run
			}
			if run == nil {
				continue
			}
			lifetime, why := run.retireDue(row.NodeID)
			if lifetime == "" {
				continue
			}
			if retireProcessSpawnedPerformer(row, lifetime, why) {
				done[row.AgentID] = true
			}
		}
		if len(rows) < db.MaxProcessDispatchReadPage {
			return
		}
		last := rows[len(rows)-1]
		afterRun, afterCommand = last.RunID, last.CommandID
	}
}

// processSpawnedRun is what one sweep reads of a run, once.
type processSpawnedRun struct {
	definition *engine.Definition
	checkpoint engine.Checkpoint
}

func loadProcessSpawnedRun(runID string) *processSpawnedRun {
	record, err := db.GetProcessRun(runID)
	if err != nil || record == nil {
		return nil
	}
	definition, err := prepareProcessRunDefinition(record)
	if err != nil {
		return nil
	}
	var checkpoint engine.Checkpoint
	if err := record.DecodeCheckpoint(&checkpoint); err != nil {
		return nil
	}
	return &processSpawnedRun{definition: definition, checkpoint: checkpoint}
}

// retireDue is the lifetime of the agent spawned for nodeID and why it is
// due, or "" while it is not.
func (r *processSpawnedRun) retireDue(nodeID string) (string, string) {
	lifetime, ok := r.definition.SpawnLifetime(nodeID)
	if !ok || lifetime == model.LifetimeKeep {
		return "", ""
	}
	if r.checkpoint.Status != engine.RunRunning {
		return lifetime, fmt.Sprintf("its run %s", r.checkpoint.Status)
	}
	if lifetime == model.LifetimeStep && r.definition.StepFinished(r.checkpoint, nodeID) {
		return lifetime, "its step is done"
	}
	return "", ""
}

// retireProcessSpawnedPerformer retires one spawned performer, if it is
// settled and has no process work open. It reports whether the agent is gone.
func retireProcessSpawnedPerformer(row db.ProcessAgentDispatch, lifetime, why string) bool {
	agentID := row.AgentID
	a, err := db.GetAgent(agentID)
	if err != nil || a == nil || !a.Active() || a.CurrentConvID == "" {
		return a != nil && err == nil && !a.Active()
	}
	if open, err := db.ListProcessAgentDispatchesForAgent(agentID, true, 1); err != nil || len(open) > 0 {
		return false
	}
	guard := func() error {
		settled, err := processSpawnedSettledFn(agentID)
		if err != nil {
			return fmt.Errorf("confirm the spawned performer is settled: %w", err)
		}
		if !settled {
			return errProcessSpawnedBusy
		}
		return nil
	}
	if guard() != nil {
		return false
	}
	convID := a.CurrentConvID
	reason := fmt.Sprintf("process run %s spawned it for step %s (lifetime %s): %s", row.RunID, row.NodeID, lifetime, why)
	if _, _, err := retireAgentConvCascading(convID, processSpawnedRetireActor, reason, false, guard, false); err != nil {
		if !errors.Is(err, errProcessSpawnedBusy) {
			slog.Warn("process performers: could not retire a spawned performer",
				"run", row.RunID, "node", row.NodeID, "agent_id", agentID, "error", err)
		}
		return false
	}
	finishRetiredConv(convID, true /* shutdown */, false /* keep the worktree */, agentWorktreeView{}, "")
	slog.Info("process performers: retired a spawned performer",
		"run", row.RunID, "node", row.NodeID, "agent_id", agentID, "lifetime", lifetime, "why", why)
	return true
}
