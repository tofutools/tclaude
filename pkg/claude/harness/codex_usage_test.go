package harness_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// LatestCodexUsage is exercised through the real CodexSim rollout writer, so
// the on-disk rate_limits shape under test is the one the sim records — the
// same testharness v2 discipline the telemetry tests use. External package
// for the same import-cycle reason as the other Codex read-path tests.

// epoch returns a far-future reset time; the exact value doesn't matter to
// these tests, only that ResetsAt round-trips and stays unreset.
func futureReset(d time.Duration) time.Time { return time.Now().Add(d) }

// A subscription account's token_count carries a 5-hour (primary,
// window_minutes≈300) and a weekly (secondary, ≈10080) window. Both must be
// classified by duration onto FiveHour / Weekly and round-trip percent +
// reset.
func TestLatestCodexUsage_ClassifiesBothWindows(t *testing.T) {
	home := codexTestHome(t)
	cx := testharness.NewCodexSim(t, home, "/home/u/proj")
	require.NoError(t, cx.Start())
	fiveReset := futureReset(2 * time.Hour)
	weekReset := futureReset(5 * 24 * time.Hour)
	require.NoError(t, cx.WriteTokenCountRateLimits(
		testharness.CodexTokenUsage{TotalTokens: 100},
		testharness.CodexTokenUsage{TotalTokens: 100},
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 14, WindowMinutes: 300, ResetsAt: fiveReset},
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 7, WindowMinutes: 10080, ResetsAt: weekReset},
	))

	u, err := harness.LatestCodexUsage(home, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.NotNil(t, u)
	require.NotNil(t, u.FiveHour, "5h window classified from window_minutes≈300")
	assert.Equal(t, 14.0, u.FiveHour.UsedPercent)
	assert.WithinDuration(t, fiveReset, u.FiveHour.ResetsAt, time.Second)
	require.NotNil(t, u.Weekly, "weekly window classified from window_minutes≈10080")
	assert.Equal(t, 7.0, u.Weekly.UsedPercent)
	assert.WithinDuration(t, weekReset, u.Weekly.ResetsAt, time.Second)
	assert.False(t, u.Observed.IsZero(), "observation timestamp parsed from the envelope")
}

// A free account has no 5-hour tier — its weekly cap lands in the PRIMARY
// slot. Classifying by slot would mislabel it as 5-hour; classifying by
// window_minutes puts it on Weekly with FiveHour left nil (the aistat
// slot-vs-duration fix).
func TestLatestCodexUsage_FreeTierWeeklyInPrimarySlot(t *testing.T) {
	home := codexTestHome(t)
	cx := testharness.NewCodexSim(t, home, "/home/u/proj")
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteTokenCountRateLimits(
		testharness.CodexTokenUsage{TotalTokens: 100},
		testharness.CodexTokenUsage{TotalTokens: 100},
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 42, WindowMinutes: 10080, ResetsAt: futureReset(3 * 24 * time.Hour)},
		nil,
	))

	u, err := harness.LatestCodexUsage(home, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Nil(t, u.FiveHour, "no 5h window for a free account")
	require.NotNil(t, u.Weekly, "the primary slot's weekly window classified by duration, not slot")
	assert.Equal(t, 42.0, u.Weekly.UsedPercent)
}

// A window of an unrecognised duration (e.g. a 30-day cap, window_minutes
// 43200) is ignored — it maps to neither rendered bucket, so a snapshot
// carrying only that returns nil rather than a half-filled CodexUsage.
func TestLatestCodexUsage_IgnoresUnknownWindowDuration(t *testing.T) {
	home := codexTestHome(t)
	cx := testharness.NewCodexSim(t, home, "/home/u/proj")
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteTokenCountRateLimits(
		testharness.CodexTokenUsage{TotalTokens: 100},
		testharness.CodexTokenUsage{TotalTokens: 100},
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 5, WindowMinutes: 43200, ResetsAt: futureReset(20 * 24 * time.Hour)},
		nil,
	))

	u, err := harness.LatestCodexUsage(home, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Nil(t, u, "a lone 30-day window classifies to neither bucket")
}

// Within one rollout the LAST populated token_count wins — usage climbs over
// a session and the readout must reflect the most recent figure.
func TestLatestCodexUsage_LastEventInRolloutWins(t *testing.T) {
	home := codexTestHome(t)
	cx := testharness.NewCodexSim(t, home, "/home/u/proj")
	require.NoError(t, cx.Start())
	mk := func(pct float64) error {
		return cx.WriteTokenCountRateLimits(
			testharness.CodexTokenUsage{TotalTokens: 100},
			testharness.CodexTokenUsage{TotalTokens: 100},
			&testharness.CodexRateLimitWindowSeed{UsedPercent: pct, WindowMinutes: 300, ResetsAt: futureReset(time.Hour)},
			nil,
		)
	}
	require.NoError(t, mk(10))
	require.NoError(t, mk(55)) // later turn, higher usage

	u, err := harness.LatestCodexUsage(home, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.NotNil(t, u)
	require.NotNil(t, u.FiveHour)
	assert.Equal(t, 55.0, u.FiveHour.UsedPercent, "the latest token_count's figure wins")
}

// Across rollouts the most recently observed snapshot wins — two Codex
// sessions, and the account-wide readout reflects whichever ran last.
func TestLatestCodexUsage_NewestRolloutAcrossFilesWins(t *testing.T) {
	home := codexTestHome(t)
	coarseTime := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)

	older := testharness.NewCodexSimWithID(t, home, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "/home/u/a")
	require.NoError(t, older.Start())
	older.SetNextEventTime(coarseTime.Add(100 * time.Millisecond))
	require.NoError(t, older.WriteTokenCountRateLimits(
		testharness.CodexTokenUsage{TotalTokens: 100},
		testharness.CodexTokenUsage{TotalTokens: 100},
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 11, WindowMinutes: 300, ResetsAt: futureReset(time.Hour)},
		nil,
	))
	newer := testharness.NewCodexSimWithID(t, home, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "/home/u/b")
	require.NoError(t, newer.Start())
	newer.SetNextEventTime(coarseTime.Add(900 * time.Millisecond))
	require.NoError(t, newer.WriteTokenCountRateLimits(
		testharness.CodexTokenUsage{TotalTokens: 100},
		testharness.CodexTokenUsage{TotalTokens: 100},
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 88, WindowMinutes: 300, ResetsAt: futureReset(time.Hour)},
		nil,
	))
	// Model a filesystem that stores both writes in the same coarse mtime
	// bucket. The older path sorts first, so the scan must inspect the tied
	// sibling rather than stopping after the first valid snapshot.
	require.NoError(t, os.Chtimes(older.RolloutPath, coarseTime, coarseTime))
	require.NoError(t, os.Chtimes(newer.RolloutPath, coarseTime, coarseTime))

	u, err := harness.LatestCodexUsage(home, coarseTime.Add(-time.Hour))
	require.NoError(t, err)
	require.NotNil(t, u)
	require.NotNil(t, u.FiveHour)
	assert.Equal(t, 88.0, u.FiveHour.UsedPercent, "the newest rollout's snapshot wins")
}

func TestLatestCodexUsageForConvs_IgnoresNonTargetRollouts(t *testing.T) {
	home := codexTestHome(t)
	baseTime := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)

	target := testharness.NewCodexSim(t, home, "/home/u/live")
	require.NoError(t, target.Start())
	target.SetNextEventTime(baseTime)
	require.NoError(t, target.WriteTokenCountRateLimits(
		testharness.CodexTokenUsage{TotalTokens: 100},
		testharness.CodexTokenUsage{TotalTokens: 100},
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 12, WindowMinutes: 300, ResetsAt: futureReset(time.Hour)},
		nil,
	))
	newerNonTarget := testharness.NewCodexSim(t, home, "/home/u/offline")
	require.NoError(t, newerNonTarget.Start())
	newerNonTarget.SetNextEventTime(baseTime.Add(time.Minute))
	require.NoError(t, newerNonTarget.WriteTokenCountRateLimits(
		testharness.CodexTokenUsage{TotalTokens: 100},
		testharness.CodexTokenUsage{TotalTokens: 100},
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 91, WindowMinutes: 300, ResetsAt: futureReset(time.Hour)},
		nil,
	))

	u, err := harness.LatestCodexUsageForConvs(home, []string{target.ConvID}, baseTime.Add(-time.Hour))
	require.NoError(t, err)
	require.NotNil(t, u)
	require.NotNil(t, u.FiveHour)
	assert.Equal(t, 12.0, u.FiveHour.UsedPercent, "targeted repair reads only the supplied live conv ids")
}

// A rollout whose file mtime predates `since` is skipped — the caller bounds
// the scan to recently-active sessions, so a long-idle session can't keep
// feeding stale figures into the readout.
func TestLatestCodexUsage_SkipsRolloutsOlderThanSince(t *testing.T) {
	home := codexTestHome(t)
	cx := testharness.NewCodexSim(t, home, "/home/u/proj")
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteTokenCountRateLimits(
		testharness.CodexTokenUsage{TotalTokens: 100},
		testharness.CodexTokenUsage{TotalTokens: 100},
		&testharness.CodexRateLimitWindowSeed{UsedPercent: 33, WindowMinutes: 300, ResetsAt: futureReset(time.Hour)},
		nil,
	))
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(cx.RolloutPath, old, old))

	// since = 30 min ago ⇒ the hour-old file is out of scope.
	u, err := harness.LatestCodexUsage(home, time.Now().Add(-30*time.Minute))
	require.NoError(t, err)
	assert.Nil(t, u, "a rollout modified before `since` is not read")
}

// No Codex rollouts at all ⇒ (nil, nil): the normal "Codex never ran" state,
// not an error.
func TestLatestCodexUsage_NoRollouts(t *testing.T) {
	home := codexTestHome(t)
	u, err := harness.LatestCodexUsage(home, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Nil(t, u)
}
