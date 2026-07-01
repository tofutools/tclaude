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

// parseStamp parses a stored RFC3339(Nano) updated_at into a time.Time.
// updated_at is stored with variable sub-second precision (trailing zeros
// are trimmed), so two stamps must be compared AS TIME — a lexical string
// compare is wrong across the precision boundary (e.g. ".978Z" sorts
// after ".978494Z" even though it is the earlier instant).
func parseStamp(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, err, "parse timestamp %q", s)
	return ts
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
	bumped := dailyRowFor(t, rows, "ts-a", today).UpdatedAt
	assert.False(t, parseStamp(t, bumped).Before(parseStamp(t, first)),
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

// TestUpdateSessionVirtualCost_WritesVirtualColumnOnly is the WHAT-IF
// sibling of TestUpdateSessionCost_WritesDailySnapshot: a subscription
// session's virtual cost lands on virtual_cost_usd (monotonic within the
// day, conv_id denormalised) and leaves the real cost_usd at 0, so the two
// figures never conflate.
func TestUpdateSessionVirtualCost_WritesVirtualColumnOnly(t *testing.T) {
	setupTestDB(t)
	today := time.Now().Format(costDayFormat)

	require.NoError(t, SaveSession(&SessionRow{
		ID: "vc-a", TmuxSession: "tmux-vc-a", ConvID: "conv-vc-a", Cwd: "/tmp/vc-a", Status: "idle",
	}), "SaveSession")
	require.NoError(t, UpdateSessionVirtualCost("vc-a", 1.00), "first virtual tick")
	require.NoError(t, UpdateSessionVirtualCost("vc-a", 1.50), "higher cumulative")
	require.NoError(t, UpdateSessionVirtualCost("vc-a", 1.25), "stale lower render must not lower the snapshot")

	rows, err := AllCostDailyRows()
	require.NoError(t, err, "AllCostDailyRows")
	row := dailyRowFor(t, rows, "vc-a", today)
	require.NotNil(t, row, "today's daily row exists")
	assert.InDelta(t, 1.50, row.VirtualCostUSD, 1e-9, "daily snapshot keeps the day's maximum virtual cost")
	assert.Zero(t, row.CostUSD, "real cost_usd stays 0 for a subscription session")
	assert.Equal(t, "conv-vc-a", row.ConvID, "conv_id denormalised from the sessions row")

	// And the real-cost surfaces ignore it: a subscription account still
	// reads as having no real spend.
	has, err := HasAnyRealCost()
	require.NoError(t, err, "HasAnyRealCost")
	assert.False(t, has, "virtual-only spend is not real cost")
	total, err := SumCostSinceDay(today)
	require.NoError(t, err, "SumCostSinceDay")
	assert.Zero(t, total, "month-to-date real spend ignores virtual cost")
}

// TestUpdateSessionCost_PrefersPersistedAgentID pins JOH-288: the daily
// snapshot's agent_id is taken from the session's PERSISTED agent_id column
// first, falling back to the live agent_conversations lookup only when that
// column is empty. A /clear or clone can move the conv's actor mapping out from
// under the live lookup while the persisted column still names the owning actor
// — the snapshot must keep that attribution rather than blanking it.
func TestUpdateSessionCost_PrefersPersistedAgentID(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "open db")

	// A session whose conv has NO agent_conversations mapping (the live lookup
	// yields ''), but whose persisted agent_id names the owning actor.
	require.NoError(t, SaveSession(&SessionRow{
		ID: "j288", TmuxSession: "tmux-j288", ConvID: "conv-j288", Cwd: "/tmp/j288", Status: "idle",
	}), "SaveSession")
	_, err = d.Exec(`UPDATE sessions SET agent_id = 'agt_persisted' WHERE id = 'j288'`)
	require.NoError(t, err, "stamp persisted agent_id")

	require.NoError(t, UpdateSessionCost("j288", 1.00), "UpdateSessionCost")

	var agentID string
	require.NoError(t, d.QueryRow(
		`SELECT agent_id FROM session_cost_daily WHERE session_id = 'j288'`).Scan(&agentID),
		"read daily agent_id")
	assert.Equal(t, "agt_persisted", agentID,
		"daily snapshot prefers the session's persisted agent_id over the empty conv lookup")
}

// TestUpdateSessionCost_FallsBackToConvLookupForAgentID is the other half of
// JOH-288: when the session carries no persisted agent_id, the snapshot still
// derives it from agent_conversations, preserving the pre-fix behaviour.
func TestUpdateSessionCost_FallsBackToConvLookupForAgentID(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "open db")

	// Map the conv to an actor, then blank the session's persisted column so the
	// only path to a non-empty agent_id is the agent_conversations fallback.
	agentID, _, err := EnsureAgentForConv("conv-j288b", "test")
	require.NoError(t, err, "EnsureAgentForConv")
	require.NoError(t, SaveSession(&SessionRow{
		ID: "j288b", TmuxSession: "tmux-j288b", ConvID: "conv-j288b", Cwd: "/tmp/j288b", Status: "idle",
	}), "SaveSession")
	_, err = d.Exec(`UPDATE sessions SET agent_id = '' WHERE id = 'j288b'`)
	require.NoError(t, err, "blank persisted agent_id")

	require.NoError(t, UpdateSessionCost("j288b", 1.00), "UpdateSessionCost")

	var got string
	require.NoError(t, d.QueryRow(
		`SELECT agent_id FROM session_cost_daily WHERE session_id = 'j288b'`).Scan(&got),
		"read daily agent_id")
	assert.Equal(t, agentID, got,
		"falls back to the agent_conversations lookup when the persisted column is empty")
}

// TestUpdateSessionVirtualCost_UnknownSessionWritesNothing mirrors the
// real-cost INSERT…SELECT guard: a virtual cost write keyed to a session id
// with no sessions row must not mint an orphan daily row.
func TestUpdateSessionVirtualCost_UnknownSessionWritesNothing(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, UpdateSessionVirtualCost("ghost", 1.23), "UpdateSessionVirtualCost on unknown session")
	rows, err := AllCostDailyRows()
	require.NoError(t, err, "AllCostDailyRows")
	assert.Empty(t, rows, "no daily row for a session that doesn't exist")
}

// TestHasAnyRealCost pins the Costs-tab auto-hide signal: an empty DB and a
// virtual-only (subscription) account both read false; any real pay-per-token
// spend flips it true.
func TestHasAnyRealCost(t *testing.T) {
	setupTestDB(t)

	has, err := HasAnyRealCost()
	require.NoError(t, err, "HasAnyRealCost on empty DB")
	assert.False(t, has, "fresh DB has no real cost")

	// A subscription session records only virtual cost → still no real cost.
	require.NoError(t, SaveSession(&SessionRow{
		ID: "sub-1", TmuxSession: "tmux-sub-1", ConvID: "conv-sub-1", Cwd: "/tmp/sub-1", Status: "idle",
	}), "SaveSession sub")
	require.NoError(t, UpdateSessionVirtualCost("sub-1", 5.00), "virtual spend")
	has, err = HasAnyRealCost()
	require.NoError(t, err)
	assert.False(t, has, "virtual-only account reads as no real cost")

	// A pay-per-token session records real cost → flips true.
	saveCostSession(t, "ppt-1", "idle", 0.01)
	has, err = HasAnyRealCost()
	require.NoError(t, err)
	assert.True(t, has, "any real pay-per-token spend flips the signal true")
}

// TestSumCostSinceDay pins the DB-side windowed aggregate (the top
// bar's month-to-date read) to the SAME fixture and totals as agentd's
// TestCostDeltasFromRows — the SQL closed form (windowed peak minus
// prior high-water, clamped at zero) and the Go day-by-day delta walk
// must agree, or the top-bar headline drifts from the Costs tab. The
// aggregate groups PER CONVERSATION, so a conv resumed under a new
// session id is not double-counted.
func TestSumCostSinceDay(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "open db")
	for _, r := range []CostDailyRow{
		// conv-1: grows under session a1, resumed the NEXT day under a new
		// session whose cumulative carries forward (includes the 2.50).
		{SessionID: "sess-a1", Day: "2026-06-01", ConvID: "conv-1", CostUSD: 1.00},
		{SessionID: "sess-a1", Day: "2026-06-02", ConvID: "conv-1", CostUSD: 1.00}, // ticked, spent nothing
		{SessionID: "sess-a1", Day: "2026-06-03", ConvID: "conv-1", CostUSD: 2.50},
		{SessionID: "sess-a2", Day: "2026-06-04", ConvID: "conv-1", CostUSD: 4.00}, // resume — cumulative continues
		// conv-2: resumed twice the SAME day under a new session.
		{SessionID: "sess-b1", Day: "2026-06-03", ConvID: "conv-2", CostUSD: 2.00},
		{SessionID: "sess-b2", Day: "2026-06-03", ConvID: "conv-2", CostUSD: 5.00},
		// conv-3: dips, then recovers past the max.
		{SessionID: "sess-c1", Day: "2026-06-01", ConvID: "conv-3", CostUSD: 3.00},
		{SessionID: "sess-c1", Day: "2026-06-02", ConvID: "conv-3", CostUSD: 1.00}, // dip — never negative
		{SessionID: "sess-c1", Day: "2026-06-03", ConvID: "conv-3", CostUSD: 3.50}, // only the rise above 3.00 counts
	} {
		_, err := d.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd)
			VALUES (?, ?, ?, ?)`, r.SessionID, r.Day, r.ConvID, r.CostUSD)
		require.NoError(t, err, "seed %s/%s", r.SessionID, r.Day)
	}

	total, err := SumCostSinceDay("2026-06-01")
	require.NoError(t, err)
	assert.InDelta(t, 12.50, total, 1e-9, "whole history — matches sumCostDeltas unbounded")

	total, err = SumCostSinceDay("2026-06-03")
	require.NoError(t, err)
	assert.InDelta(t, 8.50, total, 1e-9,
		"windowed: conv-1 rises 1.50+1.50, conv-2's same-day resume counts 5.00 once, conv-3 clamps to 0.50")

	total, err = SumCostSinceDay("2026-06-04")
	require.NoError(t, err)
	assert.InDelta(t, 1.50, total, 1e-9,
		"conv-1's resume day counts only its rise over the prior high-water (4.00 - 2.50)")

	total, err = SumCostSinceDay("2026-06-05")
	require.NoError(t, err)
	assert.Zero(t, total, "no snapshots in window sums to 0")
}

// TestSumCostSinceDay_SessionResetAcrossExit pins the cross-month bug fix
// on the top-bar surface, to the SAME fixture and totals as agentd's
// TestCostDeltasFromRows_SessionResetAcrossExit — a conversation whose
// spawn session peaked at 8.42 one day, exited, then resumed the next day
// under a new session whose per-session counter started fresh at 2.28.
// Both independent counters must sum; a window opening on the resume day
// must still see the 2.28 rather than clamping it to the prior 8.42 peak
// (which had the agent vanishing from the current month while still
// showing in the previous).
func TestSumCostSinceDay_SessionResetAcrossExit(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "open db")
	for _, r := range []CostDailyRow{
		{SessionID: "spwn-x", Day: "2026-06-30", ConvID: "conv-r", CostUSD: 8.42},
		{SessionID: "conv-r", Day: "2026-07-01", ConvID: "conv-r", CostUSD: 2.28},
	} {
		_, err := d.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd)
			VALUES (?, ?, ?, ?)`, r.SessionID, r.Day, r.ConvID, r.CostUSD)
		require.NoError(t, err, "seed %s/%s", r.SessionID, r.Day)
	}

	total, err := SumCostSinceDay("2026-06-30")
	require.NoError(t, err)
	assert.InDelta(t, 10.70, total, 1e-9, "both independent per-session counters sum (8.42 + 2.28)")

	total, err = SumCostSinceDay("2026-07-01")
	require.NoError(t, err)
	assert.InDelta(t, 2.28, total, 1e-9,
		"a window opening on the resume day sees the resumed session's spend, not 0")
}

// TestCostDeltas pins the canonical delta walk both cost surfaces share
// (the Costs tab via agentd's costDeltasFromRows, the top bar via
// SumCostSinceDay): a carry-forward resume telescopes to the rise, a
// same-session dip-and-recover never goes negative, and a resume under a
// new session whose counter drops below the conversation's peak restarts
// the baseline so its spend is counted rather than swallowed.
func TestCostDeltas(t *testing.T) {
	rows := []CostDailyRow{
		// conv-1: grows, then a carry-forward resume under a new session
		// (higher cumulative) — counts only the rise.
		{SessionID: "s-a1", Day: "2026-06-01", ConvID: "conv-1", CostUSD: 1.00},
		{SessionID: "s-a1", Day: "2026-06-02", ConvID: "conv-1", CostUSD: 2.50},
		{SessionID: "s-a2", Day: "2026-06-03", ConvID: "conv-1", CostUSD: 4.00}, // carry-forward
		// conv-2: same session dips (stale render) then recovers — no reset,
		// only the rise above the running peak counts.
		{SessionID: "s-b1", Day: "2026-06-01", ConvID: "conv-2", CostUSD: 3.00},
		{SessionID: "s-b1", Day: "2026-06-02", ConvID: "conv-2", CostUSD: 1.00}, // dip within a session
		{SessionID: "s-b1", Day: "2026-06-03", ConvID: "conv-2", CostUSD: 3.50},
		// conv-3: resume-after-exit under a new session with a FRESH lower
		// counter — the baseline resets, so both counters are counted.
		{SessionID: "spwn-c", Day: "2026-06-30", ConvID: "conv-3", CostUSD: 8.42},
		{SessionID: "conv-3", Day: "2026-07-01", ConvID: "conv-3", CostUSD: 2.28},
	}

	byDayConv := map[string]float64{}
	for _, d := range CostDeltas(rows, false) {
		byDayConv[d.Day+"/"+d.ConvID] += d.USD
	}
	assert.InDelta(t, 1.00, byDayConv["2026-06-01/conv-1"], 1e-9, "first day carries the cumulative")
	assert.InDelta(t, 1.50, byDayConv["2026-06-02/conv-1"], 1e-9, "growth within a session")
	assert.InDelta(t, 1.50, byDayConv["2026-06-03/conv-1"], 1e-9, "carry-forward counts only the rise (4.00-2.50)")
	assert.InDelta(t, 3.00, byDayConv["2026-06-01/conv-2"], 1e-9)
	assert.NotContains(t, byDayConv, "2026-06-02/conv-2", "the same-session dip produces no delta")
	assert.InDelta(t, 0.50, byDayConv["2026-06-03/conv-2"], 1e-9, "only the rise above the running peak")
	assert.InDelta(t, 8.42, byDayConv["2026-06-30/conv-3"], 1e-9, "first session's peak")
	assert.InDelta(t, 2.28, byDayConv["2026-07-01/conv-3"], 1e-9,
		"resumed session's fresh counter is counted, not clamped to 8.42")
}

// TestCostDeltas_WhatIf pins the column switch: whatif reads
// virtual_cost_usd and the real walk sees nothing on the same
// subscription rows, and the session-reset logic applies to the virtual
// column identically.
func TestCostDeltas_WhatIf(t *testing.T) {
	rows := []CostDailyRow{
		{SessionID: "spwn-v", Day: "2026-06-30", ConvID: "conv-v", VirtualCostUSD: 6.00},
		{SessionID: "conv-v", Day: "2026-07-01", ConvID: "conv-v", VirtualCostUSD: 1.50}, // fresh counter
	}
	var whatif float64
	for _, d := range CostDeltas(rows, true) {
		whatif += d.USD
	}
	assert.InDelta(t, 7.50, whatif, 1e-9, "virtual counters reset the same way (6.00 + 1.50)")
	assert.Empty(t, CostDeltas(rows, false), "real walk sees nothing — these are subscription rows")
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

// TestAllCostDailyRows_OrderAndEmpty pins the (conv-key, day, session_id)
// ordering the delta aggregation depends on — all of a conversation's
// sessions grouped together so the per-day delta walk can carry one
// high-water baseline across a resume — and the clean empty state (a
// subscription-only install has no daily rows at all).
func TestAllCostDailyRows_OrderAndEmpty(t *testing.T) {
	setupTestDB(t)

	rows, err := AllCostDailyRows()
	require.NoError(t, err, "AllCostDailyRows on empty DB")
	assert.Empty(t, rows, "no rows on a fresh DB")

	// Seed out of order: one conversation (c1) spread across two sessions
	// and two days, plus a second conversation (c2). Direct SQL — only the
	// day key matters, and UpdateSessionCost can only ever write "today".
	d, err := Open()
	require.NoError(t, err, "open db")
	for _, r := range []CostDailyRow{
		{SessionID: "s2", Day: "2026-06-02", ConvID: "c2", CostUSD: 1},
		{SessionID: "s1", Day: "2026-06-03", ConvID: "c1", CostUSD: 3}, // c1, later day
		{SessionID: "s3", Day: "2026-06-01", ConvID: "c1", CostUSD: 2}, // c1, earlier day, DIFFERENT session
	} {
		_, err := d.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd)
			VALUES (?, ?, ?, ?)`, r.SessionID, r.Day, r.ConvID, r.CostUSD)
		require.NoError(t, err, "seed %s/%s", r.SessionID, r.Day)
	}

	rows, err = AllCostDailyRows()
	require.NoError(t, err, "AllCostDailyRows")
	require.Len(t, rows, 3)
	assert.Equal(t, []string{"c1", "c1", "c2"}, []string{rows[0].ConvID, rows[1].ConvID, rows[2].ConvID},
		"a conversation's rows group together regardless of session id")
	assert.Equal(t, []string{"2026-06-01", "2026-06-03", "2026-06-02"}, []string{rows[0].Day, rows[1].Day, rows[2].Day},
		"day ascending within a conversation")
	assert.Equal(t, []string{"s3", "s1", "s2"}, []string{rows[0].SessionID, rows[1].SessionID, rows[2].SessionID},
		"c1's two sessions stay adjacent, ordered by day not session id")
}
