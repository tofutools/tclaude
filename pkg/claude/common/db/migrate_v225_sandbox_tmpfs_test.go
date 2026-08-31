package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV225AddsSandboxTmpfsColumn(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v224.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (224);
		CREATE TABLE sandbox_profiles (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
		INSERT INTO sandbox_profiles (name) VALUES ('existing');
	`)

	require.NoError(t, migrateV224toV225(d))
	assert.Equal(t, 225, schemaVersion(d))
	assert.GreaterOrEqual(t, currentVersion, 225)
	// '[]' is the exact opt-out default: a profile written before the column
	// existed decodes to no mounts, which renders no plan entry at all.
	assertRowValue(t, d, `SELECT tmpfs_json FROM sandbox_profiles WHERE name = 'existing'`, "[]")
	require.NoError(t, migrateV224toV225(d), "partially applied migration converges")
}

// A host that has never had a sandbox_profiles table must still advance, or the
// chain stalls for anyone who upgrades past this point without one.
func TestMigrateV225WithoutTheSandboxProfilesTable(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v224-bare.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (224);
	`)

	require.NoError(t, migrateV224toV225(d))
	assert.Equal(t, 225, schemaVersion(d))
}
