package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV170toV171AddsInertMatcherColumns(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v171?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version(version) VALUES (170);
		CREATE TABLE agent_standing_orders (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			trigger_event TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO agent_standing_orders(id, name) VALUES (1, 'existing');
	`)
	require.NoError(t, err)

	require.NoError(t, migrateV170toV171(d))
	assert.Equal(t, 171, schemaVersion(d))

	var field, expression string
	require.NoError(t, d.QueryRow(
		`SELECT match_field, match_regex FROM agent_standing_orders WHERE id = 1`,
	).Scan(&field, &expression))
	assert.Empty(t, field)
	assert.Empty(t, expression)

	var indexes int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master
		 WHERE type = 'index' AND name = 'idx_agent_standing_orders_enabled_trigger'`,
	).Scan(&indexes))
	assert.Equal(t, 1, indexes)

	require.NoError(t, migrateV170toV171(d), "partially applied migration converges")
}
