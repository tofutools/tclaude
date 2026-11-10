package session

import (
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// advanceHookConv updates the session's ConvID from hook input (tracks conversation changes). A
// /clear rotates the conv-id; needsIdentityMigration / migrateClearedIdentity
// handle moving the agent's identity across that rotation.
func advanceHookConv(state *SessionState, input HookCallbackInput, envSessionID string, amb HookAmbient) {
	if input.ConvID != "" && state.ConvID != input.ConvID {
		switch {
		case envSessionID == "" || state.ConvID == "" || amb.InTaskRunnerHook():
			// Not an env-keyed rotation we can migrate identity across
			// (a non-tclaude session, or the session's first conv-id
			// record). Plain advance — the pre-/clear-fix behaviour.
			//
			// `tclaude task run` lands here too: its rotation is a fresh
			// task starting, NOT an identity move. Forcing the plain
			// advance is load-bearing, not just an optimisation — the
			// reaper (agentd enrollOnlineSession) enrolls a task session's
			// CURRENT conv each tick, so without this exemption the next
			// task boundary would see the old task conv as "an active
			// agent", fire needsIdentityMigration, and inject a stray
			// `/rename` into the running task pane via migrateClearedIdentity.
			slog.Info("updating conversation ID",
				"old_conv_id", state.ConvID, "new_conv_id", input.ConvID,
				"session_id", state.ID, "module", "hooks")
			state.ConvID = input.ConvID
		default:
			shouldMigrate, predErr := needsIdentityMigration(state.ConvID, input.ConvID)
			switch {
			case predErr != nil:
				// A transient DB error trying to decide. Do NOT advance:
				// advancing on an "I don't know" answer would skip the
				// migration if the truth was "migrate," and identity
				// would strand. The next hook re-evaluates the predicate
				// (the rotation is still visible since we left ConvID
				// alone).
				slog.Warn("clear-migrate: predicate check failed; deferring conv-id advance to the next hook",
					"old_conv_id", state.ConvID, "new_conv_id", input.ConvID,
					"session_id", state.ID, "error", predErr, "module", "hooks")
			case shouldMigrate:
				// A /clear rotated the conv-id and the old conv is an
				// agent whose identity has not moved yet. Migrate it
				// BEFORE recording the new conv-id (the migration needs
				// the old value). On a migration failure DO NOT advance
				// state.ConvID: the migration is atomic so identity is
				// still wholly on the old conv-id — keeping the session
				// row there means needsIdentityMigration still fires on
				// the next hook and the (idempotent) migration is
				// retried, rather than the conv-id silently advancing
				// to a conv whose identity never arrived (issue #192).
				if migrateClearedIdentity(state, input.ConvID) {
					slog.Info("updating conversation ID after /clear",
						"old_conv_id", state.ConvID, "new_conv_id", input.ConvID,
						"session_id", state.ID, "module", "hooks")
					state.ConvID = input.ConvID
				} else {
					slog.Warn("clear-migrate: deferring conv-id advance until the migration succeeds",
						"old_conv_id", state.ConvID, "new_conv_id", input.ConvID,
						"session_id", state.ID, "module", "hooks")
				}
			default:
				// Predicate said no — the rotation does not need
				// identity migration (oldConv not an agent, newConv
				// already an agent, or the edge is already recorded).
				// Advance normally.
				slog.Info("updating conversation ID",
					"old_conv_id", state.ConvID, "new_conv_id", input.ConvID,
					"session_id", state.ID, "module", "hooks")
				state.ConvID = input.ConvID
			}
		}
	}
}

// refreshHookIdentity settles who this session is once its conv-id is
// final: a resumed retired conversation is reinstated, a launched session
// is enrolled as an agent, and the row is keyed by the harness's PID.
//
// Instant agent-enrollment for a tclaude-launched session. A
// SessionStart means the conversation just (re)booted, and by here
// state.ConvID is the settled conv-id (after any /clear or /resume
// identity migration in advanceHookConv). Enrolling it now means a
// terminal-launched session — `tclaude conv new`, a fresh
// `tclaude session` — surfaces on the dashboard the instant it
// starts, the same way a web-UI/CLI `tclaude agent spawn` does,
// instead of waiting up to one reaper interval for the daemon's
// online-enrollment sweep (which stays as the backstop, and also
// covers sessions tclaude did not launch — see agentd
// enrollOnlineSession).
//
// Gated on envSessionID (TCLAUDE_SESSION_ID): only sessions tclaude
// launched get the instant path, so a foreign headless one-shot
// (`claude -p`, a plugin probe) firing a SessionStart never lands an
// enrollment row the reaper would never have created. Subagent and
// foreign-process SessionStarts were dropped before this runs, so they
// cannot reach here.
//
// Restricted to a genuine fresh launch (source startup / none) via
// !isConvTransitionStart: a /clear, an interactive /resume switch, or
// a compaction also fires a SessionStart, but those are in-process
// conversation TRANSITIONS, not a process booting — and the
// conv advance and migration (advanceHookConv) already decided their
// enrollment correctly (an agent's identity, including its
// enrollment, migrates onto the new conv-id; a plain conversation's
// conv-id rotation just advances the session row and stays plain).
// Without this guard the post-/clear SessionStart(source=clear) would
// promote a never-an-agent plain conversation to an agent on the
// freshly rotated conv-id — the #407 regression
// (TestClearRotation_PlainConversationNotPromotedToAgent). The reaper
// sweep still enrolls any genuinely live transitioned conv as the
// backstop, exactly as it does for sessions tclaude did not launch.
//
// EnsureAgentForConv makes this idempotent and retirement-safe: a conv
// the rotation in advanceHookConv already linked to an actor, or one the human
// deliberately retired, is left untouched (a retired actor is never
// reinstated by an ensure).
//
// `tclaude task run` is exempt: its per-task conversations are
// throwaway task executions, not managed agents, so skip the INSTANT
// path here. (The reaper's online-reconcile sweep still enrolls a task
// session's current conv — making the roster fully task-free is a
// separate agentd concern; the task-runner exemption in advanceHookConv is what
// keeps the migration machinery from firing regardless.) The task
// runner needs only the session row + Stop-hook signal. See
// HookAmbient.InTaskRunnerHook.
//
// Codex has one extra repair path: if the launch/start hooks were missed,
// a later modeled hook from a non-pending spawned session still proves the
// conv is alive and should be enrolled. Pending dashboard spawns are skipped
// here because agentd's sweeper owns their group/name/briefing intent.
// A session that started with no conversation and then reports a resumed one
// is `tclaude -- --resume`: Claude Code's picker chose it, after the launch
// could have refused. See ReinstateRetiredForResume for the --resume path.
func refreshHookIdentity(state *SessionState, input HookCallbackInput, envSessionID string, amb HookAmbient, adoptingConv bool) {
	if adoptingConv && input.HookEventName == "SessionStart" && input.Source == "resume" && !amb.InTaskRunnerHook() {
		if retiredHead, err := isRetiredAgentHead(input.ConvID); err != nil {
			slog.Warn("failed to read agent state of resumed conversation",
				"conv_id", input.ConvID, "session_id", state.ID, "error", err, "module", "hooks")
		} else if retiredHead {
			if _, err := db.ReinstateAgent(input.ConvID); err != nil {
				slog.Warn("failed to reinstate resumed conversation",
					"conv_id", input.ConvID, "session_id", state.ID, "error", err, "module", "hooks")
			} else {
				slog.Info("reinstated retired conversation opened from the picker",
					"conv_id", input.ConvID, "session_id", state.ID, "module", "hooks")
			}
		}
	}
	if shouldEnrollLaunchedSessionFromHook(state, input, envSessionID, amb) {
		agentID, created, err := db.EnsureAgentForConv(state.ConvID, "session-start")
		if err != nil {
			slog.Warn("failed to register launched session as agent",
				"conv_id", state.ConvID, "session_id", state.ID, "error", err, "module", "hooks")
		} else {
			AssignFreeFloatingFallback(state, agentID, created)
		}
	}

	// Keep the row keyed by the real harness process, not tmux's shell
	// wrapper pane PID. Spawn records #{pane_pid}; hooks run under the
	// harness, so FindClaudePID can correct wrapper-shaped rows.
	if newPID := amb.HarnessPID(); newPID > 0 && state.PID != newPID {
		state.PID = newPID
	}
}
