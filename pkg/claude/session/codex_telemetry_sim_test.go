package session_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Hooks are intentionally state-transition-only for Codex. Rollouts can grow
// to hundreds of megabytes, so neither a mid-turn nor a turn-boundary hook may
// replay one. The agentd telemetry follower owns rollout-derived context,
// effort, subscription usage, and cost from its durable byte cursor.
func TestApplyHook_CodexHooksDoNotReadRolloutTelemetry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()

	const convID = "019ec004-4250-79b1-9ade-ebaea4170170"
	const sessionID = "agent-codex"
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: sessionID, ConvID: convID, Status: session.StatusWorking,
		Harness: "codex", Cwd: "/home/u/proj",
	}))

	cx := testharness.NewCodexSimWithID(t, dir, convID, "/home/u/proj")
	cx.ContextWindow = 200000
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteUserInput("do the thing"))
	reset5h := time.Now().Add(2 * time.Hour)
	reset7d := time.Now().Add(5 * 24 * time.Hour)
	usage := testharness.CodexTokenUsage{
		InputTokens: 49000, OutputTokens: 1000, TotalTokens: 50000,
	}
	require.NoError(t, cx.WriteTokenCountRateLimits(usage, usage,
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 31, WindowMinutes: 300, ResetsAt: reset5h},
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 45, WindowMinutes: 10080, ResetsAt: reset7d},
	))

	for _, event := range []string{"PreToolUse", "Stop", "SessionStart", "PostCompact"} {
		require.NoError(t, session.ApplyHook(session.HookCallbackInput{
			HookEventName: event, ConvID: convID, Cwd: "/home/u/proj",
			TranscriptPath: cx.RolloutPath,
		}, sessionID))
	}

	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Zero(t, snap.ContextPct)
	assert.Zero(t, snap.TokensInput)
	assert.Zero(t, snap.ContextWindowSize)
	assert.Empty(t, snap.EffortLevel)
	assert.Zero(t, snap.VirtualCostUSD)
	cache, err := db.LoadCodexUsageCache()
	require.NoError(t, err)
	assert.Nil(t, cache)
}
