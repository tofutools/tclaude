package agentd_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
)

// dashUsage mirrors agentd.dashboardUsage — the account-wide
// subscription usage readout the dashboard renders in its top bar.
type dashUsage struct {
	Available bool          `json:"available"`
	FiveHour  *dashUsageWin `json:"five_hour"`
	SevenDay  *dashUsageWin `json:"seven_day"`
	// TotalCostUSD is the month-to-date API cost summed from the
	// session_cost_daily table — independent of Available, so an
	// API-billing account shows a dollar figure where "usage: n/a"
	// would sit.
	TotalCostUSD float64 `json:"total_cost_usd"`
	// TodayCostUSD is the same aggregate windowed to the current local
	// day — the top bar's "(today)" figure beside "(mtd)".
	TodayCostUSD float64 `json:"today_cost_usd"`
	// Codex mirrors agentd.codexDashboardUsage — the Codex account's
	// subscription windows, lifted from Codex's local rollout files.
	// nil when Codex has no recent usage data.
	Codex *dashCodexUsage `json:"codex"`
}

// dashCodexUsage mirrors agentd.codexDashboardUsage.
type dashCodexUsage struct {
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

	// Case 2: a cached reading older than the idle-timeout grace — a truly
	// dead source (no statusline, no successful opt-in API poll for days) must
	// eventually stop showing days-old figures. Just past the default 3-day
	// window (config.DefaultUsageIdleTimeout).
	stale := time.Now().Add(-config.DefaultUsageIdleTimeout - time.Hour)
	seedUsageCache(t, usageapi.CachedUsage{
		FiveHour:      &usageapi.CachedBucket{Pct: 50, ResetsAt: time.Now().Add(time.Hour)},
		SevenDay:      &usageapi.CachedBucket{Pct: 40, ResetsAt: time.Now().Add(48 * time.Hour)},
		FetchedAt:     stale,
		LastAttemptAt: stale,
	})
	snap = fetchDashSnapshot(t, mux)
	assert.False(t, snap.Usage.Available, "unavailable when the cached reading is older than the idle timeout")

	// Case 2b: the same reading, but only a few hours old — well within the
	// 3-day grace. It must STILL show, off the last-known figures, even
	// though the live source (statusline plus any opt-in API poll) has gone
	// quiet. This is the "hiding too soon" fix: an overnight idle spell used
	// to blank the readout within 30 minutes.
	fresh := time.Now().Add(-6 * time.Hour)
	seedUsageCache(t, usageapi.CachedUsage{
		FiveHour:      &usageapi.CachedBucket{Pct: 50, ResetsAt: time.Now().Add(time.Hour)},
		SevenDay:      &usageapi.CachedBucket{Pct: 40, ResetsAt: time.Now().Add(48 * time.Hour)},
		FetchedAt:     fresh,
		LastAttemptAt: fresh,
	})
	snap = fetchDashSnapshot(t, mux)
	assert.True(t, snap.Usage.Available, "still available hours after the source went quiet, within the idle grace")

	// Case 3: a fresh cache entry carrying no rolling-limit buckets.
	now := time.Now()
	seedUsageCache(t, usageapi.CachedUsage{FetchedAt: now, LastAttemptAt: now})
	snap = fetchDashSnapshot(t, mux)
	assert.False(t, snap.Usage.Available, "unavailable for an account with no rolling-limit windows")
}

// Scenario: a Claude Code statusline render reports both the 5h and 7d
// windows; the next render omits the 7d bucket — the Anthropic usage API
// drops a window when it has nothing fresh to report. The dashboard's
// top-bar 7d bar must not flicker out: UpdateFromStatusLine (the real
// statusbar write path) carries the last-known nonzero, unreset window
// forward, so /api/snapshot still surfaces it. Pins the operator's stated
// requirement — keep the 7d (and 5h) bars while there's still usage within
// the window — at the real dashboard surface.
func TestDashboardUsage_SevenDayCarriedForwardWhenRenderOmitsIt(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	newFlow(t) // temp $HOME + a fresh SQLite DB

	now := time.Now()
	// First render: both windows present, exactly as the statusbar leaves
	// the SQLite usage_cache after a session renders its statusline.
	usageapi.UpdateFromStatusLine(
		&usageapi.CachedBucket{Pct: 18, ResetsAt: now.Add(2 * time.Hour)},
		&usageapi.CachedBucket{Pct: 33, ResetsAt: now.Add(4 * 24 * time.Hour)},
		nil,
	)
	// Next render: only the 5h window; the API dropped the 7d bucket.
	usageapi.UpdateFromStatusLine(
		&usageapi.CachedBucket{Pct: 21, ResetsAt: now.Add(90 * time.Minute)},
		nil,
		nil,
	)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	require.True(t, snap.Usage.Available, "usage available — windows carried in the cache")
	require.NotNil(t, snap.Usage.SevenDay, "7d window still surfaced after the render dropped its bucket")
	assert.Equal(t, 33.0, snap.Usage.SevenDay.Pct, "carried-forward 7d keeps its last-known percent")
	require.NotNil(t, snap.Usage.FiveHour, "5h window present")
	assert.Equal(t, 21.0, snap.Usage.FiveHour.Pct, "fresh 5h reading wins over the carried one")
}

// Scenario: the 5h and 7d bars are shown as a pair or not at all. A
// window that has reset (its 5h/7d have elapsed) reads as 0% rather than
// vanishing, so the two bars never appear or disappear independently. The
// readout disappears entirely only when there's no live subscription usage
// at all. Drives the operator's stated rule at the real /api/snapshot
// surface across the cases that distinguish it.
func TestDashboardUsage_ShowsBothWindowsOrNeither(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	// Case A: only the 5h window is live (mid-session, nothing yet on the
	// week). The 7d bar still renders, at 0%.
	now := time.Now()
	seedUsageCache(t, usageapi.CachedUsage{
		FiveHour:      &usageapi.CachedBucket{Pct: 25, ResetsAt: now.Add(3 * time.Hour)},
		FetchedAt:     now,
		LastAttemptAt: now,
	})
	snap := fetchDashSnapshot(t, mux)
	require.True(t, snap.Usage.Available, "available when one window is live")
	require.NotNil(t, snap.Usage.FiveHour, "5h present")
	assert.Equal(t, 25.0, snap.Usage.FiveHour.Pct, "5h shows its live percent")
	require.NotNil(t, snap.Usage.SevenDay, "7d bar paired with 5h, not dropped")
	assert.Equal(t, 0.0, snap.Usage.SevenDay.Pct, "absent 7d window reads as 0%")

	// Case B: the operator's headline case — the 5h window has reset (its
	// last reading is older than 5h), but the week still has usage. The 5h
	// bar must read 0%, not the stale 80%, and the 7d bar must stay.
	now = time.Now()
	seedUsageCache(t, usageapi.CachedUsage{
		FiveHour:      &usageapi.CachedBucket{Pct: 80, ResetsAt: now.Add(-10 * time.Minute)},
		SevenDay:      &usageapi.CachedBucket{Pct: 15, ResetsAt: now.Add(4 * 24 * time.Hour)},
		FetchedAt:     now,
		LastAttemptAt: now,
	})
	snap = fetchDashSnapshot(t, mux)
	require.True(t, snap.Usage.Available, "available while the week still has usage")
	require.NotNil(t, snap.Usage.FiveHour, "5h bar kept, paired with 7d")
	assert.Equal(t, 0.0, snap.Usage.FiveHour.Pct, "reset 5h reads 0%, not the stale percent")
	assert.Empty(t, snap.Usage.FiveHour.Remaining, "reset 5h carries no remaining-time hint")
	require.NotNil(t, snap.Usage.SevenDay, "7d still present")
	assert.Equal(t, 15.0, snap.Usage.SevenDay.Pct, "7d shows its live percent")

	// Case C: every window has reset — nothing live, so the readout
	// disappears rather than showing a pair of 0% bars.
	now = time.Now()
	seedUsageCache(t, usageapi.CachedUsage{
		FiveHour:      &usageapi.CachedBucket{Pct: 50, ResetsAt: now.Add(-time.Hour)},
		SevenDay:      &usageapi.CachedBucket{Pct: 40, ResetsAt: now.Add(-time.Minute)},
		FetchedAt:     now,
		LastAttemptAt: now,
	})
	snap = fetchDashSnapshot(t, mux)
	assert.False(t, snap.Usage.Available, "unavailable when no window is live")
	assert.Nil(t, snap.Usage.FiveHour, "no 5h bar when nothing is live")
	assert.Nil(t, snap.Usage.SevenDay, "no 7d bar when nothing is live")

	// Case D: both windows are open (future resets) but at 0% — a current
	// account that simply hasn't spent into either window yet. The data IS
	// valid: a future reset means the period is live, so both bars render at
	// a genuine 0% rather than collapsing to "usage: n/a". This is the
	// operator's headline case — the statusline shows "5h 0% / 7d 0%" and the
	// dashboard must agree, not show n/a.
	now = time.Now()
	seedUsageCache(t, usageapi.CachedUsage{
		FiveHour:      &usageapi.CachedBucket{Pct: 0, ResetsAt: now.Add(2 * time.Hour)},
		SevenDay:      &usageapi.CachedBucket{Pct: 0, ResetsAt: now.Add(5 * 24 * time.Hour)},
		FetchedAt:     now,
		LastAttemptAt: now,
	})
	snap = fetchDashSnapshot(t, mux)
	require.True(t, snap.Usage.Available, "available when both windows are live at 0%")
	require.NotNil(t, snap.Usage.FiveHour, "5h bar present at 0%")
	assert.Equal(t, 0.0, snap.Usage.FiveHour.Pct, "5h shows a genuine 0%")
	assert.NotEmpty(t, snap.Usage.FiveHour.Remaining, "live 0% 5h keeps its remaining-time hint")
	require.NotNil(t, snap.Usage.SevenDay, "7d bar present at 0%")
	assert.Equal(t, 0.0, snap.Usage.SevenDay.Pct, "7d shows a genuine 0%")
	assert.NotEmpty(t, snap.Usage.SevenDay.Remaining, "live 0% 7d keeps its remaining-time hint")
}

// Scenario: the operator's "hiding too soon" report. The weekly (7d) bucket
// still holds real usage but arrives WITHOUT a resets_at — the Anthropic
// usage API commonly drops that field once a session goes quiet — while the
// 5h window has reset overnight. The old gate required a future reset on
// SOME window and hid the whole Claude readout, leaving only Codex on the
// top bar. It must now stay: a reset-less window with a nonzero percent is
// live usage and keeps the readout up. Drives the fix at the real
// /api/snapshot surface.
func TestDashboardUsage_SevenDayWithoutResetStillShows(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	now := time.Now()
	// 5h reset overnight (elapsed); 7d carries a real percent but no reset.
	seedUsageCache(t, usageapi.CachedUsage{
		FiveHour:      &usageapi.CachedBucket{Pct: 60, ResetsAt: now.Add(-3 * time.Hour)},
		SevenDay:      &usageapi.CachedBucket{Pct: 42}, // zero ResetsAt
		FetchedAt:     now,
		LastAttemptAt: now,
	})
	snap := fetchDashSnapshot(t, mux)
	require.True(t, snap.Usage.Available, "readout stays up on a reset-less 7d window that still has usage")
	require.NotNil(t, snap.Usage.SevenDay, "7d bar present")
	assert.Equal(t, 42.0, snap.Usage.SevenDay.Pct, "7d shows its real percent")
	require.NotNil(t, snap.Usage.FiveHour, "5h bar paired in, at 0%")
	assert.Equal(t, 0.0, snap.Usage.FiveHour.Pct, "reset 5h reads 0%, not its stale percent")
}

// seedCostSession writes one sessions row carrying a recorded API
// cost, through the production write path: SaveSession (the
// state-tracking hooks' upsert) + UpdateSessionCost (the statusline
// hook's API-pricing write).
func seedCostSession(t *testing.T, id, status string, cost float64) {
	t.Helper()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:          id,
		TmuxSession: "tmux-" + id,
		ConvID:      "conv-" + id,
		Cwd:         "/tmp/" + id,
		Status:      status,
	}), "SaveSession %s", id)
	if cost > 0 {
		require.NoError(t, db.UpdateSessionCost(id, cost), "UpdateSessionCost %s", id)
	}
}

// Scenario: an API/enterprise-billing account — agents accrue dollar
// cost (sessions.cost_usd via the statusline hook) but the usage API
// reports no rolling-limit windows, so the subscription readout is
// unavailable. The snapshot must still carry the month-to-date cost
// total so the dashboard top bar can show "$1.75 (mtd)" where
// "usage: n/a" would otherwise sit. Exited sessions keep their rows
// (nothing auto-prunes them), so a retired agent's cost stays in the
// sum.
func TestDashboardUsage_TotalCostSurfacedWithoutSubscription(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	newFlow(t)

	seedCostSession(t, "tcost-live", "idle", 1.25)
	seedCostSession(t, "tcost-retired", "exited", 0.50)
	seedCostSession(t, "tcost-sub", "idle", 0) // subscription session: contributes nothing

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	assert.False(t, snap.Usage.Available, "no subscription windows on an API-billing account")
	assert.InDelta(t, 1.75, snap.Usage.TotalCostUSD, 1e-9,
		"live + retired session costs summed on the snapshot")
	// UpdateSessionCost only ever writes today's session_cost_daily row,
	// so every dollar above was spent today — month-to-date and today
	// coincide here, and both must surface.
	assert.InDelta(t, 1.75, snap.Usage.TodayCostUSD, 1e-9,
		"today's cost surfaced alongside the month-to-date total")
}

// Scenario: spend straddles a day boundary — a session that spent on an
// earlier day and again today. The month-to-date headline must include
// the earlier spend while the "(today)" figure is windowed to just
// today's delta, so the top bar can show the two distinct figures. This
// is the bit production's UpdateSessionCost write path can't exercise
// (it only ever stamps today), so the earlier row is seeded directly.
func TestDashboardUsage_TodayCostWindowsToToday(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	newFlow(t)

	now := time.Now()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	d, err := db.Open()
	require.NoError(t, err, "open db")
	// One session, cumulative cost growing across two days: $1.00 by end
	// of yesterday, $1.60 by today — so today's spend is the $0.60 delta.
	for _, r := range []struct {
		day  string
		cost float64
	}{
		{yesterday, 1.00},
		{today, 1.60},
	} {
		_, err := d.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd)
			VALUES (?, ?, ?, ?)`, "straddle", r.day, "conv-straddle", r.cost)
		require.NoError(t, err, "seed cost row for %s", r.day)
	}

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	// Today is the $0.60 rise over yesterday's high-water mark —
	// calendar-robust (the windowed delta never reaches back past
	// yesterday) on every day, including the first of a month.
	assert.InDelta(t, 0.60, snap.Usage.TodayCostUSD, 1e-9,
		"today's cost is the delta over yesterday, not the cumulative total")
	assert.LessOrEqual(t, snap.Usage.TodayCostUSD, snap.Usage.TotalCostUSD,
		"today's cost can never exceed month-to-date")
}

// Scenario: both data sources present — fresh subscription windows in
// the usage cache AND recorded API cost on a session row (e.g. a mixed
// fleet, or a subscription account that ran an API-keyed agent). The
// snapshot carries both so the dashboard renders the cost token next
// to the 5h/7d bars.
func TestDashboardUsage_TotalCostAlongsideSubscription(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	newFlow(t)

	now := time.Now()
	seedUsageCache(t, usageapi.CachedUsage{
		FiveHour:      &usageapi.CachedBucket{Pct: 17, ResetsAt: now.Add(2 * time.Hour)},
		FetchedAt:     now,
		LastAttemptAt: now,
	})
	seedCostSession(t, "bcost-live", "idle", 0.42)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	require.True(t, snap.Usage.Available, "subscription windows available")
	require.NotNil(t, snap.Usage.FiveHour, "5h window present")
	assert.InDelta(t, 0.42, snap.Usage.TotalCostUSD, 1e-9, "cost total rides alongside the windows")
}
