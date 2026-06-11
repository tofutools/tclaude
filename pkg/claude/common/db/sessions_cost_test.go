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
