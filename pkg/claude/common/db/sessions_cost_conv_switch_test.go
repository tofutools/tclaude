package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedCostRows inserts raw daily snapshots, bypassing the writers (which can
// only ever write "today").
func seedCostRows(t *testing.T, rows ...CostDailyRow) {
	t.Helper()
	d, err := Open()
	require.NoError(t, err, "open db")
	for _, r := range rows {
		harness := r.Harness
		if harness == "" {
			harness = DefaultHarness
		}
		_, err := d.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd, virtual_cost_usd, updated_at, harness)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, r.SessionID, r.Day, r.ConvID, r.CostUSD, r.VirtualCostUSD, r.UpdatedAtNS, harness)
		require.NoError(t, err, "seed %s/%s/%s", r.SessionID, r.ConvID, r.Day)
	}
}

func deltasByDayConv(deltas []CostDelta) map[string]float64 {
	out := map[string]float64{}
	for _, d := range deltas {
		out[d.Day+"/"+d.ConvID] += d.USD
	}
	return out
}

// TestCostDeltas_ConvSwitchAcrossDays is the observed double-count: an agent
// spawned under spwn-b5b5b3, resumed the next day under a session whose id is
// the conv id (carry-forward), then switched conversation (/clear or rename)
// in that same Claude Code process. total_cost_usd kept climbing 62.97 →
// 86.27 across the switch, so the new conversation must count only the rise,
// not its whole cumulative.
func TestCostDeltas_ConvSwitchAcrossDays(t *testing.T) {
	setupTestDB(t)
	seedCostRows(t,
		CostDailyRow{SessionID: "spwn-b5b5b3", Day: "2026-10-26", ConvID: "conv-a", CostUSD: 25.99,
			UpdatedAtNS: costStampNS(t, "2026-10-26T18:00:00Z")},
		CostDailyRow{SessionID: "conv-a", Day: "2026-10-27", ConvID: "conv-a", CostUSD: 62.97,
			UpdatedAtNS: costStampNS(t, "2026-10-27T18:00:00Z")},
		CostDailyRow{SessionID: "conv-a", Day: "2026-10-28", ConvID: "conv-b", CostUSD: 86.27,
			UpdatedAtNS: costStampNS(t, "2026-10-28T18:00:00Z")},
	)
	rows, err := AllCostDailyRows()
	require.NoError(t, err)

	for name, deltas := range map[string][]CostDelta{
		"CostDeltas":      CostDeltas(rows, false),
		"MixedCostDeltas": MixedCostDeltas(rows),
	} {
		got := deltasByDayConv(deltas)
		assert.InDelta(t, 25.99, got["2026-10-26/conv-a"], 1e-9, name)
		assert.InDelta(t, 36.98, got["2026-10-27/conv-a"], 1e-9, name)
		assert.InDelta(t, 23.30, got["2026-10-28/conv-b"], 1e-9, name+": only the rise after the switch")
	}

	total, err := SumCostSinceDay("2026-10-28")
	require.NoError(t, err)
	assert.InDelta(t, 23.30, total, 1e-9, "the top bar agrees")
	total, err = SumCostSinceDay("2026-10-01")
	require.NoError(t, err)
	assert.InDelta(t, 86.27, total, 1e-9, "the whole history is the counter's final value")
}

// TestUpdateSessionCost_ConvSwitchSameDaySplitsRow drives the writer through a
// same-day conversation switch: the old conversation's row stops at its
// pre-switch cumulative, the new one gets its own row, and the day's total
// equals the counter's rise.
func TestUpdateSessionCost_ConvSwitchSameDaySplitsRow(t *testing.T) {
	setupTestDB(t)
	today := time.Now().Format(costDayFormat)
	require.NoError(t, SaveSession(&SessionRow{ID: "sw", TmuxSession: "tmux-sw", ConvID: "conv-a", Cwd: "/tmp/sw", Status: "idle"}))
	require.NoError(t, UpdateSessionCost("sw", 4.00))
	require.NoError(t, UpdateSessionCost("sw", 10.00))

	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE sessions SET conv_id = 'conv-b' WHERE id = 'sw'`)
	require.NoError(t, err)
	require.NoError(t, UpdateSessionCost("sw", 12.00))
	require.NoError(t, UpdateSessionCost("sw", 15.00))

	rows, err := AllCostDailyRows()
	require.NoError(t, err)
	byConv := map[string]float64{}
	for _, r := range rows {
		if r.SessionID == "sw" && r.Day == today {
			byConv[r.ConvID] = r.CostUSD
		}
	}
	assert.Equal(t, map[string]float64{"conv-a": 10.00, "conv-b": 15.00}, byConv,
		"the old conversation's row keeps its pre-switch cumulative")

	got := deltasByDayConv(CostDeltas(rows, false))
	assert.InDelta(t, 10.00, got[today+"/conv-a"], 1e-9)
	assert.InDelta(t, 5.00, got[today+"/conv-b"], 1e-9, "the new conversation counts only the rise")
	total, err := SumCostSinceDay(today)
	require.NoError(t, err)
	assert.InDelta(t, 15.00, total, 1e-9)
}

// TestUpdateSessionCost_AdoptsUnattributedRow keeps the pre-v281 behaviour for
// a session that learns its conversation mid-day: the spend it recorded
// before then moves onto the conversation's row instead of staying behind as
// a second, separately-baselined row.
func TestUpdateSessionCost_AdoptsUnattributedRow(t *testing.T) {
	setupTestDB(t)
	today := time.Now().Format(costDayFormat)
	require.NoError(t, SaveSession(&SessionRow{ID: "late", TmuxSession: "tmux-late", Cwd: "/tmp/late", Status: "idle"}))
	require.NoError(t, UpdateSessionCost("late", 2.00))
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE sessions SET conv_id = 'conv-late' WHERE id = 'late'`)
	require.NoError(t, err)
	require.NoError(t, UpdateSessionCost("late", 3.00))

	rows, err := AllCostDailyRows()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "conv-late", rows[0].ConvID)
	assert.InDelta(t, 3.00, rows[0].CostUSD, 1e-9)

	// And a session that loses its conversation keeps writing to that row.
	_, err = d.Exec(`UPDATE sessions SET conv_id = '' WHERE id = 'late'`)
	require.NoError(t, err)
	require.NoError(t, UpdateSessionCost("late", 4.00))
	rows, err = AllCostDailyRows()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "conv-late", rows[0].ConvID)
	assert.Equal(t, today, rows[0].Day)
	assert.InDelta(t, 4.00, rows[0].CostUSD, 1e-9)
}

// TestAnnotateCostCarry_Boundaries pins what does NOT carry: a new session in
// a new conversation (reincarnation, clone), another harness's per-conversation
// counter, and a later fresh process whose counter is below the earlier one.
func TestAnnotateCostCarry_Boundaries(t *testing.T) {
	rows := []CostDailyRow{
		// Reincarnation: a new session and conversation start a fresh counter.
		{SessionID: "s-old", Day: "2026-10-27", ConvID: "conv-1", CostUSD: 50, Harness: "claude"},
		{SessionID: "s-new", Day: "2026-10-27", ConvID: "conv-2", CostUSD: 60, Harness: "claude"},
		// Codex /new under one session: per-thread counters start from zero.
		{SessionID: "cx", Day: "2026-10-27", ConvID: "conv-x", CostUSD: 5, Harness: "codex"},
		{SessionID: "cx", Day: "2026-10-28", ConvID: "conv-y", CostUSD: 8, Harness: "codex"},
		// Same session id, conversation switch, but the later counter is lower
		// than the earlier one: nothing from the earlier value is carried.
		{SessionID: "re", Day: "2026-10-27", ConvID: "conv-p", CostUSD: 9, Harness: "claude"},
		{SessionID: "re", Day: "2026-10-28", ConvID: "conv-q", CostUSD: 3, Harness: "claude"},
	}
	AnnotateCostCarry(rows)
	for _, r := range rows {
		assert.Zero(t, r.CarriedCostUSD, "%s/%s carries nothing", r.SessionID, r.ConvID)
	}
	var total float64
	for _, d := range CostDeltas(rows, false) {
		total += d.USD
	}
	assert.InDelta(t, 50+60+5+8+9+3, total, 1e-9)
}

// TestCostDeltas_ConvSwitchVirtual covers the subscription what-if column,
// which Claude Code fills from the same process-wide counter.
func TestCostDeltas_ConvSwitchVirtual(t *testing.T) {
	rows := []CostDailyRow{
		{SessionID: "s", Day: "2026-10-28", ConvID: "conv-a", VirtualCostUSD: 7,
			UpdatedAtNS: costStampNS(t, "2026-10-28T10:00:00Z")},
		{SessionID: "s", Day: "2026-10-28", ConvID: "conv-b", VirtualCostUSD: 11,
			UpdatedAtNS: costStampNS(t, "2026-10-28T12:00:00Z")},
	}
	AnnotateCostCarry(rows)
	got := deltasByDayConv(CostDeltas(rows, true))
	assert.InDelta(t, 7, got["2026-10-28/conv-a"], 1e-9)
	assert.InDelta(t, 4, got["2026-10-28/conv-b"], 1e-9)
	got = deltasByDayConv(MixedCostDeltas(rows))
	assert.InDelta(t, 4, got["2026-10-28/conv-b"], 1e-9)
}

func TestMigrateV280toV281KeysCostDailyByConv(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v281?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (280)`)
	mustExec(t, d, `CREATE TABLE session_cost_daily (
		session_id TEXT NOT NULL,
		day        TEXT NOT NULL,
		conv_id    TEXT NOT NULL DEFAULT '',
		cost_usd   REAL NOT NULL DEFAULT 0, updated_at INTEGER, virtual_cost_usd REAL NOT NULL DEFAULT 0, model TEXT NOT NULL DEFAULT '', agent_id TEXT NOT NULL DEFAULT '', harness TEXT NOT NULL DEFAULT 'claude',
		PRIMARY KEY (session_id, day)
	) STRICT`)
	mustExec(t, d, `INSERT INTO session_cost_daily VALUES
		('s', '2026-10-27', 'conv-a', 62.97, 100, 0, 'Opus', 'agt_1', 'claude'),
		('s', '2026-10-28', 'conv-b', 86.27, 200, 0, 'Opus', 'agt_1', 'claude')`)

	require.NoError(t, migrateV280toV281(d))
	assert.Equal(t, 281, schemaVersion(d))

	var n int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM session_cost_daily
		WHERE session_id = 's' AND model = 'Opus' AND agent_id = 'agt_1'`).Scan(&n))
	assert.Equal(t, 2, n, "rows survive the rebuild")
	mustExec(t, d, `INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd)
		VALUES ('s', '2026-10-28', 'conv-a', 70)`)
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name IN ('idx_session_cost_daily_day', 'idx_session_cost_daily_walk')`).Scan(&n))
	assert.Equal(t, 2, n, "indexes recreated")
}
