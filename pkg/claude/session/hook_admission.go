package session

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// lockHookSession takes the per-session exclusive lock that keeps concurrent
// hook callbacks from racing on the read-modify-write of session state. The
// lock is keyed by the env session, or by the conv-id when there is none; a
// hook with neither has nothing to lock and gets a no-op unlock.
func lockHookSession(ctx context.Context, input HookCallbackInput, envSessionID string) (unlock func(), err error) {
	sessionKey := envSessionID
	if sessionKey == "" {
		sessionKey = input.ConvID
	}
	if sessionKey == "" {
		return func() {}, nil
	}
	unlock, lockErr := acquireHookLockContext(ctx, sessionKey)
	if lockErr != nil {
		slog.Warn("failed to acquire hook lock", "error", lockErr, "module", "hooks")
		return nil, fmt.Errorf("failed to acquire hook lock: %w", lockErr)
	}
	return unlock, nil
}

// loadHookSession returns the session row this hook updates, creating it when
// needed, or nil when the hook must be dropped: there is no row, the row is a
// shell's, or the hook comes from a foreign process (see admitHookConv).
func loadHookSession(input HookCallbackInput, envSessionID string, amb HookAmbient) (*SessionState, error) {
	slog.Debug("hook received",
		"event", input.HookEventName,
		"conv_id", input.ConvID,
		"notification_type", input.NotificationType,
		"tool_name", input.ToolName,
		"cwd", input.Cwd,
		"sessionId", envSessionID,
		"module", "hooks",
	)

	state, err := getOrCreateSessionState(input, envSessionID, amb)
	if err != nil || state == nil {
		return nil, err
	}
	slog.Debug("session found", "session_id", state.ID, "status", state.Status, "subagent_count", state.SubagentCount, "module", "hooks")

	// A shell row never has a ConvID, so the foreign-process guard below
	// (keyed off state.ConvID != "") can never engage for one. runNewShell
	// still exports TCLAUDE_SESSION_ID (goto/focus need it), so a headless
	// coding-harness run launched from inside the shell (`claude -p "hi"`,
	// an interactive `claude`, …) inherits it and its hooks land here,
	// against the shell's own row. Without this guard that hijacks the
	// row: the throwaway conv-id gets stamped onto it, its PID gets
	// rewritten via FindClaudePID, it gets enrolled as a dashboard agent,
	// and it flips to "exited" when the child exits — while the shell
	// itself is still alive. A shell row has no hooks of its own, ever,
	// so drop every hook unconditionally.
	if state.Harness == ShellHarnessName {
		return nil, nil
	}
	if !admitHookConv(state, input, envSessionID, amb) {
		return nil, nil
	}
	return state, nil
}

// admitHookConv is the foreign-process guard: it reports whether the hook
// may proceed, and false drops it. An env-keyed session's hooks normally all
// carry the conversation its row tracks. A hook carrying a DIFFERENT
// conv-id is one of two things:
//
//   - an in-process conversation transition (/clear, an interactive
//     /resume switch, compaction) — always announced by a
//     SessionStart whose `source` names the transition; or
//   - a FOREIGN process's event: a one-shot headless claude run
//     (`claude -p`, `claude mcp get`, …) launched from this
//     session's own Bash, inheriting TCLAUDE_SESSION_ID, firing
//     hooks for its own throwaway conv against OUR row.
//
// Foreign events must be dropped wholesale: processing one flips the
// live session's status (a notified "Exited" for a 2-second `claude
// mcp get`; an idle stamp from the child's Stop that can fire a
// context nudge mid-turn), and the conv-advance logic below would
// read the rotation as a /clear and migrate the agent's identity
// onto the throwaway conv — observed in production as a live agent
// retired "superseded by <conv> (clear)" where <conv> was a plugin
// probe's conv-id.
//
// A transition SessionStart records its new conv-id as pending_conv
// BEFORE the row advances, so the migration-failure retry keeps
// working: post-/clear hooks carry the announced conv-id and pass
// this guard via the pending_conv match, while a foreign conv-id
// was never announced and cannot match.
//
// One rotation is real but NEVER announced by an accepted source:
// Claude Code's /remote-control bridge handoff rotates the
// conversation to a fresh conv-id (full history copied into the new
// file) and fires only SessionStart(source=startup) — by source
// alone a foreign one-shot. For that case a mismatched hook gets one
// more chance before the drop: a bounded transcript head-scan that
// proves the new conv is an in-process continuation of the tracked
// one (isVerifiedConvContinuation). A verified continuation is
// recorded as pending_conv exactly like an announced transition, so
// the same conv-advance/identity-migration/retry machinery runs.
//
// PostCompact is exempt — it
// only resets per-env-session compact state and returns before any
// status or conv mutation, and it may legitimately arrive carrying
// a rotated conv-id.
//
// `tclaude task run` is exempt too (HookAmbient.InTaskRunnerHook): it drives a
// sequence of independent conversations under one env-session — one
// fresh conv per task — so its conv-id rotations are legitimate, not
// foreign. See HookAmbient.InTaskRunnerHook for the full rationale.
func admitHookConv(state *SessionState, input HookCallbackInput, envSessionID string, amb HookAmbient) bool {
	if !amb.InTaskRunnerHook() &&
		envSessionID != "" && state.ConvID != "" && input.ConvID != "" &&
		input.ConvID != state.ConvID &&
		input.HookEventName != "PostCompact" {
		if isConvTransitionStart(input) {
			// Announce the rotation. Persisted immediately (not via the
			// SaveSessionState at the end of this call) so a crash or
			// migration failure mid-call still leaves the announcement
			// for the retry on the next hook. If THIS write fails too,
			// the retry hooks get dropped as foreign and the rotation
			// only converges at the next transition SessionStart —
			// accepted: it takes two correlated SQLite faults in one
			// call to get there.
			if err := db.SetSessionPendingConv(state.ID, input.ConvID); err != nil {
				slog.Warn("failed to record pending conv", "error", err, "module", "hooks")
			}
		} else if pending, err := db.GetSessionPendingConv(state.ID); err != nil || pending != input.ConvID {
			if err != nil {
				slog.Warn("failed to read pending conv; dropping mismatched-conv hook", "error", err, "module", "hooks")
				return false
			}
			if isVerifiedConvContinuation(input, state) {
				// An unannounced in-process rotation (the /remote-control
				// bridge handoff) proven by transcript lineage. Record it
				// as pending_conv like an announced transition would —
				// later hooks then pass via the pending match without
				// re-reading the transcript, even if this call's
				// conv-advance/migration fails and has to retry.
				slog.Info("accepting unannounced conv rotation: transcript verified as in-process continuation",
					"event", input.HookEventName, "new_conv", input.ConvID,
					"tracked_conv", state.ConvID, "session_id", state.ID, "module", "hooks")
				if err := db.SetSessionPendingConv(state.ID, input.ConvID); err != nil {
					slog.Warn("failed to record pending conv", "error", err, "module", "hooks")
				}
			} else {
				slog.Info("ignoring hook from foreign claude process",
					"event", input.HookEventName, "foreign_conv", input.ConvID,
					"tracked_conv", state.ConvID, "session_id", state.ID, "module", "hooks")
				// Deliberately NOT stamping last_hook: a foreign process's
				// event is no evidence the host session itself is alive.
				return false
			}
		}
	}
	return true
}
