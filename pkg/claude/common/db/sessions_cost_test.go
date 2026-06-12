package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// saveCostSession seeds one sessions row with a recorded API cost,
// through the same write path production uses (SaveSession upsert +
// the statusline hook's UpdateSessionCost).
func saveCostSession(t *testing.T, id, status string, cost float64) {
	t.Helper()
	require.NoError(t, SaveSession(&SessionRow{
		ID:          id,
		TmuxSession: "tmux-" + id,
		ConvID:      "conv-" + id,
		Cwd:         "/tmp/" + id,
		Status:      status,
	}), "SaveSession %s", id)
	if cost > 0 {
		require.NoError(t, UpdateSessionCost(id, cost), "UpdateSessionCost %s", id)
	}
}

// dailyRowFor pulls one session's daily snapshot for a given day, or
// nil when absent.
func dailyRowFor(t *testing.T, rows []CostDailyRow, sessionID, day string) *CostDailyRow {
	t.Helper()
	for i := range rows {
		if rows[i].SessionID == sessionID && rows[i].Day == day {
			return &rows[i]
		}
	}
	return nil
}

// TestUpdateSessionCost_WritesDailySnapshot pins the statusline hook's
// sibling write: recording a session's cumulative cost also upserts
// today's session_cost_daily row — monotonic within the day (a stale,
// lower render never lowers it) and carrying the conv_id denormalised
// off the sessions row.
func TestUpdateSessionCost_WritesDailySnapshot(t *testing.T) {
	setupTestDB(t)
	today := time.Now().Format(costDayFormat)

	saveCostSession(t, "dcost-a", "idle", 1.00)
	require.NoError(t, UpdateSessionCost("dcost-a", 1.50), "second tick, higher cumulative")
	require.NoError(t, UpdateSessionCost("dcost-a", 1.25), "stale lower render must not lower the snapshot")

	rows, err := AllCostDailyRows()
	require.NoError(t, err, "AllCostDailyRows")
	row := dailyRowFor(t, rows, "dcost-a", today)
	require.NotNil(t, row, "today's daily row exists")
	assert.InDelta(t, 1.50, row.CostUSD, 1e-9, "daily snapshot keeps the day's maximum")
	assert.Equal(t, "conv-dcost-a", row.ConvID, "conv_id denormalised from the sessions row")
}

// TestUpdateSessionCost_StampsUpdatedAt pins the daily row's
// last-activity clock: the first spend stamps updated_at; a stale,
// lower render (which never raises the day's max) must leave the stamp
// alone — an idle session whose statusline keeps ticking does not read
// as fresh activity; a higher cumulative figure (real new spend) does
// refresh it.
func TestUpdateSessionCost_StampsUpdatedAt(t *testing.T) {
	setupTestDB(t)
	today := time.Now().Format(costDayFormat)

	saveCostSession(t, "ts-a", "idle", 1.00)
	rows, err := AllCostDailyRows()
	require.NoError(t, err)
	row := dailyRowFor(t, rows, "ts-a", today)
	require.NotNil(t, row)
	first := row.UpdatedAt
	require.NotEmpty(t, first, "first spend stamps updated_at")

	require.NoError(t, UpdateSessionCost("ts-a", 0.50), "stale lower render")
	rows, err = AllCostDailyRows()
	require.NoError(t, err)
	assert.Equal(t, first, dailyRowFor(t, rows, "ts-a", today).UpdatedAt,
		"a render that does not raise the day's spend must not bump the stamp")

	require.NoError(t, UpdateSessionCost("ts-a", 2.00), "real new spend")
	rows, err = AllCostDailyRows()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, dailyRowFor(t, rows, "ts-a", today).UpdatedAt, first,
		"a higher cumulative figure refreshes the stamp")
}

// TestCostDaily_SurvivesSessionDeletion is the retired-agent
// guarantee: deleting the sessions row (session kill, agent delete)
// must leave the daily cost history — and its conv attribution —
// intact for the Costs tab.
func TestCostDaily_SurvivesSessionDeletion(t *testing.T) {
	setupTestDB(t)
	today := time.Now().Format(costDayFormat)

	saveCostSession(t, "dcost-gone", "exited", 2.25)
	require.NoError(t, DeleteSession("dcost-gone"), "DeleteSession")

	rows, err := AllCostDailyRows()
	require.NoError(t, err, "AllCostDailyRows")
	row := dailyRowFor(t, rows, "dcost-gone", today)
	require.NotNil(t, row, "daily row survives the sessions row's deletion")
	assert.InDelta(t, 2.25, row.CostUSD, 1e-9, "cost preserved")
	assert.Equal(t, "conv-dcost-gone", row.ConvID, "conv attribution preserved")
}

// TestUpdateSessionCost_UnknownSessionWritesNothing pins the
// INSERT…SELECT guard: a cost write keyed to a session id with no
// sessions row (a stale hook, a pruned session) must not mint an
// orphan, attributionless daily row.
func TestUpdateSessionCost_UnknownSessionWritesNothing(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, UpdateSessionCost("ghost", 1.23), "UpdateSessionCost on unknown session")

	rows, err := AllCostDailyRows()
	require.NoError(t, err, "AllCostDailyRows")
	assert.Empty(t, rows, "no daily row for a session that doesn't exist")
}

// TestSumCostSinceDay pins the DB-side windowed aggregate (the top
// bar's month-to-date read) to the SAME fixture and totals as agentd's
// TestCostDeltasFromRows — the SQL closed form (windowed peak minus
// prior high-water, clamped at zero) and the Go day-by-day delta walk
// must agree, or the top-bar headline drifts from the Costs tab.
func TestSumCostSinceDay(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "open db")
	for _, r := range []CostDailyRow{
		// Session a (conv-1): plain growth across three days.
		{SessionID: "a", Day: "2026-06-01", ConvID: "conv-1", CostUSD: 1.00},
		{SessionID: "a", Day: "2026-06-02", ConvID: "conv-1", CostUSD: 1.00}, // ticked, spent nothing
		{SessionID: "a", Day: "2026-06-03", ConvID: "conv-1", CostUSD: 2.50},
		// Session b (conv-1, e.g. a reincarnation): cumulative restarts at 0.
		{SessionID: "b", Day: "2026-06-03", ConvID: "conv-1", CostUSD: 0.40},
		// Session c (conv-2): dips after /clear, then recovers past the max.
		{SessionID: "c", Day: "2026-06-01", ConvID: "conv-2", CostUSD: 3.00},
		{SessionID: "c", Day: "2026-06-02", ConvID: "conv-2", CostUSD: 1.00}, // dip — never negative
		{SessionID: "c", Day: "2026-06-03", ConvID: "conv-2", CostUSD: 3.50}, // only the rise above 3.00 counts
	} {
		_, err := d.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd)
			VALUES (?, ?, ?, ?)`, r.SessionID, r.Day, r.ConvID, r.CostUSD)
		require.NoError(t, err, "seed %s/%s", r.SessionID, r.Day)
	}

	total, err := SumCostSinceDay("2026-06-01")
	require.NoError(t, err)
	assert.InDelta(t, 6.40, total, 1e-9, "whole history — matches sumCostDeltas unbounded")

	total, err = SumCostSinceDay("2026-06-02")
	require.NoError(t, err)
	assert.InDelta(t, 2.40, total, 1e-9,
		"windowed: a grows 1.50 over its 06-01 baseline, b starts at 0.40, c's dip clamps to its 0.50 recovery")

	total, err = SumCostSinceDay("2026-06-04")
	require.NoError(t, err)
	assert.Zero(t, total, "no snapshots in window sums to 0")
}

// TestSumCostSinceDay_EmptyDB pins the zero state: no rows at all must
// read back 0, not a NULL scan failure — this is every
// subscription-only install on the 2s snapshot tick.
func TestSumCostSinceDay_EmptyDB(t *testing.T) {
	setupTestDB(t)
	total, err := SumCostSinceDay("2026-06-01")
	require.NoError(t, err)
	assert.Zero(t, total)
}

// TestAllCostDailyRows_OrderAndEmpty pins the (session_id, day)
// ordering the delta aggregation depends on, and the clean empty
// state (a subscription-only install has no daily rows at all).
func TestAllCostDailyRows_OrderAndEmpty(t *testing.T) {
	setupTestDB(t)

	rows, err := AllCostDailyRows()
	require.NoError(t, err, "AllCostDailyRows on empty DB")
	assert.Empty(t, rows, "no rows on a fresh DB")

	// Seed out of order across two sessions and three days via direct
	// SQL — only the day key matters, and UpdateSessionCost can only
	// ever write "today".
	d, err := Open()
	require.NoError(t, err, "open db")
	for _, r := range []CostDailyRow{
		{SessionID: "s2", Day: "2026-06-02", ConvID: "c2", CostUSD: 1},
		{SessionID: "s1", Day: "2026-06-03", ConvID: "c1", CostUSD: 3},
		{SessionID: "s1", Day: "2026-06-01", ConvID: "c1", CostUSD: 2},
	} {
		_, err := d.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd)
			VALUES (?, ?, ?, ?)`, r.SessionID, r.Day, r.ConvID, r.CostUSD)
		require.NoError(t, err, "seed %s/%s", r.SessionID, r.Day)
	}

	rows, err = AllCostDailyRows()
	require.NoError(t, err, "AllCostDailyRows")
	require.Len(t, rows, 3)
	assert.Equal(t, []string{"s1", "s1", "s2"}, []string{rows[0].SessionID, rows[1].SessionID, rows[2].SessionID}, "session order")
	assert.Equal(t, []string{"2026-06-01", "2026-06-03", "2026-06-02"}, []string{rows[0].Day, rows[1].Day, rows[2].Day}, "day order within session")
}
