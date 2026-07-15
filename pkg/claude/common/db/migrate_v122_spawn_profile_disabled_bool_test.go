package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV121ToV122SpawnProfileDisabledBool(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v122?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (121);
		CREATE TABLE spawn_profiles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			disabled_reason TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO spawn_profiles (name, disabled_reason) VALUES
			('enabled', ''),
			('paused', 'provider maintenance');
	`)
	require.NoError(t, err)

	require.NoError(t, migrateV121toV122(d))
	require.NoError(t, migrateV121toV122(d), "half-applied migration converges")

	rows, err := d.Query(`SELECT name, disabled, disabled_reason FROM spawn_profiles ORDER BY name`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	type state struct {
		disabled int
		reason   string
	}
	got := map[string]state{}
	for rows.Next() {
		var name string
		var value state
		require.NoError(t, rows.Scan(&name, &value.disabled, &value.reason))
		got[name] = value
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, state{disabled: 0}, got["enabled"])
	assert.Equal(t, state{disabled: 1, reason: "provider maintenance"}, got["paused"])

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 122, ver)
}
