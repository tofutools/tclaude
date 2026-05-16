package agentd_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
)

// dashUsage mirrors agentd.dashboardUsage — the account-wide
// subscription usage readout the dashboard renders in its top bar.
type dashUsage struct {
	Available bool          `json:"available"`
	FiveHour  *dashUsageWin `json:"five_hour"`
	SevenDay  *dashUsageWin `json:"seven_day"`
}

// dashUsageWin mirrors agentd.usageWindow — one rolling-limit window.
type dashUsageWin struct {
	Pct       float64 `json:"pct"`
	ResetsAt  string  `json:"resets_at"`
	Remaining string  `json:"remaining"`
}

// seedUsageCache writes a usage reading into the SQLite usage_cache
// table — the same row the statusbar populates in production and the
// row usageapi.Peek (the snapshot's read path) reads back.
func seedUsageCache(t *testing.T, cu usageapi.CachedUsage) {
	t.Helper()
	blob, err := json.Marshal(cu)
	require.NoError(t, err, "marshal usage cache blob")
	require.NoError(t, db.SaveUsageCache(blob, cu.FetchedAt, cu.LastAttemptAt), "save usage cache")
}

// Scenario: the SQLite usage_cache carries a fresh subscription
// reading — a 5-hour and a 7-day rolling window — exactly as the
// statusbar leaves it after a Claude Code session renders its
// statusline. The dashboard's top-bar readout renders from
// /api/snapshot, so the snapshot must carry, per window, the percent
// consumed, a human remaining-time string, and the raw reset
// timestamp, behind an availability flag.
//
// Pins the wiring end to end: a dropped Usage field, a snapshot that
// forgot to call collectUsageSnapshot, or a broken usageapi.Peek read
// path all fail here, on the real /api/snapshot surface the dashboard
// renders from.
func TestDashboardUsage_SurfacedInSnapshot(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	newFlow(t) // temp $HOME + a fresh SQLite DB

	now := time.Now()
	seedUsageCache(t, usageapi.CachedUsage{
		FiveHour:      &usageapi.CachedBucket{Pct: 17, ResetsAt: now.Add(2*time.Hour + 16*time.Minute)},
		SevenDay:      &usageapi.CachedBucket{Pct: 10, ResetsAt: now.Add(5*24*time.Hour + 9*time.Hour)},
		FetchedAt:     now,
		LastAttemptAt: now,
	})

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	require.True(t, snap.Usage.Available, "usage available when the cache is fresh")

	require.NotNil(t, snap.Usage.FiveHour, "5h window present")
	assert.Equal(t, 17.0, snap.Usage.FiveHour.Pct, "5h percent")
	assert.Regexp(t, `^\d+h\d+m$`, snap.Usage.FiveHour.Remaining, "5h remaining time format")
	assert.NotEmpty(t, snap.Usage.FiveHour.ResetsAt, "5h resets_at populated")

	require.NotNil(t, snap.Usage.SevenDay, "7d window present")
	assert.Equal(t, 10.0, snap.Usage.SevenDay.Pct, "7d percent")
	assert.Regexp(t, `^\d+d\d+h$`, snap.Usage.SevenDay.Remaining, "7d remaining time format")
	assert.NotEmpty(t, snap.Usage.SevenDay.ResetsAt, "7d resets_at populated")
}

// Scenario: subscription usage is "sometimes not available". The
// readout must degrade gracefully — Available=false, no windows — so
// the dashboard can show a muted "usage: n/a" instead of a broken or
// error state. Three ways the data goes missing, all reached via the
// real /api/snapshot surface:
//
//  1. nothing cached yet (cold start),
//  2. a cached reading that has gone stale past the 30-min cap,
//  3. a fresh cache entry with no rolling-limit buckets — e.g. an
//     API-billing account, which has cost but no 5h/7d windows.
func TestDashboardUsage_UnavailableDegradesGracefully(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	// Case 1: no usage cached at all — the cold-start state.
	snap := fetchDashSnapshot(t, mux)
	assert.False(t, snap.Usage.Available, "unavailable when nothing is cached")
	assert.Nil(t, snap.Usage.FiveHour, "no 5h window when unavailable")
	assert.Nil(t, snap.Usage.SevenDay, "no 7d window when unavailable")

	// Case 2: a cached reading older than the 30-min staleness cap —
	// a dead source must not keep showing hours-old figures.
	stale := time.Now().Add(-31 * time.Minute)
	seedUsageCache(t, usageapi.CachedUsage{
		FiveHour:      &usageapi.CachedBucket{Pct: 50, ResetsAt: time.Now().Add(time.Hour)},
		SevenDay:      &usageapi.CachedBucket{Pct: 40, ResetsAt: time.Now().Add(48 * time.Hour)},
		FetchedAt:     stale,
		LastAttemptAt: stale,
	})
	snap = fetchDashSnapshot(t, mux)
	assert.False(t, snap.Usage.Available, "unavailable when the cached reading is stale")

	// Case 3: a fresh cache entry carrying no rolling-limit buckets.
	now := time.Now()
	seedUsageCache(t, usageapi.CachedUsage{FetchedAt: now, LastAttemptAt: now})
	snap = fetchDashSnapshot(t, mux)
	assert.False(t, snap.Usage.Available, "unavailable for an account with no rolling-limit windows")
}
