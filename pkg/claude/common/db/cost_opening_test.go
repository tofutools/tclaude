package db

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func costTestDay(n int) string {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local).AddDate(0, 0, n).Format(costDayFormat)
}

// costTestNow is the moment whose opening day is costTestDay(n).
func costTestNow(n int) time.Time {
	return time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local).AddDate(0, 0, n+CostReadLimitDays-2)
}

// seedRandomCostLedger writes the histories that make the walk path-dependent:
// a /clear moving a session between conversations (carry), resumes across
// sessions (baseline), conversation-less rows, other harnesses and mixed billing.
func seedRandomCostLedger(t *testing.T, d *sql.DB, seed uint64, days int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 42))
	for s := range 40 {
		session := fmt.Sprintf("sess-%02d", s)
		cost := 0.0
		conv := fmt.Sprintf("conv-%02d", rng.IntN(14))
		harness := ""
		if s%7 == 6 {
			harness = "codex"
		}
		for n := rng.IntN(days / 2); n < days; n += 1 + rng.IntN(6) {
			cost += rng.Float64() * 4
			switch rng.IntN(6) {
			case 0:
				conv = fmt.Sprintf("conv-%02d", rng.IntN(14))
			case 1:
				cost = rng.Float64() // A fresh process counter.
			}
			rowConv := conv
			if s%10 == 9 {
				rowConv = ""
			}
			realCost, virtual := cost, 0.0
			switch s % 5 {
			case 4:
				realCost, virtual = 0, cost
			case 3:
				if n%2 == 0 {
					realCost, virtual = 0, cost
				}
			}
			_, err := d.Exec(`INSERT OR REPLACE INTO session_cost_daily
				(session_id, day, conv_id, cost_usd, virtual_cost_usd, updated_at, model, harness)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				session, costTestDay(n), rowConv, realCost, virtual,
				int64(n+1)*int64(time.Hour)+int64(s), fmt.Sprintf("model-%d", s%3), harness)
			require.NoError(t, err)
		}
	}
}

func costDeltasIn(deltas []CostDelta, from, to string) []CostDelta {
	out := []CostDelta{}
	for _, delta := range deltas {
		if delta.Day >= from && (to == "" || delta.Day <= to) {
			out = append(out, delta)
		}
	}
	return out
}

func keepCostRows(rows []CostDailyRow, keep func(*CostDailyRow) bool) []CostDailyRow {
	out := []CostDailyRow{}
	for i := range rows {
		if keep(&rows[i]) {
			out = append(out, rows[i])
		}
	}
	return out
}

func assertCostWindowMatchesFullWalk(t *testing.T, all, rows []CostDailyRow, from, to string) {
	t.Helper()
	assert.Equal(t, costDeltasIn(MixedCostDeltas(all), from, to), costDeltasIn(MixedCostDeltas(rows), from, to), "mixed from %s to %q", from, to)
	assert.Equal(t, costDeltasIn(CostDeltas(all, false), from, to), costDeltasIn(CostDeltas(rows, false), from, to), "real from %s to %q", from, to)
	assert.Equal(t, costDeltasIn(CostDeltas(all, true), from, to), costDeltasIn(CostDeltas(rows, true), from, to), "what-if from %s to %q", from, to)
}

// The randomized comparison against the full read: for every window start on
// either side of the opening, and with conversations ignored the way the Costs
// view splits them, the read equals walking the whole ledger.
func TestCostLedgerWindow_MatchesFullWalk(t *testing.T) {
	for _, seed := range []uint64{7, 8, 9} {
		setupTestDB(t)
		d, err := Open()
		require.NoError(t, err, "open db")
		const days, openingAt = 90, 45
		seedRandomCostLedger(t, d, seed, days)
		require.NoError(t, RefreshCostOpening(costTestNow(openingAt)))
		all, err := AllCostDailyRows()
		require.NoError(t, err)

		for start := 30; start < days+2; start += 3 {
			from := costTestDay(start)
			for _, to := range []string{"", costTestDay(start + 7)} {
				ledger, err := CostLedgerWindow(from, to, false)
				require.NoError(t, err)
				assert.False(t, ledger.NeedsRefresh)
				assert.Equal(t, start >= openingAt, ledger.OpeningDay != "", "opening serves windows from its day on")
				assertCostWindowMatchesFullWalk(t, all, ledger.Rows, from, to)

				ignored := func(r *CostDailyRow) bool { return r.ConvID == "conv-03" || r.ConvID == "conv-07" }
				counted := func(r *CostDailyRow) bool { return !ignored(r) }
				assertCostWindowMatchesFullWalk(t, keepCostRows(all, counted), keepCostRows(ledger.Rows, counted), from, to)
				assertCostWindowMatchesFullWalk(t, keepCostRows(all, ignored), keepCostRows(ledger.Rows, ignored), from, to)
			}
		}
	}
}

// Proof the default read leaves older rows alone: with the triggers out of the
// way, rewriting every older row into nonsense changes no result, and removing
// them changes none either. An older range still reads them.
func TestCostLedgerWindow_ReadsNoOlderRows(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "open db")
	const openingAt = 60
	seedRandomCostLedger(t, d, 11, 100)
	require.NoError(t, RefreshCostOpening(costTestNow(openingAt)))
	opening := costTestDay(openingAt)
	require.NoError(t, SetCostConvIgnored("conv-05", true))
	all, err := AllCostDailyRows()
	require.NoError(t, err)
	wantFirst, err := FirstCountedCostDay(true)
	require.NoError(t, err)
	require.Less(t, wantFirst, opening)

	for _, name := range []string{"insert", "delete", "update"} {
		_, err := d.Exec(`DROP TRIGGER cost_opening_stale_` + name)
		require.NoError(t, err)
	}
	check := func(label string) {
		ledger, err := CostLedgerWindow(opening, "", true)
		require.NoError(t, err, label)
		assert.Equal(t, opening, ledger.OpeningDay, label)
		for _, r := range ledger.Rows {
			assert.GreaterOrEqual(t, r.Day, opening, label)
		}
		assertCostWindowMatchesFullWalk(t, all, ledger.Rows, opening, "")
		first, err := FirstCountedCostDay(true)
		require.NoError(t, err, label)
		assert.Equal(t, wantFirst, first, label)
		hasReal, err := HasCountedRealCost()
		require.NoError(t, err, label)
		assert.True(t, hasReal, label)
	}
	check("as stored")
	_, err = d.Exec(`UPDATE session_cost_daily SET cost_usd = 99999, virtual_cost_usd = 99999, model = 'poison' WHERE day < ?`, opening)
	require.NoError(t, err)
	check("older rows poisoned")
	_, err = d.Exec(`DELETE FROM session_cost_daily WHERE day < ?`, opening)
	require.NoError(t, err)
	check("older rows gone")
}

// The queries a default read runs must search the day index: a filter over the
// walk index would still visit every older entry.
func TestCostLedgerWindow_QueryPlanSearchesTheDay(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "open db")
	for _, query := range []string{
		`SELECT ` + costDailyColumns + recentCostRowsFrom + ` ORDER BY ` + costWalkOrder,
		`SELECT 1` + recentCostRowsFrom + ` AND cost_usd > 0 AND ` + costNotIgnored,
	} {
		rows, err := d.Query(`EXPLAIN QUERY PLAN `+query, "2026-01-01")
		require.NoError(t, err)
		var plan []string
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail))
			plan = append(plan, detail)
		}
		require.NoError(t, rows.Close())
		assert.Contains(t, strings.Join(plan, "\n"), "SEARCH session_cost_daily USING INDEX idx_session_cost_daily_day (day>?)")
	}
}

func costOpeningStale(t *testing.T, d *sql.DB) bool {
	t.Helper()
	meta, err := readCostOpeningMeta(d)
	require.NoError(t, err)
	return meta.stale
}

// A change to an old row cannot leave a wrong total behind, whichever way it
// happens: the opening is marked stale in the same write, readers go back to
// the full read, and a refresh restores it.
func TestCostOpening_OlderRowChangesFallBack(t *testing.T) {
	const openingAt = 60
	opening := costTestDay(openingAt)
	for name, change := range map[string]string{
		"update":   `UPDATE session_cost_daily SET cost_usd = cost_usd + 5 WHERE rowid = (SELECT rowid FROM session_cost_daily WHERE day < ? AND cost_usd > 0 LIMIT 1)`,
		"delete":   `DELETE FROM session_cost_daily WHERE rowid = (SELECT rowid FROM session_cost_daily WHERE day < ? LIMIT 1)`,
		"insert":   `INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd) VALUES ('late', date(?, '-1 day'), 'conv-00', 77)`,
		"reattrib": `UPDATE session_cost_daily SET conv_id = 'conv-99' WHERE rowid = (SELECT rowid FROM session_cost_daily WHERE day < ? AND conv_id <> '' LIMIT 1)`,
	} {
		t.Run(name, func(t *testing.T) {
			setupTestDB(t)
			d, err := Open()
			require.NoError(t, err, "open db")
			seedRandomCostLedger(t, d, 13, 100)
			now := costTestNow(openingAt)
			require.NoError(t, RefreshCostOpening(now))
			assert.False(t, costOpeningStale(t, d))

			_, err = d.Exec(change, opening)
			require.NoError(t, err)
			assert.True(t, costOpeningStale(t, d))
			due, err := CostOpeningDue(now)
			require.NoError(t, err)
			assert.True(t, due)

			all, err := AllCostDailyRows()
			require.NoError(t, err)
			ledger, err := CostLedgerWindow(opening, "", false)
			require.NoError(t, err)
			assert.Empty(t, ledger.OpeningDay, "no stale opening behind a total")
			assert.True(t, ledger.NeedsRefresh)
			assertCostWindowMatchesFullWalk(t, all, ledger.Rows, opening, "")

			require.NoError(t, RefreshCostOpening(now))
			ledger, err = CostLedgerWindow(opening, "", false)
			require.NoError(t, err)
			assert.Equal(t, opening, ledger.OpeningDay)
			assertCostWindowMatchesFullWalk(t, all, ledger.Rows, opening, "")
		})
	}
}

// A row at or after the opening day changes nothing stored.
func TestCostOpening_RecentWritesLeaveItAlone(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "open db")
	const openingAt = 60
	seedRandomCostLedger(t, d, 17, 100)
	require.NoError(t, RefreshCostOpening(costTestNow(openingAt)))
	_, err = d.Exec(`UPDATE session_cost_daily SET cost_usd = cost_usd + 1 WHERE day >= ?`, costTestDay(openingAt))
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd) VALUES ('new', ?, 'conv-new', 3)`, costTestDay(openingAt))
	require.NoError(t, err)
	assert.False(t, costOpeningStale(t, d))
}

// History followers zero a conversation and write it back on every flush. An
// identical rewrite must not spend the opening; a real correction must.
func TestCostOpening_IdenticalHistoryRewriteKeepsIt(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "open db")
	require.NoError(t, SaveSession(&SessionRow{ID: "sess-h", TmuxSession: "tmux-h", ConvID: "conv-h", Harness: "codex",
		Cwd: "/tmp", Status: "idle"}))
	observed := costTestNow(0)
	snapshots := []VirtualCostDailySnapshot{
		{Day: costTestDay(10), CostUSD: 1, UpdatedAt: observed, Model: "m"},
		{Day: costTestDay(70), CostUSD: 2, UpdatedAt: observed, Model: "m"},
	}
	require.NoError(t, ReplaceSessionVirtualCostHistory("sess-h", 2, snapshots))
	require.NoError(t, RefreshCostOpening(costTestNow(60)))

	require.NoError(t, ReplaceSessionVirtualCostHistory("sess-h", 2, snapshots))
	assert.False(t, costOpeningStale(t, d), "an identical rewrite keeps the opening")

	snapshots[0].CostUSD = 1.5
	require.NoError(t, ReplaceSessionVirtualCostHistory("sess-h", 2, snapshots))
	assert.True(t, costOpeningStale(t, d), "a corrected old day spends it")
}

// A database with history that has never been refreshed, an upgrade's first
// start, reads exactly and asks for a refresh.
func TestCostOpening_UpgradeBeforeFirstRefresh(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "open db")
	seedRandomCostLedger(t, d, 19, 100)
	all, err := AllCostDailyRows()
	require.NoError(t, err)
	from := costTestDay(70)

	ledger, err := CostLedgerWindow(from, "", false)
	require.NoError(t, err)
	assert.True(t, ledger.NeedsRefresh)
	assert.Empty(t, ledger.OpeningDay)
	assertCostWindowMatchesFullWalk(t, all, ledger.Rows, from, "")
	first, err := FirstCountedCostDay(false)
	require.NoError(t, err)
	assert.NotEmpty(t, first)
}

// Stored state in any broken shape degrades to the full read, never to a wrong
// total or a failed request.
func TestCostOpening_BrokenStateFallsBack(t *testing.T) {
	const openingAt = 60
	opening := costTestDay(openingAt)
	for name, breakIt := range map[string]string{
		"no meta row":      `DELETE FROM cost_opening_meta`,
		"no meta table":    `DROP TABLE cost_opening_meta`,
		"no conv table":    `DROP TABLE cost_opening_convs`,
		"no session table": `DROP TABLE cost_opening_sessions`,
		"no spend table":   `DROP TABLE cost_opening_spend`,
		"corrupt conv":     `UPDATE cost_opening_convs SET state = 'not json'`,
		"corrupt session":  `UPDATE cost_opening_sessions SET state = 'not json'`,
	} {
		t.Run(name, func(t *testing.T) {
			setupTestDB(t)
			d, err := Open()
			require.NoError(t, err, "open db")
			seedRandomCostLedger(t, d, 23, 100)
			require.NoError(t, RefreshCostOpening(costTestNow(openingAt)))
			all, err := AllCostDailyRows()
			require.NoError(t, err)
			wantFirst, err := FirstCountedCostDay(true)
			require.NoError(t, err)
			if name == "no meta table" {
				for _, trigger := range []string{"insert", "delete", "update"} {
					_, err := d.Exec(`DROP TRIGGER cost_opening_stale_` + trigger)
					require.NoError(t, err)
				}
			}
			_, err = d.Exec(breakIt)
			require.NoError(t, err)

			ledger, err := CostLedgerWindow(opening, "", true)
			require.NoError(t, err)
			if name != "no spend table" { // Only the all-history facts below read it.
				assert.Empty(t, ledger.OpeningDay)
				assert.True(t, ledger.NeedsRefresh)
			}
			assertCostWindowMatchesFullWalk(t, all, ledger.Rows, opening, "")
			first, err := FirstCountedCostDay(true)
			require.NoError(t, err)
			assert.Equal(t, wantFirst, first)
			hasReal, err := HasCountedRealCost()
			require.NoError(t, err)
			assert.True(t, hasReal)
			if strings.HasPrefix(name, "corrupt") {
				due, err := CostOpeningDue(costTestNow(openingAt))
				require.NoError(t, err)
				assert.True(t, due, "an unreadable opening asks to be rebuilt")
				require.NoError(t, RefreshCostOpening(costTestNow(openingAt)))
				ledger, err := CostLedgerWindow(opening, "", true)
				require.NoError(t, err)
				assert.Equal(t, opening, ledger.OpeningDay, "and the rebuild restores it")
				assertCostWindowMatchesFullWalk(t, all, ledger.Rows, opening, "")
			}
		})
	}
}

// Tampering nothing marks is what the audit is for: the refresh replaces it and
// the totals come back exact.
func TestCostOpening_RefreshReplacesWrongState(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "open db")
	const openingAt = 60
	seedRandomCostLedger(t, d, 29, 100)
	require.NoError(t, RefreshCostOpening(costTestNow(openingAt)))
	_, err = d.Exec(`UPDATE cost_opening_convs SET state = '{"w":[{"b":99999},{"b":99999},{"b":99999}]}'`)
	require.NoError(t, err)
	require.NoError(t, RefreshCostOpening(costTestNow(openingAt)))

	all, err := AllCostDailyRows()
	require.NoError(t, err)
	opening := costTestDay(openingAt)
	ledger, err := CostLedgerWindow(opening, "", false)
	require.NoError(t, err)
	assert.Equal(t, opening, ledger.OpeningDay)
	assertCostWindowMatchesFullWalk(t, all, ledger.Rows, opening, "")
}

// The opening rolls: refreshed on each of a run of days it stays exact, inside
// the limit, and still inside it the next morning before the refresh runs.
func TestCostOpening_RollsWithTheLimit(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "open db")
	seedRandomCostLedger(t, d, 31, 110)
	all, err := AllCostDailyRows()
	require.NoError(t, err)
	for n := 40; n < 75; n++ {
		now := costTestNow(n)
		require.NoError(t, RefreshCostOpening(now))
		due, err := CostOpeningDue(now)
		require.NoError(t, err)
		assert.False(t, due)

		tomorrow := now.AddDate(0, 0, 1)
		limit := tomorrow.AddDate(0, 0, -(CostReadLimitDays - 1)).Format(costDayFormat)
		monthStart := time.Date(tomorrow.Year(), tomorrow.Month(), 1, 0, 0, 0, 0, tomorrow.Location()).Format(costDayFormat)
		ledger, err := CostLedgerWindow(monthStart, "", false)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, ledger.OpeningDay, limit, "tomorrow's first poll stays inside the limit")
		assertCostWindowMatchesFullWalk(t, all, ledger.Rows, monthStart, "")
	}
}

func TestCostOpeningDay_CoversTheLongestDefaultPeriod(t *testing.T) {
	require.Greater(t, CostReadLimitDays, 31+1, "a month to date plus the day of margin must fit the limit")
}
