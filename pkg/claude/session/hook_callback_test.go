package session

import (
	"encoding/json"
	"maps"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestIsValidRenameTitle pins the session-side mirror of agentd's
// rename-title gate. The /clear title-restore injection in
// restoreClearedTitle types `/rename <carried-name>` into a tmux pane
// via send-keys; the carried name comes from
// conv_index.custom_title (verbatim from the .jsonl) or
// agent_enrollment.pending_name (stored even when invalid), neither
// pre-checked by the strict gate. This unit test locks down the
// charset rules for the cases that matter at this seam — newlines,
// slashes, control chars, length cap, double-spaces, unicode — so a
// future relaxation can't reopen the send-keys injection sink. The
// agentd-side TestIsValidRenameTitle is the authoritative spec; this
// list must stay aligned.
func TestIsValidRenameTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// --- accepted ---
		{"plain alphanumeric", "abc123", true},
		{"hyphen", "code-reviewer", true},
		{"underscore", "code_reviewer", true},
		{"single space", "code reviewer", true},
		{"brackets", "[reviewer]", true},
		{"braces", "{reviewer}", true},
		{"parens", "(reviewer)", true},
		{"max length 64", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456789AB", true},

		// --- rejected: empty / oversize ---
		{"empty", "", false},
		{"too long 65", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456789ABCD", false},

		// --- rejected: keystroke-injection vectors (the load-bearing cases) ---
		{"newline", "code\nreviewer", false},
		{"carriage return", "code\rreviewer", false},
		{"tab", "code\treviewer", false},
		{"NUL", "code\x00reviewer", false},
		{"DEL", "code\x7freviewer", false},
		{"slash command", "foo /bash", false},
		{"double quote", "code\"reviewer", false},
		{"single quote", "code'reviewer", false},
		{"backtick", "code`reviewer", false},
		{"semicolon", "code;reviewer", false},
		{"pipe", "code|reviewer", false},
		{"dollar", "code$reviewer", false},
		{"backslash", "code\\reviewer", false},
		{"angle brackets", "code<reviewer>", false},

		// --- rejected: whitespace abuse ---
		{"double space", "code  reviewer", false},
		{"NBSP", "code reviewer", false},

		// --- rejected: unicode / non-ASCII ---
		{"emoji", "reviewer\U0001f600", false},
		{"latin extended", "café", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isValidRenameTitle(c.in), "isValidRenameTitle(%q)", c.in)
		})
	}
}

// sessionEndIsExit decides whether a SessionEnd hook means the process
// is going away. Exactly "clear" (the /clear command) and "resume" (an
// interactive /resume switching conversations) keep the process alive
// and are non-exits; everything else — including an empty reason —
// counts as an exit.
func TestSessionEndIsExit(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{"", true},
		{"clear", false},
		{"resume", false},
		{"logout", true},
		{"prompt_input_exit", true},
		{"bypass_permissions_disabled", true},
		{"other", true},
		{"Clear", true},  // case-sensitive: only exact "clear" is the no-op
		{"Resume", true}, // same for "resume"
	}
	for _, c := range cases {
		assert.Equal(t, c.want, sessionEndIsExit(c.reason), "reason=%q", c.reason)
	}
}

// feedHook runs runHookCallback with the given JSON payload on stdin
// and TCLAUDE_SESSION_ID set to sessionID, restoring os.Stdin after.
func feedHook(t *testing.T, sessionID string, payload map[string]any) {
	t.Helper()
	data, err := json.Marshal(payload)
	require.NoError(t, err)

	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, _ = w.Write(data) // small payload fits the pipe buffer
	require.NoError(t, w.Close())

	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })

	t.Setenv("TCLAUDE_SESSION_ID", sessionID)
	require.NoError(t, runHookCallback())
}

// A SessionEnd hook with a real exit reason flips the session row to
// "exited" — the clean-exit half of the offline-status fix (the reaper
// covers the unclean half).
func TestRunHookCallback_SessionEndMarksExited(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:     "reap-sess",
		ConvID: "conv-reap",
		Status: StatusIdle,
	}))

	feedHook(t, "reap-sess", map[string]any{
		"session_id":      "conv-reap",
		"hook_event_name": "SessionEnd",
		"reason":          "logout",
		"cwd":             dir,
	})

	got, err := LoadSessionState("reap-sess")
	require.NoError(t, err)
	assert.Equal(t, StatusExited, got.Status,
		"SessionEnd(logout) must mark the session exited")
}

// A SessionEnd hook fired by /clear must NOT mark the session exited —
// the process survives a /clear and a fresh SessionStart follows.
func TestRunHookCallback_SessionEndClearKeepsStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:     "clear-sess",
		ConvID: "conv-clear",
		Status: StatusIdle,
	}))

	feedHook(t, "clear-sess", map[string]any{
		"session_id":      "conv-clear",
		"hook_event_name": "SessionEnd",
		"reason":          "clear",
		"cwd":             dir,
	})

	got, err := LoadSessionState("clear-sess")
	require.NoError(t, err)
	assert.Equal(t, StatusIdle, got.Status,
		"SessionEnd(clear) keeps the process alive — status must not flip to exited")
	reason, err := db.GetSessionExitReason("clear-sess")
	require.NoError(t, err)
	assert.Equal(t, "", reason,
		"a /clear is not a real exit — it must not record an exit reason")
}

// A SessionEnd hook fired by an interactive /resume switching to
// another conversation must NOT mark the session exited — the process
// survives the switch and a SessionStart(source=resume) follows.
// Claude Code 2.1.79 started firing SessionEnd for this; before the
// reason was added to sessionEndIsExit's allow-list, every /resume
// produced a spurious "Exited" desktop notification.
func TestRunHookCallback_SessionEndResumeKeepsStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:     "resume-sess",
		ConvID: "conv-old",
		Status: StatusIdle,
	}))

	feedHook(t, "resume-sess", map[string]any{
		"session_id":      "conv-old",
		"hook_event_name": "SessionEnd",
		"reason":          "resume",
		"cwd":             dir,
	})

	got, err := LoadSessionState("resume-sess")
	require.NoError(t, err)
	assert.Equal(t, StatusIdle, got.Status,
		"SessionEnd(resume) keeps the process alive — status must not flip to exited")
	reason, err := db.GetSessionExitReason("resume-sess")
	require.NoError(t, err)
	assert.Equal(t, "", reason,
		"a /resume switch is not a real exit — it must not record an exit reason")
}

// A SessionEnd hook carrying agent_id was fired from inside a subagent
// (per the hooks docs, agent_id is "present only when the hook fires
// inside a subagent call"). Whatever ended there, the main process is
// still running — the session must not flip to exited.
func TestRunHookCallback_SessionEndFromSubagentIgnored(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:     "sub-sess",
		ConvID: "conv-sub",
		Status: StatusWorking,
	}))

	feedHook(t, "sub-sess", map[string]any{
		"session_id":      "conv-sub",
		"hook_event_name": "SessionEnd",
		"reason":          "other",
		"agent_id":        "agent-abc123",
		"agent_type":      "Explore",
		"cwd":             dir,
	})

	got, err := LoadSessionState("sub-sess")
	require.NoError(t, err)
	assert.Equal(t, StatusWorking, got.Status,
		"a subagent-context SessionEnd must not mark the main session exited")
	reason, err := db.GetSessionExitReason("sub-sess")
	require.NoError(t, err)
	assert.Equal(t, "", reason,
		"a subagent-context SessionEnd must not record an exit reason")
}

// A SessionEnd for a conversation tclaude has never tracked must not
// auto-register a session row. One-shot headless claude invocations
// (`claude -p`, `claude mcp get`, …) fire SessionEnd(other) with a
// fresh conv-id per run; registering them created instant exited rows
// and fired an "Exited" notification per run — the agentd plugin
// checker's per-minute probes notified the user every 60 seconds.
func TestRunHookCallback_SessionEndUntrackedConvNotRegistered(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	feedHook(t, "", map[string]any{
		"session_id":      "deadbeef-0355-4e23-9283-4af96443a58f",
		"hook_event_name": "SessionEnd",
		"reason":          "other",
		"cwd":             dir,
	})

	state, err := FindSessionByConvID("deadbeef-0355-4e23-9283-4af96443a58f")
	require.NoError(t, err)
	assert.Nil(t, state,
		"a never-tracked conv's SessionEnd must not auto-register a session row")
	exists, err := SessionExists("deadbeef")
	require.NoError(t, err)
	assert.False(t, exists, "no row may be created under the truncated conv-id either")
}

// Hooks from a FOREIGN claude process — a one-shot headless run
// (`claude -p`, `claude mcp get`, …) launched from the session's own
// Bash, inheriting TCLAUDE_SESSION_ID but carrying its own throwaway
// conv-id — must be dropped wholesale: no status flip, no exit reason,
// no conv-id advance (the conv-advance path is what migrated agent
// identity onto plugin-probe convs in production).
func TestRunHookCallback_ForeignProcessHooksIgnored(t *testing.T) {
	for _, event := range []struct {
		name    string
		payload map[string]any
	}{
		{"SessionEnd", map[string]any{"hook_event_name": "SessionEnd", "reason": "other"}},
		{"Stop", map[string]any{"hook_event_name": "Stop"}},
		{"UserPromptSubmit", map[string]any{"hook_event_name": "UserPromptSubmit"}},
		{"SessionStart(startup)", map[string]any{"hook_event_name": "SessionStart", "source": "startup"}},
	} {
		t.Run(event.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			db.ResetForTest()

			require.NoError(t, SaveSessionState(&SessionState{
				ID:     "host-sess",
				ConvID: "conv-host",
				Status: StatusWorking,
			}))

			payload := map[string]any{"session_id": "conv-foreign", "cwd": dir}
			maps.Copy(payload, event.payload)
			feedHook(t, "host-sess", payload)

			got, err := LoadSessionState("host-sess")
			require.NoError(t, err)
			assert.Equal(t, StatusWorking, got.Status,
				"a foreign process's %s must not change the host session's status", event.name)
			assert.Equal(t, "conv-host", got.ConvID,
				"a foreign process's %s must not advance the host session's conv-id", event.name)
			reason, err := db.GetSessionExitReason("host-sess")
			require.NoError(t, err)
			assert.Equal(t, "", reason, "no exit reason from a foreign process's %s", event.name)
		})
	}
}

// The announced-transition path: a SessionStart whose source names an
// in-process conversation transition (clear/resume/compact) IS allowed
// to carry a new conv-id — it records the announcement and the session
// row advances. Later hooks carrying the announced conv-id pass the
// guard too (the migration-failure retry depends on that), while a
// conv-id that was never announced stays blocked.
func TestRunHookCallback_AnnouncedTransitionAdvancesConv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:     "trans-sess",
		ConvID: "conv-before",
		Status: StatusIdle,
	}))

	feedHook(t, "trans-sess", map[string]any{
		"session_id":      "conv-after",
		"hook_event_name": "SessionStart",
		"source":          "clear",
		"cwd":             dir,
	})

	got, err := LoadSessionState("trans-sess")
	require.NoError(t, err)
	assert.Equal(t, "conv-after", got.ConvID,
		"SessionStart(source=clear) must advance the tracked conv-id")

	pending, err := db.GetSessionPendingConv("trans-sess")
	require.NoError(t, err)
	assert.Equal(t, "conv-after", pending,
		"the transition must be recorded as pending_conv for the retry path")
}

// The migration-retry seam: when the session row could NOT advance at
// the transition SessionStart (e.g. a transient migration failure), a
// later ordinary hook carrying the ANNOUNCED conv-id must still be
// processed — pending_conv is what tells it apart from a foreign
// process's conv-id.
func TestRunHookCallback_PendingConvHookStillProcessed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:     "retry-sess",
		ConvID: "conv-old",
		Status: StatusIdle,
	}))
	// The transition was announced, but the row never advanced —
	// modeling a failed migration on the SessionStart hook.
	require.NoError(t, db.SetSessionPendingConv("retry-sess", "conv-new"))

	feedHook(t, "retry-sess", map[string]any{
		"session_id":      "conv-new",
		"hook_event_name": "UserPromptSubmit",
		"cwd":             dir,
	})

	got, err := LoadSessionState("retry-sess")
	require.NoError(t, err)
	assert.Equal(t, StatusWorking, got.Status,
		"a hook carrying the announced conv-id must be processed, not dropped")
	assert.Equal(t, "conv-new", got.ConvID,
		"the announced conv-id may advance the row (the retry path)")
}

// A SessionStart carrying agent_id fired inside a subagent. Subagents
// share the main session's conv-id, so the foreign-process guard can't
// catch them — agent_id is the discriminator. It must not flip a
// working main session to idle, and must not clear a recorded exit
// reason.
func TestRunHookCallback_SessionStartFromSubagentIgnored(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:     "substart-sess",
		ConvID: "conv-substart",
		Status: StatusWorking,
	}))

	feedHook(t, "substart-sess", map[string]any{
		"session_id":      "conv-substart",
		"hook_event_name": "SessionStart",
		"source":          "startup",
		"agent_id":        "agent-xyz",
		"agent_type":      "Explore",
		"cwd":             dir,
	})

	got, err := LoadSessionState("substart-sess")
	require.NoError(t, err)
	assert.Equal(t, StatusWorking, got.Status,
		"a subagent-context SessionStart must not flip the main session to idle")
}

// A SessionEnd hook with a real exit reason records that reason so the
// dashboard can tell this clean exit from an unexpected death.
func TestRunHookCallback_SessionEndRecordsExitReason(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:     "end-sess",
		ConvID: "conv-end",
		Status: StatusIdle,
	}))

	feedHook(t, "end-sess", map[string]any{
		"session_id":      "conv-end",
		"hook_event_name": "SessionEnd",
		"reason":          "logout",
		"cwd":             dir,
	})

	got, err := LoadSessionState("end-sess")
	require.NoError(t, err)
	assert.Equal(t, StatusExited, got.Status, "SessionEnd(logout) marks the session exited")

	reason, err := db.GetSessionExitReason("end-sess")
	require.NoError(t, err)
	assert.Equal(t, "logout", reason,
		"a graceful SessionEnd records its reason")
}

// SessionStart clears any stale exit_reason: a resumed session is alive
// again, so a reason left over from a previous exit (or a reaper
// 'unexpected' stamp) must not linger to mislabel a later death.
func TestRunHookCallback_SessionStartClearsExitReason(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:     "start-sess",
		ConvID: "conv-start",
		Status: StatusExited,
	}))
	// The row carries a stale reason from a previous death.
	require.NoError(t, db.SetSessionExitReason("start-sess", "unexpected"))

	feedHook(t, "start-sess", map[string]any{
		"session_id":      "conv-start",
		"hook_event_name": "SessionStart",
		"cwd":             dir,
	})

	got, err := LoadSessionState("start-sess")
	require.NoError(t, err)
	assert.Equal(t, StatusIdle, got.Status, "SessionStart sets the session idle")

	reason, err := db.GetSessionExitReason("start-sess")
	require.NoError(t, err)
	assert.Equal(t, "", reason,
		"SessionStart must clear the stale exit_reason — the session is alive again")
}

// A StopFailure hook (turn ended in an API/auth/billing error) flips the
// session row to Status="error" with the error_type in status_detail.
func TestRunHookCallback_StopFailureMarksError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:     "err-sess",
		ConvID: "conv-err",
		Status: StatusWorking,
	}))

	feedHook(t, "err-sess", map[string]any{
		"session_id":      "conv-err",
		"hook_event_name": "StopFailure",
		"error_type":      "rate_limit",
		"error_message":   "Rate limit exceeded. Retry in 60s.",
		"cwd":             dir,
	})

	got, err := LoadSessionState("err-sess")
	require.NoError(t, err)
	assert.Equal(t, StatusError, got.Status,
		"StopFailure must mark the session errored")
	assert.Equal(t, "rate_limit", got.StatusDetail,
		"error_type must land in status_detail")
}

// A StopFailure with no error_type (CC always sends one, but be
// defensive) falls back to a non-empty "unknown" detail.
func TestRunHookCallback_StopFailureMissingTypeDefaultsUnknown(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:     "err-sess2",
		ConvID: "conv-err2",
		Status: StatusWorking,
	}))

	feedHook(t, "err-sess2", map[string]any{
		"session_id":      "conv-err2",
		"hook_event_name": "StopFailure",
		"cwd":             dir,
	})

	got, err := LoadSessionState("err-sess2")
	require.NoError(t, err)
	assert.Equal(t, StatusError, got.Status)
	assert.Equal(t, "unknown", got.StatusDetail,
		"a StopFailure with no error_type falls back to 'unknown'")
}

// The error status is transient, not sticky: the next normal hook event
// after a StopFailure overwrites it. A rate-limited agent that is later
// nudged/retried leaves the error state on its own — nothing else has
// to reset it.
func TestRunHookCallback_ErrorClearedByNextEvent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:     "err-sess3",
		ConvID: "conv-err3",
		Status: StatusWorking,
	}))

	feedHook(t, "err-sess3", map[string]any{
		"session_id":      "conv-err3",
		"hook_event_name": "StopFailure",
		"error_type":      "server_error",
		"cwd":             dir,
	})
	got, err := LoadSessionState("err-sess3")
	require.NoError(t, err)
	require.Equal(t, StatusError, got.Status, "precondition: session is errored")

	// The retry: CC fires UserPromptSubmit for the new turn.
	feedHook(t, "err-sess3", map[string]any{
		"session_id":      "conv-err3",
		"hook_event_name": "UserPromptSubmit",
		"cwd":             dir,
	})
	got, err = LoadSessionState("err-sess3")
	require.NoError(t, err)
	assert.Equal(t, StatusWorking, got.Status,
		"the next normal hook event must clear the error status back to working")
	assert.Equal(t, "UserPromptSubmit", got.StatusDetail,
		"the next event overwrites status_detail with its own — the stale error_type is gone")
}

// A Notification hook with notification_type=idle_prompt flips the
// session back to idle. This is the only signal we get after the user
// cancels an in-flight turn with Escape — CC fires no dedicated
// interrupt hook (anthropics/claude-code#11189, closed as not-planned).
// Without this handler the dashboard would stay stuck at e.g.
// "working: UserPromptSubmit" indefinitely.
func TestRunHookCallback_NotificationIdlePromptClearsWorking(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:           "idle-sess",
		ConvID:       "conv-idle",
		Status:       StatusWorking,
		StatusDetail: "UserPromptSubmit",
	}))

	feedHook(t, "idle-sess", map[string]any{
		"session_id":        "conv-idle",
		"hook_event_name":   "Notification",
		"notification_type": "idle_prompt",
		"cwd":               dir,
	})

	got, err := LoadSessionState("idle-sess")
	require.NoError(t, err)
	assert.Equal(t, StatusIdle, got.Status,
		"idle_prompt must transition the session back to idle")
	assert.Equal(t, "", got.StatusDetail,
		"idle_prompt must clear the stale status_detail (e.g. 'UserPromptSubmit')")
}

// TestNeedsIdentityMigration pins the predicate that decides whether a
// conv-id rotation should migrate agent identity — and, crucially, that
// it stays true until the migration actually commits (the retry
// condition) and flips false once a succession edge exists. (bool, err)
// is the contract: the caller must not advance the conv-id when err is
// non-nil — see hook_callback.go's conv-id-update block.
func TestNeedsIdentityMigration(t *testing.T) {
	t.Run("active agent, fresh new conv, no edge -> migrate", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		db.ResetForTest()
		require.NoError(t, db.EnrollAgent("conv-old", "test"))
		got, err := needsIdentityMigration("conv-old", "conv-new")
		require.NoError(t, err)
		assert.True(t, got)
	})
	t.Run("plain (un-enrolled) old conv -> no migration", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		db.ResetForTest()
		got, err := needsIdentityMigration("conv-old", "conv-new")
		require.NoError(t, err)
		assert.False(t, got, "a plain conversation's /clear must not migrate")
	})
	t.Run("retired old agent -> no migration", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		db.ResetForTest()
		require.NoError(t, db.EnrollAgent("conv-old", "test"))
		_, err := db.RetireAgent("conv-old", "test", "test")
		require.NoError(t, err)
		got, err := needsIdentityMigration("conv-old", "conv-new")
		require.NoError(t, err)
		assert.False(t, got)
	})
	t.Run("succession edge already recorded -> no retry", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		db.ResetForTest()
		require.NoError(t, db.EnrollAgent("conv-old", "test"))
		require.NoError(t, db.RecordConvSuccession("conv-old", "conv-new", "clear"))
		got, err := needsIdentityMigration("conv-old", "conv-new")
		require.NoError(t, err)
		assert.False(t, got,
			"once the migration committed (edge exists) the predicate must stop firing")
	})
	t.Run("new conv is already an agent -> no migration (collision guard)", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		db.ResetForTest()
		require.NoError(t, db.EnrollAgent("conv-old", "test"))
		require.NoError(t, db.EnrollAgent("conv-new", "test"))
		got, err := needsIdentityMigration("conv-old", "conv-new")
		require.NoError(t, err)
		assert.False(t, got, "must not migrate onto a conv that already owns an identity")
	})
}

// TestRunHookCallback_ClearMigratesAgentIdentity drives the full hook
// callback for a post-/clear SessionStart(source=clear) and asserts the
// agent's group identity migrated from the old conv-id to the new one —
// the issue #192 fix, exercised through runHookCallback itself.
func TestRunHookCallback_ClearMigratesAgentIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	const sessionID, oldConv, newConv = "clear-mig-sess", "conv-clear-old", "conv-clear-new"

	require.NoError(t, SaveSessionState(&SessionState{
		ID:     sessionID,
		ConvID: oldConv,
		Status: StatusIdle,
	}))
	// The old conv is an agent: a group member (AddAgentGroupMember
	// enrolls it).
	gID, err := db.CreateAgentGroup("alpha", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: oldConv}))

	feedHook(t, sessionID, map[string]any{
		"session_id":      newConv,
		"hook_event_name": "SessionStart",
		"source":          "clear",
		"cwd":             dir,
	})

	// The session row now follows the /clear rotation.
	got, err := LoadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, newConv, got.ConvID, "session row should follow the /clear rotation")

	// Group membership migrated old → new.
	newGroups, err := db.ListGroupsForConv(newConv)
	require.NoError(t, err)
	require.Len(t, newGroups, 1, "new conv should be the group member")
	assert.Equal(t, "alpha", newGroups[0].Name)
	oldGroups, err := db.ListGroupsForConv(oldConv)
	require.NoError(t, err)
	assert.Empty(t, oldGroups, "old conv should no longer be a member")

	// Old conv retired; succession edge recorded. We retire (not
	// delete) the old enrollment so a human can reinstate the
	// pre-rotation agent later for knowledge pings.
	oldEnr, err := db.EnrollmentState(oldConv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentRetired, oldEnr, "old conv should retire (not vanish) so it can be reinstated for knowledge pings")
	succ, err := db.GetConvSuccessor(oldConv)
	require.NoError(t, err)
	assert.Equal(t, newConv, succ, "succession edge old→new should be recorded")
}
