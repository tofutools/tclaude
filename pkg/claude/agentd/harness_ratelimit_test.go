package agentd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// writeRateLimitConfig writes the operator's ratelimit ceilings to the config
// file the gate reads, exactly as an operator would.
func writeRateLimitConfig(t *testing.T, fiveHour, sevenDay float64) {
	t.Helper()
	cfg := map[string]any{"ratelimit": map[string]any{
		"five_hour_percent_max_used": fiveHour,
		"seven_day_percent_max_used": sevenDay,
	}}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o755))
	require.NoError(t, os.WriteFile(config.ConfigPath(), raw, 0o600))
}

func seedClaudeUsage(t *testing.T, fetchedAt time.Time, cached usageapi.CachedUsage) {
	t.Helper()
	cached.FetchedAt = fetchedAt
	raw, err := json.Marshal(cached)
	require.NoError(t, err)
	require.NoError(t, db.SaveUsageCache(raw, fetchedAt, fetchedAt))
}

func seedCodexUsage(t *testing.T, u harness.CodexUsage) {
	t.Helper()
	raw, err := json.Marshal(u)
	require.NoError(t, err)
	stored, err := db.SaveCodexUsageCacheIfNewer(raw, u.Observed, "test")
	require.NoError(t, err)
	require.True(t, stored)
}

func TestHarnessRateLimitHoldWithoutConfiguredCeilingNeverHolds(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	seedClaudeUsage(t, now, usageapi.CachedUsage{
		FiveHour: &usageapi.CachedBucket{Pct: 99, ResetsAt: now.Add(time.Hour)},
	})

	assert.Nil(t, harnessRateLimitHold(loadRateLimitPolicy(), harness.DefaultName, now),
		"an operator who configured no ratelimit block opted out of gating entirely")
}

func TestHarnessRateLimitHoldReportsClaudeFiveHourWindow(t *testing.T) {
	setupTestDB(t)
	writeRateLimitConfig(t, 80, 95)
	now := time.Now()
	reset := now.Add(90 * time.Minute)
	seedClaudeUsage(t, now, usageapi.CachedUsage{
		FiveHour: &usageapi.CachedBucket{Pct: 92.5, ResetsAt: reset},
		SevenDay: &usageapi.CachedBucket{Pct: 40, ResetsAt: now.Add(72 * time.Hour)},
	})

	hold := harnessRateLimitHold(loadRateLimitPolicy(), harness.DefaultName, now)
	require.NotNil(t, hold)
	assert.Equal(t, harness.DefaultName, hold.Harness)
	assert.Equal(t, "five_hour", hold.Window)
	assert.Equal(t, 92.5, hold.Pct)
	assert.Equal(t, 80.0, hold.Threshold)
	assert.WithinDuration(t, reset, hold.ResetsAt, time.Second)
}

func TestHarnessRateLimitHoldGatesSonnetWeeklyWindow(t *testing.T) {
	setupTestDB(t)
	writeRateLimitConfig(t, 80, 95)
	now := time.Now()
	reset := now.Add(48 * time.Hour)
	seedClaudeUsage(t, now, usageapi.CachedUsage{
		FiveHour:       &usageapi.CachedBucket{Pct: 10, ResetsAt: now.Add(time.Hour)},
		SevenDaySonnet: &usageapi.CachedBucket{Pct: 99, ResetsAt: reset},
	})

	hold := harnessRateLimitHold(loadRateLimitPolicy(), harness.DefaultName, now)
	require.NotNil(t, hold, "the Sonnet weekly bucket limits the account as hard as the general one")
	assert.Equal(t, "seven_day_sonnet", hold.Window)
	assert.Equal(t, 95.0, hold.Threshold)
	assert.WithinDuration(t, reset, hold.ResetsAt, time.Second)
}

func TestHarnessRateLimitHoldPrefersTheLatestReset(t *testing.T) {
	setupTestDB(t)
	writeRateLimitConfig(t, 50, 50)
	now := time.Now()
	weekly := now.Add(80 * time.Hour)
	seedClaudeUsage(t, now, usageapi.CachedUsage{
		FiveHour: &usageapi.CachedBucket{Pct: 90, ResetsAt: now.Add(time.Hour)},
		SevenDay: &usageapi.CachedBucket{Pct: 90, ResetsAt: weekly},
	})

	hold := harnessRateLimitHold(loadRateLimitPolicy(), harness.DefaultName, now)
	require.NotNil(t, hold)
	assert.Equal(t, "seven_day", hold.Window,
		"work may only start once every exceeded window has reset, so the last reset wins")
	assert.WithinDuration(t, weekly, hold.ResetsAt, time.Second)
}

func TestHarnessRateLimitHoldIgnoresElapsedAndMissingResets(t *testing.T) {
	setupTestDB(t)
	writeRateLimitConfig(t, 50, 50)
	now := time.Now()
	seedClaudeUsage(t, now, usageapi.CachedUsage{
		FiveHour: &usageapi.CachedBucket{Pct: 99, ResetsAt: now.Add(-time.Minute)},
		SevenDay: &usageapi.CachedBucket{Pct: 99},
	})

	assert.Nil(t, harnessRateLimitHold(loadRateLimitPolicy(), harness.DefaultName, now),
		"an elapsed window's percentage is stale and a reset-less one has no point to resume at")
}

func TestHarnessRateLimitHoldIgnoresStaleClaudeReading(t *testing.T) {
	setupTestDB(t)
	writeRateLimitConfig(t, 50, 50)
	now := time.Now()
	stale := now.Add(-config.DefaultUsageIdleTimeout - time.Hour)
	seedClaudeUsage(t, stale, usageapi.CachedUsage{
		FiveHour: &usageapi.CachedBucket{Pct: 99, ResetsAt: now.Add(time.Hour)},
	})

	assert.Nil(t, harnessRateLimitHold(loadRateLimitPolicy(), harness.DefaultName, now),
		"a reading the dashboard has stopped trusting must not hold work back either")
}

func TestHarnessRateLimitHoldReadsCodexWindows(t *testing.T) {
	setupTestDB(t)
	writeRateLimitConfig(t, 80, 95)
	now := time.Now()
	reset := now.Add(30 * time.Hour)
	seedCodexUsage(t, harness.CodexUsage{
		Observed: now.Add(-time.Hour),
		FiveHour: &harness.CodexRateLimitWindow{UsedPercent: 12, ResetsAt: now.Add(2 * time.Hour)},
		Weekly:   &harness.CodexRateLimitWindow{UsedPercent: 97, ResetsAt: reset},
	})

	assert.Nil(t, harnessRateLimitHold(loadRateLimitPolicy(), harness.DefaultName, now),
		"the Claude cache is empty, so a Codex reading must not gate a Claude spawn")
	hold := harnessRateLimitHold(loadRateLimitPolicy(), harness.CodexName, now)
	require.NotNil(t, hold)
	assert.Equal(t, harness.CodexName, hold.Harness)
	assert.Equal(t, "weekly", hold.Window)
	assert.Equal(t, 95.0, hold.Threshold)
	assert.WithinDuration(t, reset, hold.ResetsAt, time.Second)
}

func TestHarnessRateLimitHoldIgnoresStaleCodexReading(t *testing.T) {
	setupTestDB(t)
	writeRateLimitConfig(t, 80, 95)
	now := time.Now()
	seedCodexUsage(t, harness.CodexUsage{
		Observed: now.Add(-codexUsageMaxAge - time.Hour),
		Weekly:   &harness.CodexRateLimitWindow{UsedPercent: 97, ResetsAt: now.Add(time.Hour)},
	})

	assert.Nil(t, harnessRateLimitHold(loadRateLimitPolicy(), harness.CodexName, now))
}

func TestHarnessRateLimitHoldReadsCopilotMonthlyQuota(t *testing.T) {
	setupTestDB(t)
	writeRateLimitConfig(t, 80, 90)
	now := time.Now()
	reset := copilotMonthlyResetAt(now)
	stored, err := db.SaveSubscriptionUsageSample(db.SubscriptionUsageSample{
		Provider: db.SubscriptionProviderGitHub, ObservedAt: now.Add(-time.Minute), Source: "test",
		Windows: []db.SubscriptionUsageWindow{{
			Name: "monthly", UsedPercent: 96, UsedUnits: 96, LimitUnits: 100, ResetsAt: reset,
		}},
	})
	require.NoError(t, err)
	require.True(t, stored)

	hold := harnessRateLimitHold(loadRateLimitPolicy(), harness.CopilotName, now)
	require.NotNil(t, hold, "the monthly premium-request quota is a finite paid allowance and is gated")
	assert.Equal(t, "monthly", hold.Window)
	assert.Equal(t, 90.0, hold.Threshold, "a month-long window is governed by the long-window ceiling")
	assert.WithinDuration(t, reset, hold.ResetsAt, time.Second)
}

func TestHarnessRateLimitHoldNeverHoldsOpenCode(t *testing.T) {
	setupTestDB(t)
	writeRateLimitConfig(t, 1, 1)
	now := time.Now()
	seedClaudeUsage(t, now, usageapi.CachedUsage{
		FiveHour: &usageapi.CachedBucket{Pct: 99, ResetsAt: now.Add(time.Hour)},
	})
	seedCodexUsage(t, harness.CodexUsage{
		Observed: now, Weekly: &harness.CodexRateLimitWindow{UsedPercent: 99, ResetsAt: now.Add(time.Hour)},
	})

	assert.Nil(t, harnessRateLimitHold(loadRateLimitPolicy(), harness.OpenCodeName, now),
		"OpenCode runs on the operator's own provider keys, so there is no subscription window to respect")
}
