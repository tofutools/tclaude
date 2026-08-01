package statusbar

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// renderStatusline drives the real status-bar entry point end to end with
// payload on stdin, so the assertions below cover every per-session write
// the command makes — not just the attribution helper in isolation.
func renderStatusline(t *testing.T, payload string) {
	t.Helper()

	stdin, err := os.CreateTemp(t.TempDir(), "statusline-*.json")
	require.NoError(t, err)
	_, err = stdin.WriteString(payload)
	require.NoError(t, err)
	_, err = stdin.Seek(0, 0)
	require.NoError(t, err)

	stdout, err := os.CreateTemp(t.TempDir(), "statusline-out-*")
	require.NoError(t, err)

	origIn, origOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdin, stdout
	defer func() {
		os.Stdin, os.Stdout = origIn, origOut
		_ = stdin.Close()
		_ = stdout.Close()
	}()

	require.NoError(t, run())
}

func statuslinePayload(t *testing.T, convID, modelID, displayName, effort string, pct float64, window int64) string {
	t.Helper()
	payload := map[string]any{
		"session_id": convID,
		"model":      map[string]any{"id": modelID, "display_name": displayName},
		"workspace":  map[string]any{"current_dir": t.TempDir()},
		"context_window": map[string]any{
			"used_percentage":     pct,
			"total_input_tokens":  1234,
			"total_output_tokens": 56,
			"context_window_size": window,
		},
		"effort": map[string]any{"level": effort},
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return string(raw)
}

func withBilling(t *testing.T, payload string, cost float64, rateLimits bool) string {
	t.Helper()
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &decoded))
	decoded["cost"] = map[string]any{"total_cost_usd": cost}
	if rateLimits {
		decoded["rate_limits"] = map[string]any{
			"five_hour": map[string]any{
				"used_percentage": 91,
				"resets_at":       time.Now().Add(time.Hour).Unix(),
			},
		}
	}
	raw, err := json.Marshal(decoded)
	require.NoError(t, err)
	return string(raw)
}

func sessionStatusbarFields(t *testing.T, sessionID string) (autoCompactWindow, rawStatusline string) {
	t.Helper()
	d, err := db.Open()
	require.NoError(t, err)
	require.NoError(t, d.QueryRow(
		`SELECT auto_compact_window, last_statusline_json FROM sessions WHERE id = ?`,
		sessionID,
	).Scan(&autoCompactWindow, &rawStatusline))
	return autoCompactWindow, rawStatusline
}

// The reported dashboard corruption, end to end: a nested Claude Code
// started from inside an agent's pane inherits TCLAUDE_SESSION_ID, so its
// statusline renders arrive keyed to the PARENT agent's session row. Its
// model, effort and context window must not land on that row.
//
// Reproduced against Claude Code 2.1.220 with a child launched as
// `--model haiku`: every render carried the child's own conv-id, "Haiku
// 4.5" and a 200K context, while the parent was an Opus agent on a 1M
// window.
func TestStatusbar_ForeignRenderLeavesParentRowIntact(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// run() resolves git/PR context from the PROCESS cwd, not from the
	// payload's workspace.current_dir — leave the repo so the render
	// cannot shell out to git/gh for the checkout the test happens to
	// run in.
	t.Chdir(t.TempDir())
	// The host agent's own pane may pin an auto-compaction window; the
	// rebase it drives is not what these tests are about.
	t.Setenv(harness.AutoCompactWindowEnvVar, "")
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: "sess-parent", ConvID: "conv-parent"}))
	t.Setenv("TCLAUDE_SESSION_ID", "sess-parent")

	// The parent agent's own render establishes the truth.
	renderStatusline(t, statuslinePayload(t, "conv-parent", "claude-opus-5", "Opus 5", "high", 42, 1_000_000))

	parent, err := db.GetContextSnapshot("sess-parent")
	require.NoError(t, err)
	require.Equal(t, "Opus 5", parent.Model, "parent render must be recorded")
	require.NoError(t, db.UpdateSessionCost("sess-parent", 1.25))
	require.NoError(t, db.UpdateSessionVirtualCost("sess-parent", 2.50))
	require.NoError(t, db.SetSessionAutoCompactWindow("sess-parent", "450000"))
	require.NoError(t, db.UpdateStatuslineSnapshot("sess-parent", `{"owner":"parent"}`))

	// The nested child's render, carrying the parent's env session id.
	renderStatusline(t, statuslinePayload(t, "conv-child", "claude-haiku-4-5-20251001", "Haiku 4.5", "low", 17, 200_000))
	// Exercise the remaining guarded writers as well. Without rate
	// limits cost is a real API charge; with rate limits it is the
	// subscription WHAT-IF cost and also persists the raw statusline.
	// The environment-observed window would overwrite the parent's pin
	// if that writer escaped the same ownership gate.
	t.Setenv(harness.AutoCompactWindowEnvVar, "123000")
	childAPI := statuslinePayload(t, "conv-child", "claude-haiku-4-5-20251001", "Haiku 4.5", "low", 18, 200_000)
	renderStatusline(t, withBilling(t, childAPI, 99, false))
	childSubscription := statuslinePayload(t, "conv-child", "claude-haiku-4-5-20251001", "Haiku 4.5", "low", 19, 200_000)
	renderStatusline(t, withBilling(t, childSubscription, 199, true))

	got, err := db.GetContextSnapshot("sess-parent")
	require.NoError(t, err)
	assert.Equal(t, "Opus 5", got.Model, "child model must not overwrite the parent's")
	assert.Equal(t, "claude-opus-5", got.ModelID, "child model id must not overwrite the parent's")
	assert.Equal(t, "high", got.EffortLevel, "child effort must not overwrite the parent's")
	assert.Equal(t, float64(42), got.ContextPct, "child context usage must not overwrite the parent's")
	assert.Equal(t, int64(1_000_000), got.ContextWindowSize, "child window must not overwrite the parent's")
	assert.Equal(t, 1.25, got.CostUSD, "child API cost must not overwrite the parent's")
	assert.Equal(t, 2.50, got.VirtualCostUSD, "child WHAT-IF cost must not overwrite the parent's")
	autoCompactWindow, rawStatusline := sessionStatusbarFields(t, "sess-parent")
	assert.Equal(t, "450000", autoCompactWindow, "child environment must not overwrite the parent's compact window")
	assert.Equal(t, `{"owner":"parent"}`, rawStatusline, "child raw statusline must not overwrite the parent's")
}

// The same path must stay fully open for the agent's own renders: a
// second render from the tracked conversation updates the row.
func TestStatusbar_OwnRenderUpdatesRow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// run() resolves git/PR context from the PROCESS cwd, not from the
	// payload's workspace.current_dir — leave the repo so the render
	// cannot shell out to git/gh for the checkout the test happens to
	// run in.
	t.Chdir(t.TempDir())
	// The host agent's own pane may pin an auto-compaction window; the
	// rebase it drives is not what these tests are about.
	t.Setenv(harness.AutoCompactWindowEnvVar, "")
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: "sess-parent", ConvID: "conv-parent"}))
	t.Setenv("TCLAUDE_SESSION_ID", "sess-parent")

	renderStatusline(t, statuslinePayload(t, "conv-parent", "claude-opus-5", "Opus 5", "high", 42, 1_000_000))
	renderStatusline(t, statuslinePayload(t, "conv-parent", "claude-fable-5", "Fable 5", "medium", 61, 1_000_000))

	got, err := db.GetContextSnapshot("sess-parent")
	require.NoError(t, err)
	assert.Equal(t, "Fable 5", got.Model)
	assert.Equal(t, "claude-fable-5", got.ModelID)
	assert.Equal(t, float64(61), got.ContextPct)
}
