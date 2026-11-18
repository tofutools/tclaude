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

// processSpawnLifetimeOf resolves the lifetime of the agent command spawns:
// as authored, else the default, which a gate's vote takes from the
// definition (engine.Definition.SpawnLifetime).
func processSpawnLifetimeOf(runID string, command engine.Command) string {
	if command.Agent != nil && command.Agent.FallbackLifetime != "" {
		return command.Agent.FallbackLifetime
	}
	if record, err := db.GetProcessRun(runID); err == nil && record != nil {
		if definition, err := prepareProcessRunDefinition(record); err == nil {
			if lifetime, ok := definition.SpawnLifetime(command.NodeID); ok {
				return lifetime
			}
		}
	}
	return model.DefaultLifetime()
}

// reapProcessSpawnedPerformers retires every live spawned performer whose
// lifetime is up. It reads one page of live spawned dispatches at a time.
func reapProcessSpawnedPerformers() {
	processSpawnedReaperMu.Lock()
	defer processSpawnedReaperMu.Unlock()
	runs := processSpawnedRuns{}
	seen := map[string]bool{}
	var retired []string
	afterRun, afterCommand := "", ""
	for {
		rows, err := db.ListLiveSpawnedProcessDispatches(afterRun, afterCommand, db.MaxProcessDispatchReadPage)
		if err != nil {
			slog.Warn("process performers: spawned performer scan failed", "error", err)
			break
		}
		for _, row := range rows {
			if seen[row.AgentID] {
				continue
			}
			run := runs.get(row.RunID)
			if run == nil {
				continue
			}
			why := run.retireDue(row.NodeID, row.SpawnLifetime)
			if why == "" {
				continue
			}
			seen[row.AgentID] = true
			if convID := retireProcessSpawnedPerformer(row, why, runs); convID != "" {
				retired = append(retired, convID)
			}
		}
		if len(rows) < db.MaxProcessDispatchReadPage {
			break
		}
		last := rows[len(rows)-1]
		afterRun, afterCommand = last.RunID, last.CommandID
	}
	// A pane's soft exit can take seconds: off the runtime tick, and off the
	// reaper's lock.
	for _, convID := range retired {
		go finishRetiredConv(convID, true /* shutdown */, false /* keep the worktree */, agentWorktreeView{}, "")
	}
}

// processSpawnedRun is what one sweep reads of a run, once.
type processSpawnedRun struct {
	definition *engine.Definition
	checkpoint engine.Checkpoint
}

// processSpawnedRuns caches each run one sweep reads; nil for a run that
// cannot be read, which is then left alone.
type processSpawnedRuns map[string]*processSpawnedRun

func (runs processSpawnedRuns) get(runID string) *processSpawnedRun {
	if run, ok := runs[runID]; ok {
		return run
	}
	runs[runID] = loadProcessSpawnedRun(runID)
	return runs[runID]
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

// retireDue says why the agent spawned for nodeID with lifetime is due to
// be retired, or "" while it is not.
func (r *processSpawnedRun) retireDue(nodeID, lifetime string) string {
	switch {
	case r.checkpoint.Status != engine.RunRunning:
		return fmt.Sprintf("its run %s", r.checkpoint.Status)
	case lifetime == model.LifetimeStep && r.definition.StepFinished(r.checkpoint, nodeID):
		return "its step is done"
	}
	return ""
}

// processSpawnedStillWanted says why a spawned agent must stay although its
// own lifetime is up: some step it performed, in a run still running, is not
// done for good. A step of another run that picked it (by role, say), or a
// later step of this one (sameAs, a loop round, a retry in its own session),
// may still go back to it. "" when nothing does.
func processSpawnedStillWanted(agentID string, runs processSpawnedRuns) string {
	dispatches, err := db.ListProcessAgentDispatchesForAgent(agentID, false, db.MaxProcessDispatchReadPage)
	if err != nil {
		return "its process steps could not be read"
	}
	for _, dispatch := range dispatches {
		if dispatch.Open() && dispatch.State == db.ProcessDispatchDelivered {
			return fmt.Sprintf("it is on step %s of run %s", dispatch.NodeID, dispatch.RunID)
		}
		run := runs.get(dispatch.RunID)
		if run == nil || run.checkpoint.Status != engine.RunRunning {
			continue
		}
		if !run.definition.StepFinished(run.checkpoint, dispatch.NodeID) {
			return fmt.Sprintf("step %s of run %s may go back to it", dispatch.NodeID, dispatch.RunID)
		}
	}
	return ""
}

// retireProcessSpawnedPerformer retires one spawned performer if it is
// settled and no step still wants it, and returns its conversation, or ""
// when it was left alone.
func retireProcessSpawnedPerformer(row db.ProcessAgentDispatch, why string, runs processSpawnedRuns) string {
	agentID := row.AgentID
	a, err := db.GetAgent(agentID)
	if err != nil || a == nil || !a.Active() || a.CurrentConvID == "" {
		return ""
	}
	if wanted := processSpawnedStillWanted(agentID, runs); wanted != "" {
		return ""
	}
	// The guard runs right before the retire commits, under the agent's launch
	// lock, and checks again that the agent has no step open and has settled.
	// It does not take processDispatchMu: a delivery may wake the agent under
	// that same launch lock while holding it, which would deadlock. A step
	// handed to the agent in the moment between the check and the commit finds
	// a retired performer, and the dispatcher resolves another
	// (resolveLostProcessPerformer).
	guard := func() error {
		if open, err := db.ListProcessAgentDispatchesForAgent(agentID, true, 1); err != nil || len(open) > 0 {
			return errProcessSpawnedBusy
		}
		settled, err := processSpawnedSettledFn(agentID)
		if err != nil {
			return fmt.Errorf("confirm the spawned performer is settled: %w", err)
		}
		if !settled {
			return errProcessSpawnedBusy
		}
		return nil
	}
	convID := a.CurrentConvID
	reason := fmt.Sprintf("process run %s spawned it for step %s (lifetime %s): %s", row.RunID, row.NodeID, row.SpawnLifetime, why)
	if _, _, err := retireAgentConvCascading(convID, processSpawnedRetireActor, reason, false, guard, false); err != nil {
		if !errors.Is(err, errProcessSpawnedBusy) {
			slog.Warn("process performers: could not retire a spawned performer",
				"run", row.RunID, "node", row.NodeID, "agent_id", agentID, "error", err)
		}
		return ""
	}
	slog.Info("process performers: retired a spawned performer",
		"run", row.RunID, "node", row.NodeID, "agent_id", agentID, "lifetime", row.SpawnLifetime, "why", why)
	return convID
}
