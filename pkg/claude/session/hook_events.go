package session

import (
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// hookOutcome is what a hook event arm tells applyHook to do next. An arm
// either lets the hook run on through the shared tail (conversation advance,
// enrollment, the save, notifications) or ends the hook early, and an early
// end says which write, if any, still has to happen.
type hookOutcome int

const (
	// hookContinue runs the shared tail of applyHook.
	hookContinue hookOutcome = iota
	// hookSaveAndReturn saves the full session state and ends the hook.
	hookSaveAndReturn
	// hookStampAndReturn persists only last_hook and ends the hook: the
	// event is not one tclaude models, so the status is left untouched.
	hookStampAndReturn
	// hookReturn ends the hook with no further write; the arm already did
	// whatever writing the event needs.
	hookReturn
)

// hookEvent is what applyHook knows when an event arm runs.
type hookEvent struct {
	state        *SessionState
	input        HookCallbackInput
	envSessionID string
	amb          HookAmbient
	// prevStatus is the status before this hook changed anything.
	prevStatus string
	// wasCompacting reports that this hook just cleared an attributed
	// "working: compacting" phase.
	wasCompacting bool
	// trackedSubagentStop reports a SubagentStop for a sub-agent the ledger
	// still knew after the TTL sweep (or a legacy stop with no agent_id).
	trackedSubagentStop bool
	// stopped is set by an arm whose event ends the main thread's turn. It
	// drives the context nudge and the task-runner signal.
	stopped bool
}

// finishHookEarly performs the write an early outcome asks for.
func finishHookEarly(state *SessionState, outcome hookOutcome) error {
	switch outcome {
	case hookSaveAndReturn:
		// subagent_count is a derived cache of the ledger, which every hook
		// has already swept, so an early save stores the swept count too.
		state.SubagentCount = len(state.Subagents)
		return SaveSessionState(state)
	case hookStampAndReturn:
		stampLastHook(state)
	}
	return nil
}

// stampLastHook persists only last_hook, for an event that changes nothing
// else. Best-effort: a failed write is logged, never fails the hook.
func stampLastHook(state *SessionState) {
	if err := db.UpdateSessionLastHook(state.ID, state.LastHook); err != nil {
		slog.Warn("failed to persist last_hook", "error", err, "module", "hooks")
	}
}

// routeSubagentHook keeps hooks fired from INSIDE a sub-agent (agent_id set)
// away from the main thread's status machine: before this gate, a background
// sub-agent's PreToolUse flipped an idle parent to "working" — and
// the SubagentStop idle-fallback below only fires from
// main_agent_idle, so the parent stayed wedged at "working" after
// the sub-agent finished. Exceptions that DO fall through to the
// status switch:
//   - SubagentStart and SubagentStop — their arms below handle the
//     main status transitions around sub-agent lifecycle. An unknown
//     SubagentStop gets this dedicated fall-through rather than the
//     default arm, so Claude Code's synthetic per-turn event cannot
//     clear awaiting_*; trackedSubagentStop controls whether it may
//     drive stopped-only side effects;
//   - PermissionRequest / Notification — a sub-agent's permission
//     prompt surfaces on the parent (Claude Code parks the prompt in
//     the parent's UI), so awaiting_permission must still be set.
func routeSubagentHook(ev *hookEvent) hookOutcome {
	state, input := ev.state, ev.input
	if input.AgentID == "" {
		return hookContinue
	}
	switch input.HookEventName {
	case "SubagentStop":
		// Fall through to the lifecycle settle below. An unknown id
		// may be CC's synthetic turn-end event or a real stop whose
		// ledger entry was already swept/removed; only the latter
		// needs the now-empty main_agent_idle state settled, while
		// neither should take the default awaiting_* arm.
		return hookContinue
	case "SubagentStart", "PermissionRequest", "Notification", "SessionStart", "SessionEnd":
		// fall through to the status switch below
		return hookContinue
	}
	// A sub-agent acting again while the parent shows awaiting_*
	// is exactly the evidence that the prompt (parked on the
	// parent) was answered — but the resolved state must be
	// main_agent_idle, NOT "working" via the tool arms: only
	// main_agent_idle is a state the SubagentStop settle below
	// can take back to idle, so painting "working" here wedged
	// the parent at "working: <tool>" forever once the sub-agent
	// finished (found by cold review — the gate's original
	// awaiting fall-through re-created the very wedge the gate
	// exists to fix). If the parent's own turn is genuinely in
	// flight (a foreground Task), its next main-thread hook
	// repaints "working"; both states style as busy either way.
	if state.Status == StatusAwaitingPermission || state.Status == StatusAwaitingInput {
		state.Status = StatusMainAgentIdle
		state.StatusDetail = state.backgroundActivityDetail()
	}
	return hookSaveAndReturn
}

// applyHookEvent updates the session state for one hook event. This switch
// is tclaude's cross-harness event→status map. Claude Code and Codex deliver
// the same snake_case payload field names through the same
// `tclaude session hook-callback` — the parse of a Codex hook payload
// into HookCallbackInput is JOH-157's contract — so both harnesses
// drive this switch unchanged. Codex fires only a SUBSET of these
// events (no Notification, SessionEnd, StopFailure or
// PostToolUseFailure), so JOH-159's two degradations are handled by
// what the subset DOES carry:
//   - needs-attention comes from PermissionRequest (Codex has no
//     Notification(permission_prompt)); both land on
//     StatusAwaitingPermission below.
//   - exit comes from the session reaper (tmux has-session → PID
//     liveness, RefreshSessionStatus) rather than a SessionEnd hook.
//
// A subset event tclaude doesn't model (e.g. PreCompact) falls through
// to the default arm: last_hook is stamped, status is left untouched.
func applyHookEvent(ev *hookEvent) hookOutcome {
	state, input := ev.state, ev.input
	switch input.HookEventName {
	case "UserPromptSubmit":
		state.Status = StatusWorking
		state.StatusDetail = "UserPromptSubmit"

	case "PreToolUse":
		// Tool is about to execute
		state.Status = StatusWorking
		state.StatusDetail = input.ToolName

	case "PostToolUse", "PostToolUseFailure":
		// Tool completed (success or failure) - back to working
		state.Status = StatusWorking
		state.StatusDetail = input.ToolName
		recordToolUseWorkdir(input)

	case "SubagentStart":
		// Ledger already updated above; no main-status transition.

	case "SubagentStop":
		applySubagentStopHook(ev)

	case "Stop":
		applyStopHook(ev)

	case "StopFailure":
		applyStopFailureHook(state, input)

	case "SessionStart":
		return applySessionStartHook(state, input)

	case "SessionEnd":
		return applySessionEndHook(ev)

	case "PermissionRequest":
		state.Status = StatusAwaitingPermission
		state.StatusDetail = input.ToolName
		if state.StatusDetail == "" {
			state.StatusDetail = "permission"
		}

	case "PostCompact":
		return applyPostCompactHook(ev)

	case "Notification":
		return applyNotificationHook(state, input)

	default:
		// Unknown hook event - log but don't update status
		return hookStampAndReturn
	}
	return hookContinue
}

// recordToolUseWorkdir tracks where the agent is building: a file-editing
// tool just ran, so the file's directory is the most-relevant "working
// dir" — distinct from input.Cwd, which is the launch dir. We
// also resolve that dir's git worktree root + branch here, so
// read surfaces report the agent's *current* branch (correct
// when it hops between sub-repos) rather than the launch dir's.
// Recorded per conv-id; the daemon's read paths use it back.
// Best-effort: a failed UpsertAgentWorkdir just leaves the
// previous value in place.
func recordToolUseWorkdir(input HookCallbackInput) {
	if dir, ok := WorkDirFromToolUse(input.ToolName, input.ToolInput, input.Cwd); ok {
		worktreeRoot, branch := GitLocationOf(dir)
		if err := db.UpsertAgentWorkdir(input.ConvID, dir, worktreeRoot, branch); err != nil {
			slog.Warn("failed to record agent workdir", "error", err, "module", "hooks")
		}
		// Append the branch to the conv's history. This catches a
		// branch in a worktree the launch-dir .jsonl never names —
		// Claude Code stamps only the launch repo's branch onto each
		// turn, so the .jsonl re-scan alone would miss it. An empty
		// branch (edit outside a git repo) is a silent no-op.
		if err := db.AppendConvBranchHistoryHook(input.ConvID, branch, worktreeRoot); err != nil {
			slog.Warn("failed to record branch history", "error", err, "module", "hooks")
		}
	}
}

// applySubagentStopHook settles the main thread once its last sub-agent
// stops.
func applySubagentStopHook(ev *hookEvent) {
	state := ev.state
	if len(state.Subagents) == 0 && state.Status == StatusMainAgentIdle {
		// Unknown-id stops are ambiguous: allow the status settle so
		// a delayed real stop cannot leave stale activity, but reserve
		// context nudges and task signals for tracked/legacy stops.
		ev.stopped = ev.trackedSubagentStop
		if state.hasBackgroundActivity() {
			state.StatusDetail = state.backgroundActivityDetail()
		} else {
			state.Status = StatusIdle
			state.StatusDetail = ""
		}
	}
}

// applyStopHook ends the main thread's turn.
//
// The turn is over, but a sub-agent or a background shell can
// outlive it — settling to plain idle then would report an agent
// as finished while work it launched is still running. Either
// ledger therefore holds the session at main_agent_idle; the
// dashboard badges the counts, and for background shells the
// daemon's liveness reconcile is what eventually settles it.
//
// `stopped` stays keyed on the SUB-AGENT ledger alone. It drives
// the context nudge, and a background shell is genuinely not the
// agent working: the main thread really has finished its turn and
// can act on a nudge. Gating it on background shells too would
// mute the nudge for as long as the agent keeps a dev server or a
// test watch running — potentially hours.
func applyStopHook(ev *hookEvent) {
	state := ev.state
	if len(state.Subagents) == 0 {
		ev.stopped = true
	}
	if state.hasBackgroundActivity() {
		state.Status = StatusMainAgentIdle
		state.StatusDetail = state.backgroundActivityDetail()
	} else {
		state.Status = StatusIdle
		state.StatusDetail = ""
	}
}

// applyStopFailureHook marks a turn that ended because of an
// API/auth/billing error rather than completing normally (CC fires
// StopFailure instead of Stop). The agent goes to "error" with error_type
// as the detail so the dashboard can surface it (e.g. "error: rate_limit").
//
// This status is TRANSIENT, not sticky: every other hook case
// here sets state.Status unconditionally, so the next normal
// event (UserPromptSubmit, a tool event, a later Stop) clears
// it back to working/idle. A retried agent leaves the error
// state on its own — nothing else has to reset it.
//
// Deliberately NOT setting stopped=true (unlike the Stop case):
// the stopped branch drives the context nudge and the task-runner
// signal — both of which would "act on" the error (typing a nudge
// into a broken pane, or reporting a half-finished task as done).
// Acting on an error is explicitly out of scope here. The status
// transition and the desktop notification (notify.OnStateTransition
// in applyHook) both fire regardless of the stopped flag.
func applyStopFailureHook(state *SessionState, input HookCallbackInput) {
	state.Status = StatusError
	state.StatusDetail = input.ErrorType
	if state.StatusDetail == "" {
		state.StatusDetail = "unknown"
	}
	slog.Warn("agent turn ended in error",
		"conv_id", input.ConvID,
		"error_type", input.ErrorType,
		"error_message", input.ErrorMessage,
		"module", "hooks",
	)
}

// applySessionStartHook handles a session starting or resuming.
func applySessionStartHook(state *SessionState, input HookCallbackInput) hookOutcome {
	// A SessionStart carrying agent_id fired from inside a subagent
	// (subagents share the main session's conv-id, so the foreign-
	// process guard above can't catch them; agent_id is the
	// documented discriminator). It is not the main conversation
	// (re)starting — flipping a working session to idle here, or
	// clearing a recorded exit reason, would misreport the main
	// thread's state. It IS live evidence of the sub-agent though
	// (the ledger block Sighted it above), so persist the full state,
	// not just last_hook.
	if input.AgentID != "" {
		return hookSaveAndReturn
	}
	// Session started or resumed - update ConvID and normally set to
	// idle. A post-compaction SessionStart is different: it is an
	// in-process continuation boundary, not evidence that the turn
	// stopped. PostCompact already selected working (auto/unknown) or
	// idle (manual), so preserve that status until the next operational
	// hook determines a new one.
	// A (re)starting main thread definitionally has NO sub-agents
	// running yet — this is a known-zero boundary for the ledger, and
	// the reset is what clears phantoms left by lost SubagentStops
	// (e.g. sub-agents that died with a previous process). A /clear
	// or /resume transition lands here too; a background sub-agent
	// that somehow survives one re-adds itself via Sight() on its
	// next hook. The exception is a harness that fires this very event
	// for a subagent's own compaction, with no agent_id: there it says
	// nothing about the main thread, and the re-injection needs to know
	// a subagent is running.
	if input.Source != "compact" || !subagentsShareCompactionHooks(state.Harness) {
		state.Subagents = nil
	}
	// Background shells are children of the harness PROCESS, and
	// monitors belong to it, which a /clear or /resume does not
	// restart — so unlike sub-agents both can genuinely outlive this
	// boundary, and only a startup SessionStart (a new process) is a
	// true known-zero for them. Clearing on every SessionStart would
	// blank the ledgers on every /clear while the work kept running;
	// the liveness reconcile retires them honestly instead.
	if input.Source == "startup" || input.Source == "" {
		state.BgShells = nil
		state.Monitors = nil
	}
	if input.Source != "compact" && !lateSessionStart(state) {
		state.Status = StatusIdle
		state.StatusDetail = ""
	}
	// The conversation is alive again — drop any exit_reason a
	// previous exit (or the reaper) recorded. Cleared conv-wide, not
	// just for this row: a conv can own several session rows and the
	// dashboard reads exit_reason off whichever is most recent, so a
	// stale reason left on a sibling row could later be misread as a
	// crash.
	if state.ConvID != "" {
		if err := db.ClearSessionExitReasonByConv(state.ConvID); err != nil {
			slog.Warn("failed to clear exit reason", "error", err, "module", "hooks")
		}
		if err := db.ClearSessionExitIntentByConv(state.ConvID); err != nil {
			slog.Warn("exit audit: clear stale lifecycle intent failed",
				"session", state.ID, "error", err, "module", "hooks")
		}
	}
	return hookContinue
}

// applySessionEndHook handles Claude Code shutting down this conversation.
// The `reason` field tells a real process exit apart from a /clear or an
// interactive /resume switch, both of which end the conversation
// but keep the process alive and fire a fresh SessionStart
// immediately after — so neither must mark the session exited.
// logout / prompt_input_exit / other all mean the process is
// going away.
func applySessionEndHook(ev *hookEvent) hookOutcome {
	state, input, amb := ev.state, ev.input, ev.amb
	// A SessionEnd carrying agent_id was fired from inside a
	// subagent (the docs call agent_id THE discriminator for
	// "subagent hook call vs main-thread call") — whatever ended
	// there, it was not the main process, so it must not flip this
	// session to exited or fire an "Exited" notification. It does
	// mean that sub-agent is gone (the ledger block Removed it
	// above), so persist the full state, not just last_hook.
	if input.AgentID != "" {
		return hookSaveAndReturn
	}
	// The reason itself can say this is not an exit: a /clear or an
	// interactive /resume ends the conversation but not the process.
	if !sessionEndIsExit(input.Reason) {
		return hookStampAndReturn
	}
	// So can the HARNESS. A harness whose SessionEnd is best-effort has not
	// proven the event means the process is going away, and acting as if it
	// had would declare a live pane dead — settling its ledgers, writing an
	// exit observation and raising an "Exited" notification on an agent
	// sitting at a prompt. Exit detection for those harnesses stays where
	// it is provable: the reaper's tmux/PID liveness.
	//
	// The session is still OVER as far as the harness will report, so the
	// status settles to idle. That matters for a turn that ended without a
	// turn-end event at all: a Copilot provider error emits SessionStart →
	// ErrorOccurred → SessionEnd and no Stop, and ErrorOccurred is not in
	// the installed baseline, so without this the agent would sit at
	// "working" until its pane died. Status only — no exit observation, no
	// notification, no ledger reset, since none of those are warranted by
	// an event that has not proven a process ended.
	if !sessionEndProvesExit(state.Harness) {
		if state.Status == StatusWorking {
			state.Status = StatusIdle
			state.StatusDetail = ""
		}
		return hookSaveAndReturn
	}
	// The process is going away — sub-agents and monitors run inside
	// it and background shells are its children, so none of the three
	// can survive. Known-zero boundary, same as the reaper's
	// MarkSessionExitedIfUnchanged.
	state.Subagents = nil
	state.BgShells = nil
	state.Monitors = nil
	state.Status = StatusExited
	state.StatusDetail = ""
	accepted, _, err := db.RecordSessionEndExitObservation(db.AgentExitObservation{
		At:                 time.Now(),
		SessionID:          state.ID,
		TmuxSession:        state.TmuxSession,
		Observer:           db.AgentExitObserverHook,
		CauseKind:          db.AgentExitCauseNormal,
		Reason:             boundedSessionEndReason(input.Reason),
		ObservedState:      StatusExited,
		ExpectedGeneration: amb.ExitGeneration,
	})
	if err != nil {
		slog.Warn("exit audit: persist SessionEnd observation failed",
			"session", state.ID, "observer", db.AgentExitObserverHook,
			"error", err, "module", "hooks")
		return hookReturn
	}
	if !accepted {
		slog.Info("ignoring stale SessionEnd from predecessor launch",
			"session", state.ID, "module", "hooks")
		return hookReturn
	}
	if !amb.InTaskRunnerHook() {
		convTitle := getConvTitle(state.ConvID, state.Cwd)
		notifyOnStateTransition(state.ID, state.ConvID, ev.prevStatus, state.Status,
			state.Cwd, convTitle, state.Harness)
	}
	return hookReturn
}

// applyPostCompactHook handles a finished compaction.
func applyPostCompactHook(ev *hookEvent) hookOutcome {
	state, input, envSessionID, amb := ev.state, ev.input, ev.envSessionID, ev.amb
	// A compaction just happened — zero the pre-compaction context_pct
	// (the statusline hook will report the new, smaller figure) and the
	// nudged_pct ladder so the context nudge re-arms from scratch on the
	// next climb. A held compaction may be a subagent's, so the main
	// session keeps its figures until the hold settles.
	if envSessionID != "" && (amb.InTaskRunnerHook() || !compactionHeld(input, envSessionID)) {
		if err := db.ResetCompact(envSessionID); err != nil {
			slog.Warn("failed to reset compact state", "error", err, "module", "hooks")
		} else {
			slog.Debug("post-compact state reset", "session_id", envSessionID, "module", "hooks")
		}
	}
	// Auto-compaction happens inside an active turn, so the eager
	// compacting-detail clear above intentionally leaves it working.
	// A manual /compact returns the human to an idle prompt instead.
	// SessionStart(source=compact) fires just before this hook on Claude
	// Code 2.1.314, and preserves the status either way round.
	// PostCompact is exempt from the mismatched-conversation guard
	// because a legitimate compaction can rotate the conv-id before its
	// SessionStart(compact) announces that rotation. Do not let that
	// exemption turn a foreign child's manual PostCompact into an idle
	// stamp on the host: require either the ordinary attribution check,
	// or the attributed compacting phase this hook just cleared.
	attributed := ev.wasCompacting || hookBelongsToTrackedMainConversation(state, input)
	if input.Trigger == "manual" && attributed {
		state.Status = StatusIdle
		state.StatusDetail = ""
		return hookSaveAndReturn
	}
	return hookStampAndReturn
}

// applyNotificationHook maps a harness notification onto the session status.
func applyNotificationHook(state *SessionState, input HookCallbackInput) hookOutcome {
	// Check notification type for legacy support
	switch input.NotificationType {
	case "permission_prompt":
		state.Status = StatusAwaitingPermission
		state.StatusDetail = input.Message
	case "elicitation_dialog":
		state.Status = StatusAwaitingInput
		state.StatusDetail = input.Message
	case "idle_prompt":
		// CC has been idle and is waiting for user input. This is
		// our only signal back to idle after the user cancels an
		// in-flight turn with Escape: Stop does NOT fire on
		// interrupt (anthropics/claude-code#11189, closed as
		// not-planned), so without this case the agent stays stuck
		// at e.g. "working: UserPromptSubmit". CC's idle detection
		// runs on its own ~60s timer, so recovery is delayed, not
		// instant. Deliberately NOT setting stopped=true — that
		// branch context-nudges into the pane, which would collide
		// with a user mid-typing.
		//
		// The main thread can be idle while work it launched is not.
		// Keep the same main_agent_idle contract as Stop whenever a
		// sub-agent or background shell is still in the ledger. Besides
		// keeping the dashboard honest, this prevents the generic
		// "* → idle" notification rule from announcing completion while
		// background work is active. The daemon's liveness reconcile
		// eventually settles lost SubagentStop events and shell exits.
		if state.hasBackgroundActivity() {
			state.Status = StatusMainAgentIdle
			state.StatusDetail = state.backgroundActivityDetail()
		} else {
			state.Status = StatusIdle
			state.StatusDetail = ""
		}
	default:
		// Unknown notification type - log but don't update status.
		// One from inside a sub-agent still Sighted the ledger above,
		// so persist the full state for it; a last_hook-only write
		// would silently drop that mutation.
		if input.AgentID != "" {
			return hookSaveAndReturn
		}
		return hookStampAndReturn
	}
	return hookContinue
}
