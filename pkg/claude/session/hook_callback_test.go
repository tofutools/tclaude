package session

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// sessionEndIsExit decides whether a SessionEnd hook means the process
// is going away. Only an exact "clear" (the /clear command, which keeps
// the process alive) is a non-exit; everything else — including an
// empty reason — counts as an exit.
func TestSessionEndIsExit(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{"", true},
		{"clear", false},
		{"logout", true},
		{"prompt_input_exit", true},
		{"other", true},
		{"Clear", true}, // case-sensitive: only exact "clear" is the no-op
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
