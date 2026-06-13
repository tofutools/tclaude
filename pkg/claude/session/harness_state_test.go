package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestSessionState_HarnessRoundTripsThroughLoadMutateSave guards the
// session-side durability gap the cold review on #315 flagged: the hook
// callback runs load→mutate→save through SessionState, and if toRow /
// fromRow drop Harness, a saved 'codex' tag round-trips to empty →
// db.SaveSession coalesces it back to 'claude' → the ON-CONFLICT update
// resets the row every hook tick. Carrying Harness through SessionState
// closes that — this asserts the tag survives the cycle at the session
// layer (the real path), not just the raw db.SessionRow layer.
func TestSessionState_HarnessRoundTripsThroughLoadMutateSave(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:      "s1",
		ConvID:  "c1",
		Status:  StatusIdle,
		Harness: "codex",
	}))

	// Load → mutate → save, exactly what the hook callback does each tick.
	state, err := LoadSessionState("s1")
	require.NoError(t, err)
	assert.Equal(t, "codex", state.Harness, "fromRow carries the harness tag")

	state.Status = StatusWorking
	require.NoError(t, SaveSessionState(state))

	again, err := LoadSessionState("s1")
	require.NoError(t, err)
	assert.Equal(t, "codex", again.Harness, "tag survives the load→mutate→save cycle")
	assert.Equal(t, StatusWorking, again.Status)
}

// TestSessionState_FreshDefaultsToClaude pins that a fresh state with no
// harness set lands as 'claude' (the DB coalesce), so CC spawns and
// auto-registered sessions keep their tag without every call site setting
// it explicitly.
func TestSessionState_FreshDefaultsToClaude(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{ID: "s2", ConvID: "c2", Status: StatusIdle}))
	got, err := LoadSessionState("s2")
	require.NoError(t, err)
	assert.Equal(t, "claude", got.Harness, "an untagged session defaults to claude")
}
