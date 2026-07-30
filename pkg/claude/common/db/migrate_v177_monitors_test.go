package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV176toV177AddsMonitorsColumn(t *testing.T) {
	require.GreaterOrEqual(t, currentVersion, 177)
	d, err := sql.Open("sqlite", "file:migrate-v177?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (176)`)
	mustExec(t, d, `CREATE TABLE sessions (id TEXT PRIMARY KEY)`)
	mustExec(t, d, `INSERT INTO sessions (id) VALUES ('legacy-sess')`)

	require.NoError(t, migrateV176toV177(d))
	assert.Equal(t, 177, schemaVersion(d))
	require.NoError(t, migrateV176toV177(d), "migration converges after a partial application")

	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'monitors_json'`).Scan(&n))
	assert.Equal(t, 1, n, "sessions.monitors_json added")

	// A legacy row reads '' — "empty ledger", which is the correct history
	// for it: a monitor belongs to the harness process that wrote the row,
	// so none can have survived into this migration.
	var ledger string
	require.NoError(t, d.QueryRow(
		`SELECT monitors_json FROM sessions WHERE id = 'legacy-sess'`).Scan(&ledger))
	assert.Equal(t, "", ledger)
	assert.Empty(t, ParseMonitorSet(ledger), "'' decodes to an empty ledger")
}
