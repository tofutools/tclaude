package statusbar

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// resetTestDB points the DB at a fresh temp home and resets the singleton, so a
// test can exercise the status line's session-row read without touching the
// operator's real database.
func resetTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	db.ObserveTCL930SidecarsAtCleanup(t, dir, "statusbar-auto-compact")
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
}

// insertSession writes the minimum session row GetSessionAutoCompactWindow reads
// back, with the given auto-compaction window ("" for the common unpinned case).
func insertSession(t *testing.T, sessionID, window string) {
	t.Helper()
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO sessions (id, tmux_session, pid, cwd, conv_id, status,
		created_at, updated_at, auto_compact_window)
		VALUES (?, ?, 0, '/tmp', ?, 'idle', '2026-07-25T09:00:00Z', '2026-07-25T09:00:00Z', ?)`,
		sessionID, "tc-"+sessionID, "conv-"+sessionID, window)
	require.NoError(t, err)
}

// TestContextWindowTag pins the marker the status line appends to its model
// label. This marker is the entire fix for the reported bug: before it, a pane
// pinned to 450k of a 1M window rendered byte-identically to an unpinned one,
// because the percentage is re-based silently and nothing named the window it
// was re-based onto.
func TestContextWindowTag(t *testing.T) {
	cases := []struct {
		name            string
		effectiveWindow int64
		want            string
	}{
		{"pinned window", 450_000, "(450k)"},
		{"round million model window", 1_000_000, "(1M)"},
		{"standard model window", 200_000, "(200k)"},
		// Not a round multiple of 1000: the digits survive rather than being
		// rounded into a marker an operator would then mistrust.
		{"odd token count keeps digits", 123_456, "(123456)"},
		// Nothing known — no pin AND no context_window_size yet. Omitting the
		// marker is the correct render, not "(0)".
		{"unknown window omits marker", 0, ""},
		{"negative window omits marker", -1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, contextWindowTag(tc.effectiveWindow))
		})
	}
}

// TestResolvePinnedWindow_EnvironmentWins covers the primary path: the variable
// present in the pane's own environment is what Claude Code is acting on, so it
// beats the row even when the row disagrees.
func TestResolvePinnedWindow_EnvironmentWins(t *testing.T) {
	resetTestDB(t)
	insertSession(t, "s-env", "300000")

	observed, resolved := resolvePinnedWindow("450k", "s-env")
	assert.Equal(t, int64(450_000), observed, "environment value is what this process observed")
	assert.Equal(t, int64(450_000), resolved, "environment beats the session row")
}

// TestResolvePinnedWindow_FallsBackToSessionRow is the regression guard for the
// reported bug. A hook process that did not inherit the pane's environment must
// still find the pin, or the pane's meter measures against the model's full
// window while the dashboard — reading this same column — measures against the
// pin, and only the pane is wrong.
func TestResolvePinnedWindow_FallsBackToSessionRow(t *testing.T) {
	resetTestDB(t)
	insertSession(t, "s-row", "450000")

	observed, resolved := resolvePinnedWindow("", "s-row")
	assert.Zero(t, observed, "nothing was observed in the environment")
	assert.Equal(t, int64(450_000), resolved, "session row supplies the pin")
}

// TestResolvePinnedWindow_Unpinned pins the ORDINARY case: most agents never pin
// a window at all. Neither an absent variable nor an empty column is an error,
// and both must resolve to "no pin" so EffectiveContextWindow leaves the model's
// own window — and therefore Claude Code's own percentage — untouched.
func TestResolvePinnedWindow_Unpinned(t *testing.T) {
	resetTestDB(t)
	insertSession(t, "s-unpinned", "")

	for _, tc := range []struct {
		name      string
		sessionID string
	}{
		{"row exists with empty window", "s-unpinned"},
		{"no row for this session", "s-missing"},
		{"no session id at all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed, resolved := resolvePinnedWindow("", tc.sessionID)
			assert.Zero(t, observed)
			assert.Zero(t, resolved)

			// The end-to-end consequence: an unpinned agent's percentage is
			// passed through exactly as Claude Code reported it.
			effective := harness.EffectiveContextWindow(1_000_000, resolved)
			assert.Equal(t, int64(1_000_000), effective, "model window stands")
			assert.InDelta(t, 21.0, harness.RebaseContextPercentage(21, 1_000_000, effective), 0.001,
				"unpinned percentage is not re-based")
		})
	}
}

// TestResolvePinnedWindow_RejectsUnparseableEnvironment keeps the typo guard from
// eroding: a value the spawn boundary would refuse must not govern the bar here
// either. A bare `=500` re-based against a 500-token window would peg the bar at
// 100% and — because the caller records what this function observed — persist
// that onto the session row. The row is the fallback in that case, not the typo.
func TestResolvePinnedWindow_RejectsUnparseableEnvironment(t *testing.T) {
	resetTestDB(t)
	insertSession(t, "s-typo", "450000")

	for _, envValue := range []string{"500", "not-a-number", "99999999999", "450."} {
		t.Run(envValue, func(t *testing.T) {
			observed, resolved := resolvePinnedWindow(envValue, "s-typo")
			assert.Zero(t, observed, "an out-of-bounds or malformed value is not observed as a pin")
			assert.Equal(t, int64(450_000), resolved, "the recorded window governs instead")
		})
	}
}

// TestResolvePinnedWindow_ObservedNeverBorrowsFromRow guards the write asymmetry
// the caller depends on. UpdateSessionAutoCompactWindow is safe to run on every
// render only because an observer may add a pin it saw live and never echo one it
// read; if `observed` ever picked up the row's value, the status line would be
// writing the row's own content back to it every render.
func TestResolvePinnedWindow_ObservedNeverBorrowsFromRow(t *testing.T) {
	resetTestDB(t)
	insertSession(t, "s-asym", "450000")

	observed, resolved := resolvePinnedWindow("", "s-asym")
	assert.Zero(t, observed, "observed reflects the environment only")
	assert.NotZero(t, resolved, "resolved reflects the row")
}
