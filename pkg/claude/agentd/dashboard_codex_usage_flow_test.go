package agentd_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: a Codex agent has run recently, so its rollout carries a
// token_count event with a subscription rate_limits block — a 5-hour
// (primary, window_minutes≈300) and a weekly (secondary, ≈10080) window.
// tclaude has no Codex usage API, so the dashboard lifts these straight off
// the rollout. The top-bar readout renders from /api/snapshot, so the
// snapshot must carry the Codex windows under usage.codex, beside the Claude
// figures, each with percent / remaining-time / reset timestamp behind an
// availability flag.
//
// Pins the wiring end to end: a dropped Codex field, a snapshot that forgot
// to call collectCodexUsageSnapshot, a broken rollout scan, or a
// window-classification regression all fail here on the real /api/snapshot
// surface the dashboard renders from.
func TestDashboardCodexUsage_SurfacedInSnapshot(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t) // temp $HOME (the rollout lives under it) + a fresh DB

	now := time.Now()
	cx := testharness.NewCodexSim(t, f.World.HomeDir, f.World.HomeDir)
	require.NoError(t, cx.Start(), "start codex sim (writes the rollout head)")
	usage := testharness.CodexTokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}
	require.NoError(t, cx.WriteTokenCountRateLimits(usage, usage,
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 14, WindowMinutes: 300, ResetsAt: now.Add(2*time.Hour + 30*time.Minute)},
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 7, WindowMinutes: 10080, ResetsAt: now.Add(5 * 24 * time.Hour)},
	), "write a subscription rate_limits snapshot")

	// Run the scan the poller would run, then read the real snapshot.
	agentd.RefreshCodexUsageForTest()
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	require.NotNil(t, snap.Usage.Codex, "codex usage present when a recent rollout carries rate limits")
	require.True(t, snap.Usage.Codex.Available, "codex usage available")

	require.NotNil(t, snap.Usage.Codex.FiveHour, "codex 5h window present")
	assert.Equal(t, 14.0, snap.Usage.Codex.FiveHour.Pct, "codex 5h percent")
	assert.Regexp(t, `^\d+h\d+m$`, snap.Usage.Codex.FiveHour.Remaining, "codex 5h remaining format")
	assert.NotEmpty(t, snap.Usage.Codex.FiveHour.ResetsAt, "codex 5h resets_at populated")

	require.NotNil(t, snap.Usage.Codex.SevenDay, "codex weekly window present")
	assert.Equal(t, 7.0, snap.Usage.Codex.SevenDay.Pct, "codex weekly percent")
	assert.Regexp(t, `^\d+d\d+h$`, snap.Usage.Codex.SevenDay.Remaining, "codex weekly remaining format")
	assert.NotEmpty(t, snap.Usage.Codex.SevenDay.ResetsAt, "codex weekly resets_at populated")
}

// Scenario: Codex usage degrades gracefully — the dashboard omits the Codex
// line (usage.codex == null) rather than showing a broken state — across the
// ways the data goes missing, all reached via the real /api/snapshot surface:
//
//  1. Codex never run (no rollouts at all),
//  2. a rollout whose only token_count carries no usable rate limits (a turn
//     before any rate-limit header arrived — both windows null),
//  3. a rollout whose rate-limit snapshot is older than the 30-min staleness
//     cap (a long-idle Codex must not keep showing hours-old figures),
//  4. a fresh rollout whose token_count windows have already reset.
func TestDashboardCodexUsage_UnavailableDegradesGracefully(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	usage := testharness.CodexTokenUsage{InputTokens: 50, OutputTokens: 5, TotalTokens: 55}

	// Case 1: no Codex rollouts at all — Codex never ran.
	agentd.RefreshCodexUsageForTest()
	snap := fetchDashSnapshot(t, mux)
	assert.Nil(t, snap.Usage.Codex, "no codex line when Codex has never run")

	// Case 2: a token_count from before any rate-limit header arrived — both
	// slots null, so there is nothing to classify.
	cxNoRL := testharness.NewCodexSim(t, f.World.HomeDir, f.World.HomeDir)
	require.NoError(t, cxNoRL.Start())
	require.NoError(t, cxNoRL.WriteTokenCountRateLimits(usage, usage, nil, nil),
		"a token_count with both rate-limit slots null")
	agentd.RefreshCodexUsageForTest()
	snap = fetchDashSnapshot(t, mux)
	assert.Nil(t, snap.Usage.Codex, "no codex line when no token_count carries usable rate limits")

	// Case 3: a rollout carrying real rate limits, but the file is older than
	// the 30-min staleness cap — the scan skips it, so a long-idle Codex
	// degrades to nothing rather than showing stale figures.
	cxStale := testharness.NewCodexSim(t, f.World.HomeDir, f.World.HomeDir)
	require.NoError(t, cxStale.Start())
	require.NoError(t, cxStale.WriteTokenCountRateLimits(usage, usage,
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 30, WindowMinutes: 300, ResetsAt: time.Now().Add(time.Hour)},
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 20, WindowMinutes: 10080, ResetsAt: time.Now().Add(48 * time.Hour)},
	))
	old := time.Now().Add(-31 * time.Minute)
	require.NoError(t, os.Chtimes(cxStale.RolloutPath, old, old), "backdate the rollout past the staleness cap")
	agentd.RefreshCodexUsageForTest()
	snap = fetchDashSnapshot(t, mux)
	assert.Nil(t, snap.Usage.Codex, "no codex line when the rollout is older than the staleness cap")

	// Case 4: a fresh snapshot whose windows have already reset — the figures
	// predate the reset and Codex hasn't run since, so they are dropped
	// rather than shown as current. (The freshly written rollout supersedes
	// the backdated one from case 3 as the newest snapshot.)
	cxReset := testharness.NewCodexSim(t, f.World.HomeDir, f.World.HomeDir)
	require.NoError(t, cxReset.Start())
	require.NoError(t, cxReset.WriteTokenCountRateLimits(usage, usage,
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 90, WindowMinutes: 300, ResetsAt: time.Now().Add(-time.Minute)},
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 80, WindowMinutes: 10080, ResetsAt: time.Now().Add(-time.Minute)},
	), "write a snapshot whose windows have already reset")
	agentd.RefreshCodexUsageForTest()
	snap = fetchDashSnapshot(t, mux)
	assert.Nil(t, snap.Usage.Codex, "no codex line when every window has already reset")
}
