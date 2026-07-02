package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestCostDeltasFromRows covers the cumulative→delta recovery that every
// cost surface (top bar, /api/costs) is built on. The high-water
// baseline is carried PER CONVERSATION, not per session: Claude Code's
// total_cost_usd is cumulative across the whole conversation and persists
// across resume, so when a conversation is resumed under a new tclaude
// session — a new day, or twice the same day — that session's snapshot
// already includes the prior spend and only the genuine rise must count.
// The fixture pins:
//   - conv-1: resumed the NEXT day under a new session — the resume day
//     counts only the rise (4.00 − 2.50), not the whole cumulative (the
//     multi-day double-count this fix removes);
//   - conv-2: resumed twice the SAME day under a new session — the two
//     snapshots collapse to one day's spend, not their sum (the same-day
//     double-count);
//   - conv-3: a dip-and-recover never produces a negative day and counts
//     only the rise above the high-water mark;
//   - a conversation's first snapshot carries its whole cumulative (the
//     v51 backfill case), and a flat/dip day produces no delta.
//
// The fixture and its totals are shared verbatim with the db package's
// TestSumCostSinceDay — the SQL closed form behind the top bar must
// agree with this walk, or the headline drifts from the Costs tab.
func TestCostDeltasFromRows(t *testing.T) {
	rows := []db.CostDailyRow{
		// conv-1: plain growth under session a1, then resumed the NEXT day
		// under a NEW session whose cumulative carries forward (includes the
		// 2.50). Rows arrive ordered (conv-key, day, updated_at, session) per
		// AllCostDailyRows — chronological within a conv+day.
		{SessionID: "sess-a1", Day: "2026-06-01", ConvID: "conv-1", CostUSD: 1.00},
		{SessionID: "sess-a1", Day: "2026-06-02", ConvID: "conv-1", CostUSD: 1.00}, // ticked, spent nothing
		{SessionID: "sess-a1", Day: "2026-06-03", ConvID: "conv-1", CostUSD: 2.50},
		{SessionID: "sess-a2", Day: "2026-06-04", ConvID: "conv-1", CostUSD: 4.00}, // resume — cumulative continues
		// conv-2: resumed twice the SAME day under a new session — the later
		// snapshot already includes the earlier one.
		{SessionID: "sess-b1", Day: "2026-06-03", ConvID: "conv-2", CostUSD: 2.00},
		{SessionID: "sess-b2", Day: "2026-06-03", ConvID: "conv-2", CostUSD: 5.00},
		// conv-3: dips, then recovers past the max.
		{SessionID: "sess-c1", Day: "2026-06-01", ConvID: "conv-3", CostUSD: 3.00},
		{SessionID: "sess-c1", Day: "2026-06-02", ConvID: "conv-3", CostUSD: 1.00}, // dip — no negative delta
		{SessionID: "sess-c1", Day: "2026-06-03", ConvID: "conv-3", CostUSD: 3.50}, // only the rise above 3.00 counts
	}

	deltas := costDeltasFromRows(rows, false)

	got := map[string]map[string]float64{} // day -> conv -> usd
	for _, d := range deltas {
		if got[d.day] == nil {
			got[d.day] = map[string]float64{}
		}
		got[d.day][d.convID] += d.usd
	}
	assert.InDelta(t, 1.00, got["2026-06-01"]["conv-1"], 1e-9, "first snapshot carries the cumulative")
	assert.NotContains(t, got, "2026-06-02", "flat day and dip day produce no deltas")
	assert.InDelta(t, 1.50, got["2026-06-03"]["conv-1"], 1e-9, "growth within the conversation (2.50 - 1.00)")
	assert.InDelta(t, 1.50, got["2026-06-04"]["conv-1"], 1e-9,
		"resume under a NEW session counts only the rise (4.00 - 2.50), not the whole cumulative")
	assert.InDelta(t, 5.00, got["2026-06-03"]["conv-2"], 1e-9,
		"two same-day sessions collapse to one day's spend (5.00), not 2.00 + 5.00")
	assert.InDelta(t, 3.00, got["2026-06-01"]["conv-3"], 1e-9)
	assert.InDelta(t, 0.50, got["2026-06-03"]["conv-3"], 1e-9, "only the rise above the high-water mark")

	assert.InDelta(t, 12.50, sumCostDeltas(deltas, "", ""), 1e-9, "unbounded sum")
	assert.InDelta(t, 8.50, sumCostDeltas(deltas, "2026-06-03", "2026-06-04"), 1e-9, "bounded sum")
	assert.InDelta(t, 4.00, sumCostDeltas(deltas, "", "2026-06-01"), 1e-9, "upper bound only")
}

// TestCostDeltasFromRows_EmptyConvFallback pins the defensive fallback:
// a row with no denormalised conv_id baselines per session, so two
// unrelated sessions never merge into one high-water sequence (which
// would wrongly suppress the cheaper one as "below the running maximum").
func TestCostDeltasFromRows_EmptyConvFallback(t *testing.T) {
	rows := []db.CostDailyRow{
		{SessionID: "s1", Day: "2026-06-01", ConvID: "", CostUSD: 5.00},
		{SessionID: "s2", Day: "2026-06-01", ConvID: "", CostUSD: 2.00},
	}
	deltas := costDeltasFromRows(rows, false)
	assert.InDelta(t, 7.00, sumCostDeltas(deltas, "", ""), 1e-9,
		"empty-conv rows baseline per session, so both count (5.00 + 2.00)")
}

// TestCostDeltasFromRows_WhatIf pins the WHAT-IF column selection: with
// whatif=true the walk reads virtual_cost_usd and ignores cost_usd entirely,
// so a subscription account's hypothetical spend aggregates exactly as real
// spend would — and the real walk over the same rows sees nothing (these are
// subscription rows, real cost is 0).
func TestCostDeltasFromRows_WhatIf(t *testing.T) {
	rows := []db.CostDailyRow{
		// One subscription conversation: virtual cost grows, real stays 0.
		{SessionID: "s1", Day: "2026-06-01", ConvID: "conv-w", VirtualCostUSD: 1.00},
		{SessionID: "s1", Day: "2026-06-02", ConvID: "conv-w", VirtualCostUSD: 2.50},
		{SessionID: "s2", Day: "2026-06-03", ConvID: "conv-w", VirtualCostUSD: 4.00}, // resume — cumulative continues
	}

	whatif := costDeltasFromRows(rows, true)
	assert.InDelta(t, 4.00, sumCostDeltas(whatif, "", ""), 1e-9,
		"virtual deltas telescope like real ones: 1.00 + 1.50 + 1.50")

	real := costDeltasFromRows(rows, false)
	assert.Empty(t, real, "the real-cost walk sees nothing — these are subscription rows (cost_usd 0)")
}

// TestCostDeltasFromRows_SessionResetAcrossExit pins the cross-month /
// cross-span bug fix: a conversation spawned one day (a spwn- session
// peaking at 8.42), that EXITS, then is resumed the next day under a NEW
// session whose per-session total_cost_usd counter starts fresh at a LOWER
// cumulative (2.28). The high-water baseline must NOT clamp the resumed
// session's spend away — the session boundary plus the drop below the peak
// mark it as an independent counter, so its 2.28 is counted on its own day
// (day 2) instead of vanishing. Before the fix the resumed day produced no
// delta, so the agent showed only on day 1 and disappeared from any span
// starting on day 2 (the "only shows in the previous month" report).
func TestCostDeltasFromRows_SessionResetAcrossExit(t *testing.T) {
	rows := []db.CostDailyRow{
		{SessionID: "spwn-x", Day: "2026-06-30", ConvID: "conv-r", CostUSD: 8.42},
		{SessionID: "conv-r", Day: "2026-07-01", ConvID: "conv-r", CostUSD: 2.28},
	}
	deltas := costDeltasFromRows(rows, false)

	got := map[string]float64{}
	for _, d := range deltas {
		got[d.day] += d.usd
	}
	assert.InDelta(t, 8.42, got["2026-06-30"], 1e-9, "first session's peak counts on its own day")
	assert.InDelta(t, 2.28, got["2026-07-01"], 1e-9,
		"resumed session's fresh counter is counted, not clamped to the prior 8.42 peak")
	assert.InDelta(t, 10.70, sumCostDeltas(deltas, "", ""), 1e-9, "both independent counters sum (8.42 + 2.28)")
	assert.InDelta(t, 2.28, sumCostDeltas(deltas, "2026-07-01", ""), 1e-9,
		"a span starting on the resume day still sees the resumed spend")
}

// TestCostDeltasFromRows_CarryForwardNotReset is the counterpart guard: a
// resume whose new session CARRIES the prior cost forward (its cumulative
// is at or above the peak, e.g. the reinstate spwn→conv-id migration)
// must still telescope to the rise, never double-count. Only a DROP below
// the peak triggers the reset, so the double-count protection the per-conv
// baseline exists for is intact.
//
// The rows are in the CHRONOLOGICAL order AllCostDailyRows now delivers —
// original (spwn-y) first, reinstated (conv-c) second — even though the
// reinstated session's conv-id-named id ("conv-c") sorts BEFORE "spwn-y"
// lexically. Feeding them in that lexical order instead (the pre-fix SQL
// ordering) walks the higher cumulative first and double-counts the overlap;
// see TestSumCostSinceDay_ReinstateSameDayNoDoubleCount for that path end to
// end through the SQL.
func TestCostDeltasFromRows_CarryForwardNotReset(t *testing.T) {
	rows := []db.CostDailyRow{
		{SessionID: "spwn-y", Day: "2026-06-23", ConvID: "conv-c", CostUSD: 7.86,
			UpdatedAt: "2026-06-23T10:46:27+02:00"}, // original, earlier
		{SessionID: "conv-c", Day: "2026-06-23", ConvID: "conv-c", CostUSD: 12.26,
			UpdatedAt: "2026-06-23T11:17:02+02:00"}, // carry-forward, higher, later
	}
	deltas := costDeltasFromRows(rows, false)
	assert.InDelta(t, 12.26, sumCostDeltas(deltas, "", ""), 1e-9,
		"carry-forward resume counts only the rise: 7.86 + (12.26 - 7.86), not 7.86 + 12.26")
}

// TestFirstCostDay covers the first-ever-costed-day helper the Costs
// tab's month projection anchors its weekday average on: it's the min
// day across all deltas regardless of input order, and "" when nothing
// has been spent.
func TestFirstCostDay(t *testing.T) {
	assert.Equal(t, "", firstCostDay(nil), "no spend → no first day")
	assert.Equal(t, "", firstCostDay([]costDelta{}), "empty deltas → no first day")

	// Unsorted on purpose: the earliest day must win regardless of order.
	deltas := []costDelta{
		{day: "2026-06-10", usd: 1.0},
		{day: "2026-05-28", usd: 2.0},
		{day: "2026-06-03", usd: 0.5},
	}
	assert.Equal(t, "2026-05-28", firstCostDay(deltas), "earliest day across all deltas")
}
