package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func newV139DB(t *testing.T, name string) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+name+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (139)`)
	return d
}

// A pre-TCL-609 row must survive the upgrade with defaults that mean exactly
// "behave as before": inherited harness read baseline, no protected access.
func TestMigrateV140AddsStrictBaselineColumnsWithCompatibleDefaults(t *testing.T) {
	d := newV139DB(t, "migrate-v140-defaults")
	mustExec(t, d, `CREATE TABLE sandbox_profiles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		filesystem_json TEXT NOT NULL DEFAULT '[]',
		environment_json TEXT NOT NULL DEFAULT '[]',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`)
	mustExec(t, d, `INSERT INTO sandbox_profiles (name, created_at, updated_at) VALUES ('legacy', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)

	require.NoError(t, migrateV139toV140(d))

	for _, column := range []string{"read_baseline", "break_glass_filesystem_json"} {
		var have int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = ?`, column).Scan(&have))
		assert.Equal(t, 1, have, "column %s", column)
	}

	var readBaseline, breakGlass string
	require.NoError(t, d.QueryRow(`SELECT read_baseline, break_glass_filesystem_json FROM sandbox_profiles WHERE name = 'legacy'`).Scan(&readBaseline, &breakGlass))
	assert.Equal(t, "", readBaseline, "an existing profile must keep the inherited harness read baseline")
	assert.Equal(t, "[]", breakGlass, "an existing profile must gain no protected-path authority")

	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 140, version)
}

// The migration must be safe to re-run and safe on a DB that never had the
// table, matching every other sandbox-profile column migration.
func TestMigrateV140IsIdempotentAndTableOptional(t *testing.T) {
	d := newV139DB(t, "migrate-v140-idempotent")
	require.NoError(t, migrateV139toV140(d), "no sandbox_profiles table present")

	mustExec(t, d, `UPDATE schema_version SET version = 139`)
	mustExec(t, d, `CREATE TABLE sandbox_profiles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		read_baseline TEXT NOT NULL DEFAULT '',
		break_glass_filesystem_json TEXT NOT NULL DEFAULT '[]',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`)
	require.NoError(t, migrateV139toV140(d), "columns already present")
}
