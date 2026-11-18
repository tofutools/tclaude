package agentd

import "github.com/tofutools/tclaude/pkg/claude/process/executor"

// ProcessRunScopeForTest is the run scope a flow test binds a run to without
// going through scoped run creation, so a test can pick an owner or an
// orphaned state directly. TestProcessPerformer_PersistedGroupScope covers the
// persisted scope end to end.
type ProcessRunScopeForTest struct {
	Kind     string // "none", "group", or "agent"
	GroupID  int64
	AgentID  string
	Owner    string
	Orphaned bool
}

// SetProcessRunScopeForTest binds runID to scope for the dispatcher. Runs not
// named keep the production default.
func SetProcessRunScopeForTest(runID string, scope ProcessRunScopeForTest) func() {
	previous := processRunScopeOf
	processRunScopeOf = func(id string) (processRunScope, error) {
		if id != runID {
			return previous(id)
		}
		owner := scope.Owner
		if owner == "" {
			owner = "human:operator"
		}
		return processRunScope{Kind: scope.Kind, GroupID: scope.GroupID, AgentID: scope.AgentID,
			Owner: owner, Orphaned: scope.Orphaned}, nil
	}
	return func() { processRunScopeOf = previous }
}

// SweepProcessAgentDispatchesForTest runs one periodic recovery pass
// synchronously: the pass a daemon runs at startup and on every tick.
func SweepProcessAgentDispatchesForTest() {
	sweepProcessAgentDispatches()
	WaitForProcessDispatchForTest()
}

// WaitForProcessDispatchForTest drains background dispatch passes and the run
// claims they may start, until both are quiet.
func WaitForProcessDispatchForTest() {
	for range 4 {
		processDispatchWG.Wait()
		processRuns.wg.Wait()
	}
}

// ReapProcessSpawnedPerformersForTest waits for every run end the executor has
// told of (its hook also reaps), then runs the spawned-performer sweep once.
func ReapProcessSpawnedPerformersForTest() {
	executor.WaitTerminalHooks()
	reapProcessSpawnedPerformers()
	processSpawnedTeardowns.Wait()
}

// SetProcessSpawnedSettledForTest swaps the settled check the reaper uses.
func SetProcessSpawnedSettledForTest(t interface{ Cleanup(func()) }, settled func(agentID string) (bool, error)) {
	previous := processSpawnedSettledFn
	processSpawnedSettledFn = settled
	t.Cleanup(func() { processSpawnedSettledFn = previous })
}
