package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV65toV66_AddsRemoteControlDefaults seeds a bare v65 schema, runs
// the v66 migration, and asserts the tri-state remote_control columns land on
// BOTH spawn_profiles and agent_groups as NULLABLE (no NOT NULL / DEFAULT) — so
// "unset" is a real state distinct from "off" — and accept NULL / 0 / 1 writes.
func TestMigrateV65toV66_AddsRemoteControlDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v65.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (65);
		CREATE TABLE spawn_profiles (id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO spawn_profiles (id, name) VALUES (1, 'p');
		CREATE TABLE agent_groups (id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO agent_groups (id, name) VALUES (1, 'g');
	`)
	require.NoError(t, err, "seed v65 schema")

	require.NoError(t, migrateV65toV66(d), "migrateV65toV66")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 66, ver, "schema_version after migration")

	// Pre-existing rows read NULL (unset), not 0 — the tri-state's whole point.
	for _, q := range []struct{ table, sel string }{
		{"spawn_profiles", `SELECT remote_control FROM spawn_profiles WHERE id = 1`},
		{"agent_groups", `SELECT remote_control FROM agent_groups WHERE id = 1`},
	} {
		var rc sql.NullInt64
		require.NoErrorf(t, d.QueryRow(q.sel).Scan(&rc), "read %s.remote_control", q.table)
		assert.Falsef(t, rc.Valid, "pre-v66 %s row must read remote_control as NULL (unset)", q.table)
	}

	// Both columns accept the full tri-state.
	_, err = d.Exec(`UPDATE spawn_profiles SET remote_control = 1 WHERE id = 1`)
	require.NoError(t, err, "spawn_profiles.remote_control accepts 1")
	_, err = d.Exec(`UPDATE agent_groups SET remote_control = 0 WHERE id = 1`)
	require.NoError(t, err, "agent_groups.remote_control accepts 0")
}

// TestMigrateV65toV66_HealsHalfAppliedRun guards the wedge class: an interrupted
// earlier attempt added ONE of the two columns but never bumped schema_version.
// The per-column pragma_table_info probe makes the re-run skip the already-added
// column, add the missing one, and land on 66 — and a second re-run is a no-op.
func TestMigrateV65toV66_HealsHalfAppliedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v65-half.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// Half-applied: spawn_profiles already has the column (with a non-default
	// value, to prove the re-run preserves it); agent_groups does not; version
	// still 65.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (65);
		CREATE TABLE spawn_profiles (id INTEGER PRIMARY KEY, name TEXT, remote_control INTEGER);
		INSERT INTO spawn_profiles (id, name, remote_control) VALUES (1, 'p', 1);
		CREATE TABLE agent_groups (id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO agent_groups (id, name) VALUES (1, 'g');
	`)
	require.NoError(t, err, "seed half-applied v65 schema")

	require.NoError(t, migrateV65toV66(d), "re-run must converge, not fail on duplicate column")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 66, ver, "schema_version finally lands on 66")

	// The already-present value survived, and the missing column was added.
	var rc sql.NullInt64
	require.NoError(t, d.QueryRow(`SELECT remote_control FROM spawn_profiles WHERE id = 1`).Scan(&rc))
	assert.True(t, rc.Valid, "existing spawn_profiles.remote_control survives")
	assert.Equal(t, int64(1), rc.Int64, "existing value preserved")
	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'remote_control'`).Scan(&have))
	assert.Equal(t, 1, have, "the missing agent_groups.remote_control was added")

	// Second re-run: both probes find the columns, both ALTERs skip, stays 66.
	require.NoError(t, migrateV65toV66(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 66, ver, "second re-run stays at 66")
}

// TestMigrateV65toV66_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts both remote_control columns exist. The literal
// currentVersion tripwire moved forward to the v67 test when v67 became head.
func TestMigrateV65toV66_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

	for _, table := range []string{"spawn_profiles", "agent_groups"} {
		var haveCol int
		require.NoErrorf(t, d.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('`+table+`') WHERE name = 'remote_control'`,
		).Scan(&haveCol), "probe %s.remote_control", table)
		assert.Equalf(t, 1, haveCol, "fresh schema has %s.remote_control", table)
	}
}
