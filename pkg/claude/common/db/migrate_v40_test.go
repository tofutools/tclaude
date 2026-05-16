package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV39toV40_AddsTransferLog seeds a minimal v39 schema, runs
// the v40 migration, and asserts agent_transfer_log lands as a usable
// table and the schema version is bumped.
func TestMigrateV39toV40_AddsTransferLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v39.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (39);
	`)
	require.NoError(t, err, "seed v39 schema")

	require.NoError(t, migrateV39toV40(d), "migrateV39toV40")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 40, ver, "schema_version after migration")

	// The table accepts a representative import row and reads it back.
	_, err = d.Exec(`
		INSERT INTO agent_transfer_log
			(kind, at, format_version, source_group, source_home, source_os,
			 result_group, target_dir, conv_remaps, agent_count, message_count, by_conv)
		VALUES ('import', '2026-05-16T00:00:00Z', 1, 'team', '/home/a', 'linux',
		        'team-restored', '/tmp/dst', '{"old":"new"}', 3, 7, '')`)
	require.NoError(t, err, "insert into agent_transfer_log")

	var kind, resultGroup string
	var agentCount int
	require.NoError(t, d.QueryRow(
		`SELECT kind, result_group, agent_count FROM agent_transfer_log`).
		Scan(&kind, &resultGroup, &agentCount))
	assert.Equal(t, "import", kind)
	assert.Equal(t, "team-restored", resultGroup)
	assert.Equal(t, 3, agentCount)

	// Migration is idempotent — re-running against the now-v40 DB is a
	// no-op rather than an error (CREATE TABLE IF NOT EXISTS).
	require.NoError(t, migrateV39toV40(d), "migrateV39toV40 second run")
}
