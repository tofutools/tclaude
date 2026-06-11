package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV50toV51_CreatesDailyTableAndBackfills seeds a bare v50
// DB whose sessions already carry cost, runs the v51 migration, and
// asserts session_cost_daily lands with today's backfill rows — so
// pre-migration cost (including retired sessions that will never tick
// a statusline again) shows up in the daily series immediately, while
// zero-cost rows are left out.
func TestMigrateV50toV51_CreatesDailyTableAndBackfills(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v50.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// A minimal v50 sessions table: one costed row, one zero-cost row.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (50);
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			conv_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'idle',
			cost_usd REAL NOT NULL DEFAULT 0
		);
		INSERT INTO sessions (id, conv_id, status, cost_usd) VALUES
			('costed', 'conv-c', 'exited', 1.37),
			('free',   'conv-f', 'idle',   0);
	`)
	require.NoError(t, err, "seed v50 schema")

	require.NoError(t, migrateV50toV51(d), "migrateV50toV51")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 51, ver, "schema_version after migration")

	var day, convID string
	var cost float64
	require.NoError(t, d.QueryRow(
		`SELECT day, conv_id, cost_usd FROM session_cost_daily WHERE session_id = 'costed'`).
		Scan(&day, &convID, &cost), "backfilled row for the costed session")
	assert.Equal(t, time.Now().Format(costDayFormat), day, "backfill lands on today (local)")
	assert.Equal(t, "conv-c", convID, "conv_id denormalised in")
	assert.InDelta(t, 1.37, cost, 1e-9, "cumulative cost carried over")

	var n int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM session_cost_daily`).Scan(&n))
	assert.Equal(t, 1, n, "zero-cost sessions are not backfilled")
}

// TestMigrateV50toV51_FreshSchemaWritesDaily builds a fresh DB through
// the full migrate() chain and confirms the UpdateSessionCost sibling
// write lands in session_cost_daily end to end. Carries the literal
// currentVersion pin — a tripwire the next migration's author moves
// forward into their own v52 test.
func TestMigrateV50toV51_FreshSchemaWritesDaily(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 51, currentVersion, "currentVersion is 51")

	require.NoError(t, SaveSession(&SessionRow{ID: "s1", ConvID: "conv-1", Status: "idle"}), "SaveSession")
	require.NoError(t, UpdateSessionCost("s1", 0.42), "UpdateSessionCost on a fresh schema")

	rows, err := AllCostDailyRows()
	require.NoError(t, err, "AllCostDailyRows")
	require.Len(t, rows, 1, "one daily row after one costed tick")
	assert.Equal(t, "s1", rows[0].SessionID)
	assert.Equal(t, time.Now().Format(costDayFormat), rows[0].Day, "keyed by today (local)")
	assert.Equal(t, "conv-1", rows[0].ConvID)
	assert.InDelta(t, 0.42, rows[0].CostUSD, 1e-9)
}
