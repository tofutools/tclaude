package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV171toV172BackfillsStandingOrderRowVersion(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v172?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version(version) VALUES (171);
		CREATE TABLE agent_standing_orders (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL DEFAULT 1
		);
		INSERT INTO agent_standing_orders(id, name, revision)
		VALUES (1, 'existing', 7);
	`)
	require.NoError(t, err)

	require.NoError(t, migrateV171toV172(d))
	assert.Equal(t, 172, schemaVersion(d))

	var revision, rowVersion int64
	require.NoError(t, d.QueryRow(
		`SELECT revision, row_version FROM agent_standing_orders WHERE id = 1`,
	).Scan(&revision, &rowVersion))
	assert.Equal(t, int64(7), revision)
	assert.Equal(t, revision, rowVersion)

	require.NoError(t, migrateV171toV172(d), "already-applied migration is a no-op")
}
