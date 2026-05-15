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
}
