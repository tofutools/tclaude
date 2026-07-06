package session_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// End-to-end at the real surfaces: a CodexSim writes a faithful rollout
// (the same on-disk token_count shape every Codex flow test records), the
// production hook callback runs, and the context snapshot the dashboard /
// `agent context-info` read (db.GetContextSnapshot) reflects it — the way
// the statusbar populates it for Claude Code. External package so it can
// drive both the CodexSim and the exported hook entry without the
// testharness → session import cycle.
func TestApplyHook_CodexStopPersistsContextSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()

	const convID = "019ec004-4250-79b1-9ade-ebaea4170170"
	const sessionID = "agent-codex"

	// A Codex session tclaude spawned: its row is tagged harness=codex.
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID:      sessionID,
		ConvID:  convID,
		Status:  session.StatusWorking,
		Harness: "codex",
		Cwd:     "/home/u/proj",
	}))

	// The sim owns the rollout. One full turn, then a token_count putting
	// the window at 25% (50000 / 200000).
	cx := testharness.NewCodexSimWithID(t, dir, convID, "/home/u/proj")
	cx.ContextWindow = 200000
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteUserInput("do the thing"))
	require.NoError(t, cx.WriteAgentMessage("done"))
	require.NoError(t, cx.WriteTokenCount(
		testharness.CodexTokenUsage{InputTokens: 120000, OutputTokens: 8000, TotalTokens: 128000},
		testharness.CodexTokenUsage{InputTokens: 49000, OutputTokens: 1000, TotalTokens: 50000}))

	// Codex fires a Stop hook at turn end (it has no SessionEnd event); the
	// callback lifts the rollout telemetry onto the sessions row.
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "Stop",
		ConvID:        convID,
		Cwd:           "/home/u/proj",
	}, sessionID))

	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 25.0, snap.ContextPct, 0.001, "context% from the rollout's latest token_count")
	assert.Equal(t, int64(49000), snap.TokensInput)
	assert.Equal(t, int64(1000), snap.TokensOutput)
	assert.Equal(t, int64(200000), snap.ContextWindowSize)
}

// The hook payload's transcript_path is the session's rollout, so the
// callback reads it directly (no ~/.codex/sessions walk). Passing it
// through produces the same snapshot as the by-id fallback above.
func TestApplyHook_CodexStopUsesTranscriptPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()

	const convID = "019ec004-4250-79b1-9ade-ebaea4170171"
	const sessionID = "agent-codex-tp"

	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID:      sessionID,
		ConvID:  convID,
		Status:  session.StatusWorking,
		Harness: "codex",
		Cwd:     "/home/u/proj",
	}))

	cx := testharness.NewCodexSimWithID(t, dir, convID, "/home/u/proj")
	cx.ContextWindow = 200000
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteUserInput("do the thing"))
	require.NoError(t, cx.WriteTokenCount(
		testharness.CodexTokenUsage{InputTokens: 49000, OutputTokens: 1000, TotalTokens: 50000},
		testharness.CodexTokenUsage{InputTokens: 49000, OutputTokens: 1000, TotalTokens: 50000}))

	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName:  "Stop",
		ConvID:         convID,
		Cwd:            "/home/u/proj",
		TranscriptPath: cx.RolloutPath, // the rollout, fed straight in
	}, sessionID))

	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 25.0, snap.ContextPct, 0.001)
	assert.Equal(t, int64(200000), snap.ContextWindowSize)
}

func TestApplyHook_CodexStopPersistsUsageCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()

	const convID = "019ec004-4250-79b1-9ade-ebaea4170173"
	const sessionID = "agent-codex-usage"

	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID:      sessionID,
		ConvID:  convID,
		Status:  session.StatusWorking,
		Harness: "codex",
		Cwd:     "/home/u/proj",
	}))

	cx := testharness.NewCodexSimWithID(t, dir, convID, "/home/u/proj")
	require.NoError(t, cx.Start())
	reset5h := time.Now().Add(2 * time.Hour)
	reset7d := time.Now().Add(5 * 24 * time.Hour)
	usage := testharness.CodexTokenUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}
	require.NoError(t, cx.WriteTokenCountRateLimits(usage, usage,
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 31, WindowMinutes: 300, ResetsAt: reset5h},
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 45, WindowMinutes: 10080, ResetsAt: reset7d},
	))

	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName:  "Stop",
		ConvID:         convID,
		Cwd:            "/home/u/proj",
		TranscriptPath: cx.RolloutPath,
	}, sessionID))

	row, err := db.LoadCodexUsageCache()
	require.NoError(t, err)
	require.NotNil(t, row, "Stop hook writes Codex usage cache")
	assert.Equal(t, cx.RolloutPath, row.Source)

	var got harness.CodexUsage
	require.NoError(t, json.Unmarshal(row.Data, &got))
	require.NotNil(t, got.FiveHour)
	assert.Equal(t, 31.0, got.FiveHour.UsedPercent)
	require.NotNil(t, got.Weekly)
	assert.Equal(t, 45.0, got.Weekly.UsedPercent)
}

// Telemetry is refreshed at turn boundaries, not on every hook: a
// mid-turn PreToolUse must NOT read the rollout (it would, per tool call,
// walk ~/.codex/sessions on the fallback path), so context_pct stays put;
// the turn-ending Stop then persists it. Locks in the per-hook-walk fix.
func TestApplyHook_CodexTelemetryOnlyAtTurnBoundary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()

	const convID = "019ec004-4250-79b1-9ade-ebaea4170172"
	const sessionID = "agent-codex-tb"

	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID:      sessionID,
		ConvID:  convID,
		Status:  session.StatusWorking,
		Harness: "codex",
		Cwd:     "/home/u/proj",
	}))

	cx := testharness.NewCodexSimWithID(t, dir, convID, "/home/u/proj")
	cx.ContextWindow = 200000
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteUserInput("do the thing"))
	require.NoError(t, cx.WriteTokenCount(
		testharness.CodexTokenUsage{InputTokens: 49000, OutputTokens: 1000, TotalTokens: 50000},
		testharness.CodexTokenUsage{InputTokens: 49000, OutputTokens: 1000, TotalTokens: 50000}))

	// Mid-turn tool hook: must not persist telemetry.
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "PreToolUse",
		ConvID:        convID,
		ToolName:      "Bash",
	}, sessionID))
	mid, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Zero(t, mid.ContextPct, "PreToolUse must not refresh Codex context%")

	// Turn end: now it persists.
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "Stop",
		ConvID:        convID,
	}, sessionID))
	end, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 25.0, end.ContextPct, 0.001, "Stop refreshes context%")
}
