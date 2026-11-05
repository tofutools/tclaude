package session

// updateSubagentLedger applies this hook's evidence to the sub-agent ledger
// and reports whether a SubagentStop names a sub-agent the ledger still
// knew after its TTL sweep (or a legacy stop with no agent_id).
//
// ---- Sub-agent ledger (db.SubagentSet) ----
// The hook stream is LOSSY: Claude Code fires no hooks at all on a
// user interrupt (anthropics/claude-code#11189) and SubagentStop has
// no documented guarantee for aborts/errors/process death — so the
// ledger, not the event stream, is what the "🤖+N" badge trusts.
// Sweep expired entries first (self-healing for a lost SubagentStop),
// then apply this event's evidence: Start/Stop maintain the set, and
// any OTHER hook carrying agent_id proves that sub-agent is alive —
// Sight() re-adds one whose SubagentStart was lost.
func updateSubagentLedger(state *SessionState, input HookCallbackInput) (trackedSubagentStop bool) {
	state.Subagents.Sweep(state.LastHook)
	// Whether this SubagentStop names a sub-agent the ledger still knows
	// after the TTL sweep, captured before Remove erases that evidence.
	// Claude Code 2.1.220 ends
	// EVERY main-thread turn with a synthetic SubagentStop carrying a
	// freshly minted agent_id that no SubagentStart ever announced, an
	// empty agent_type, and an agent_transcript_path pointing at a file
	// that does not exist — fired right after the turn's real Stop. It
	// describes the main thread finishing, not a sub-agent.
	//
	// This is HARDENING, not a repair: at the ordering 2.1.220 actually
	// produces (always after Stop) the sub-agent lifecycle arm happens to
	// be a no-op or idempotent, so no misbehaviour was observed. What it
	// removes is the standing hazard of an event that is not a sub-agent
	// stopping being routed through the arm that decides what a sub-agent
	// stopping means to the MAIN thread's status — an ordering tclaude
	// does not control and upstream has already changed once.
	//
	// Ledger membership, not the empty agent_type, is the discriminator
	// for whether the stop may drive stopped-only side effects. A real
	// sub-agent can still arrive unknown when its ledger entry was swept
	// during a long model turn, or when its preceding SessionEnd removed
	// the entry. Such a stop may settle a now-empty main_agent_idle state
	// (applySubagentStopHook), but does not drive the context nudge/task signal because its
	// attribution is ambiguous. An empty agent_id is the legacy shape;
	// Remove falls back to decrement semantics for it, so it keeps its
	// old stopped behaviour.
	trackedSubagentStop = input.HookEventName == "SubagentStop" &&
		(input.AgentID == "" || state.Subagents.Has(input.AgentID))
	switch {
	case input.HookEventName == "SubagentStart":
		state.Subagents = state.Subagents.Add(input.AgentID, input.AgentType, state.LastHook)
	case input.HookEventName == "SubagentStop":
		state.Subagents.Remove(input.AgentID)
	case input.AgentID != "" && input.HookEventName == "SessionEnd":
		// A sub-agent's own conversation ending is as good as a
		// SubagentStop for the ledger (the main-thread status handling
		// of this event stays in applyHookEvent).
		state.Subagents.Remove(input.AgentID)
	case input.AgentID != "":
		state.Subagents = state.Subagents.Sight(input.AgentID, input.AgentType, state.LastHook)
	}
	return trackedSubagentStop
}

// updateBackgroundLedgers applies this hook's evidence to the background
// shell and monitor ledgers.
//
// ---- Background-shell ledger (db.BgShellSet) ----
// Strictly lossier than the sub-agent ledger (updateSubagentLedger): Claude Code
// announces a background shell's LAUNCH (this PostToolUse) and never
// its exit, so hooks alone can only ever grow the set. What actually
// retires an entry is the daemon's process-liveness reconcile on the
// dashboard read path; the two removals available here are the
// explicit ones — a TaskStop naming the task, and the known-zero
// boundaries handled in the SessionStart/SessionEnd arms (hook_events.go).
//
// Deliberately placed BEFORE the sub-agent gate (routeSubagentHook): a shell
// backgrounded by a SUB-agent is a child of the same harness process
// and keeps running past the parent's turn just the same, so it
// belongs in the session's ledger. The gate's early return persists
// full state, so the mutation is not lost.
//
// ---- Monitor ledger (db.MonitorSet) ----
//
// Same shape, same lossiness, and — because a `Monitor` watch is a
// background task in the SAME id namespace as a background shell — the
// same TaskStop event retires from either. A stop is routed to the one
// ledger that actually knows the id rather than offered to both, so a
// TaskStop naming a monitor cannot also decrement the shell count.
//
// A stop carrying NO id keeps its legacy meaning ("drop the oldest
// background shell") and is deliberately not extended to monitors: with
// two ledgers there is no non-arbitrary way to guess which one lost an
// entry, and dropping from both would double-count a single kill.
func updateBackgroundLedgers(state *SessionState, input HookCallbackInput) {
	trackShells := harnessTracksBackgroundShells(state.Harness)
	trackMonitors := harnessTracksMonitors(state.Harness)
	if trackShells {
		state.BgShells.Sweep(state.LastHook)
	}
	if trackMonitors {
		state.Monitors.Sweep(state.LastHook)
	}
	shellID, shellCommand, isShellLaunch := bgShellLaunch(input)
	monitor, isMonitorLaunch := monitorLaunch(input, state.LastHook)
	stopID, isStop := bgShellStop(input)
	switch {
	case trackShells && isShellLaunch:
		state.BgShells = state.BgShells.Add(shellID, shellCommand, state.LastHook)
	case trackMonitors && isMonitorLaunch:
		state.Monitors = state.Monitors.Add(
			monitor.ID, monitor.Command, monitor.Label, monitor.WS,
			state.LastHook, monitor.Deadline)
	case isStop && trackMonitors && state.Monitors.Has(stopID):
		state.Monitors.Remove(stopID)
	case isStop && trackShells:
		state.BgShells.Remove(stopID)
	}
}
