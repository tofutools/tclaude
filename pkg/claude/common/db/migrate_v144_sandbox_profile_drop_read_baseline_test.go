package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func newV143DB(t *testing.T, name string) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+name+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (143)`)
	return d
}

func v144ProfilesTable(t *testing.T, d *sql.DB) {
	t.Helper()
	mustExec(t, d, `CREATE TABLE sandbox_profiles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		filesystem_json TEXT NOT NULL DEFAULT '[]',
		environment_json TEXT NOT NULL DEFAULT '[]',
		read_baseline TEXT NOT NULL DEFAULT '',
		read_baseline_exclusions_json TEXT NOT NULL DEFAULT '[]',
		break_glass_filesystem_json TEXT NOT NULL DEFAULT '[]',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`)
}

// The strict-read mechanism is dropped outright (TCL-623): the columns go away
// and the surrounding profile payload is untouched. Any value they carried is
// deliberately discarded rather than translated into deny rows — inventing
// rules on an operator's behalf would change what their profiles enforce.
func TestMigrateV144DropsReadBaselineColumnsAndKeepsPayload(t *testing.T) {
	d := newV143DB(t, "migrate-v144-drop")
	v144ProfilesTable(t, d)
	mustExec(t, d, `INSERT INTO sandbox_profiles (name, filesystem_json, read_baseline, read_baseline_exclusions_json, created_at, updated_at)
		VALUES ('strict', '[{"path":"/tmp","access":"deny"}]', 'minimal', '["home.directory"]', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)

	require.NoError(t, migrateV143toV144(d))

	for _, column := range []string{"read_baseline", "read_baseline_exclusions_json"} {
		var have int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = ?`, column).Scan(&have))
		assert.Equalf(t, 0, have, "column %s must be gone", column)
	}
	var filesystem, breakGlass string
	require.NoError(t, d.QueryRow(`SELECT filesystem_json, break_glass_filesystem_json FROM sandbox_profiles WHERE name = 'strict'`).Scan(&filesystem, &breakGlass))
	assert.JSONEq(t, `[{"path":"/tmp","access":"deny"}]`, filesystem, "the ordinary rows are the surviving mechanism and must be untouched")
	assert.Equal(t, "[]", breakGlass)

	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 144, version)
}

func TestMigrateV144IsIdempotentAndTableOptional(t *testing.T) {
	d := newV143DB(t, "migrate-v144-idempotent")
	require.NoError(t, migrateV143toV144(d), "no sandbox_profiles table present")

	mustExec(t, d, `UPDATE schema_version SET version = 143`)
	v144ProfilesTable(t, d)
	require.NoError(t, migrateV143toV144(d))
	mustExec(t, d, `UPDATE schema_version SET version = 143`)
	require.NoError(t, migrateV143toV144(d), "columns already dropped")
}

func TestMigrateV145RemainsInTheMigrationChain(t *testing.T) {
	require.GreaterOrEqual(t, currentVersion, 145)
}
