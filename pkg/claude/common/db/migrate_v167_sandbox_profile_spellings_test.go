package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV166toV167PreservesLegacySandboxFilesystemBytes(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v167?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (166)`)
	mustExec(t, d, `CREATE TABLE sandbox_profiles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		filesystem_json TEXT NOT NULL DEFAULT '[]',
		environment_json TEXT NOT NULL DEFAULT '[]',
		agent_directories_json TEXT NOT NULL DEFAULT '[]',
		network_access TEXT NOT NULL DEFAULT '',
		network_json TEXT NOT NULL DEFAULT '',
		unix_sockets_json TEXT NOT NULL DEFAULT '',
		includes_json TEXT NOT NULL DEFAULT '[]',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`)
	const filesystem = `[ { "access" : "read", "path" : "/legacy/B" },{"path":"/legacy/a","access":"deny"} ]`
	mustExec(t, d, `INSERT INTO sandbox_profiles
		(name, filesystem_json, created_at, updated_at)
		VALUES ('legacy', ?, 'created-exact', 'updated-exact')`, filesystem)

	require.NoError(t, migrateV166toV167(d))
	var gotFilesystem, spellings, createdAt, updatedAt string
	require.NoError(t, d.QueryRow(`SELECT filesystem_json, filesystem_spellings_json,
		created_at, updated_at FROM sandbox_profiles WHERE name = 'legacy'`).Scan(
		&gotFilesystem, &spellings, &createdAt, &updatedAt,
	))
	assert.Equal(t, filesystem, gotFilesystem)
	assert.Equal(t, "", spellings)
	assert.Equal(t, "created-exact", createdAt)
	assert.Equal(t, "updated-exact", updatedAt)
	assert.Equal(t, 167, schemaVersion(d))
	require.NoError(t, migrateV166toV167(d), "partially applied migration converges")
}
