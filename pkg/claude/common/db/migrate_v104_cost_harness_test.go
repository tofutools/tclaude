package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV103toV104_FreshSchema builds a fresh DB through the full
// migrate() chain and asserts it lands at currentVersion. The literal head
// tripwire is advanced by each subsequent migration test.
func TestMigrateV103toV104_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('session_cost_daily') WHERE name = 'harness'`).Scan(&have))
	assert.Equal(t, 1, have, "fresh schema has session_cost_daily.harness")
}

// TestMigrateV103toV104_AddsAndBackfillsHarness drives the real v103→v104 ALTER
// over a v103-pinned DB. Live sessions backfill first; conv_index fills rows
// whose sessions row is already gone.
func TestMigrateV103toV104_AddsAndBackfillsHarness(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	mustExec(t, d, `ALTER TABLE session_cost_daily DROP COLUMN harness`)
	mustExec(t, d, `UPDATE schema_version SET version = 103`)

	require.NoError(t, SaveSession(&SessionRow{
		ID: "live-codex", TmuxSession: "tmux-live-codex", ConvID: "conv-live", Status: "idle", Harness: "codex",
	}), "SaveSession")
	mustExec(t, d, `INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd)
		VALUES ('live-codex', '2026-07-09', 'conv-live', 1.00)`)
	require.NoError(t, UpsertConvIndex(&ConvIndexRow{
		ConvID: "conv-retired", ProjectDir: "/tmp", Created: "2026-07-09T00:00:00Z", Harness: "codex",
	}), "UpsertConvIndex")
	mustExec(t, d, `INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd)
		VALUES ('retired-codex', '2026-07-09', 'conv-retired', 2.00)`)

	require.NoError(t, migrateV103toV104(d), "v103→v104")

	var live, retired string
	require.NoError(t, d.QueryRow(
		`SELECT harness FROM session_cost_daily WHERE session_id = 'live-codex'`).Scan(&live))
	require.NoError(t, d.QueryRow(
		`SELECT harness FROM session_cost_daily WHERE session_id = 'retired-codex'`).Scan(&retired))
	assert.Equal(t, "codex", live, "live session harness backfilled from sessions")
	assert.Equal(t, "codex", retired, "retired row harness backfilled from conv_index")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 104, ver, "version advanced")

	require.NoError(t, migrateV103toV104(d), "v103→v104 re-run is a clean no-op")
}
