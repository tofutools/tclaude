package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestCostDeltasFromRows covers the cumulative→delta recovery that
// every cost surface (top bar, /api/costs) is built on: consecutive
// daily snapshots of one session become that session's per-day spend,
// the first snapshot carries the whole cumulative (the v51 backfill
// case), the baseline resets between sessions, and a dip-and-recover
// sequence (the /clear edge) never produces a negative day or double
// counts the recovery.
func TestCostDeltasFromRows(t *testing.T) {
	rows := []db.CostDailyRow{
		// Session a (conv-1): plain growth across three days.
		{SessionID: "a", Day: "2026-06-01", ConvID: "conv-1", CostUSD: 1.00},
		{SessionID: "a", Day: "2026-06-02", ConvID: "conv-1", CostUSD: 1.00}, // ticked, spent nothing
		{SessionID: "a", Day: "2026-06-03", ConvID: "conv-1", CostUSD: 2.50},
		// Session b (conv-1, e.g. a reincarnation): cumulative restarts at 0.
		{SessionID: "b", Day: "2026-06-03", ConvID: "conv-1", CostUSD: 0.40},
		// Session c (conv-2): dips after /clear, then recovers past the max.
		{SessionID: "c", Day: "2026-06-01", ConvID: "conv-2", CostUSD: 3.00},
		{SessionID: "c", Day: "2026-06-02", ConvID: "conv-2", CostUSD: 1.00}, // dip — no negative delta
		{SessionID: "c", Day: "2026-06-03", ConvID: "conv-2", CostUSD: 3.50}, // only the rise above 3.00 counts
	}

	deltas := costDeltasFromRows(rows)

	got := map[string]map[string]float64{} // day -> conv -> usd
	for _, d := range deltas {
		if got[d.day] == nil {
			got[d.day] = map[string]float64{}
		}
		got[d.day][d.convID] += d.usd
	}
	assert.InDelta(t, 1.00, got["2026-06-01"]["conv-1"], 1e-9, "first snapshot carries the cumulative")
	assert.NotContains(t, got, "2026-06-02", "flat day and dip day produce no deltas")
	assert.InDelta(t, 1.90, got["2026-06-03"]["conv-1"], 1e-9, "growth (1.50) + sibling session's start (0.40)")
	assert.InDelta(t, 3.00, got["2026-06-01"]["conv-2"], 1e-9)
	assert.InDelta(t, 0.50, got["2026-06-03"]["conv-2"], 1e-9, "only the rise above the high-water mark")

	assert.InDelta(t, 6.40, sumCostDeltas(deltas, "", ""), 1e-9, "unbounded sum")
	assert.InDelta(t, 2.40, sumCostDeltas(deltas, "2026-06-02", "2026-06-03"), 1e-9, "bounded sum")
	assert.InDelta(t, 4.00, sumCostDeltas(deltas, "", "2026-06-01"), 1e-9, "upper bound only")
}
