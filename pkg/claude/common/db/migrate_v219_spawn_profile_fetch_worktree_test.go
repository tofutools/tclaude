package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV218toV219ConvergesMigrationCollision(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		haveValidationColumn  bool
		haveFetchLatestColumn bool
	}{
		{name: "normal v218 validation path", haveValidationColumn: true},
		{name: "colliding v218 fetch path", haveFetchLatestColumn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v218.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = d.Close() })
			mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
			mustExec(t, d, `INSERT INTO schema_version VALUES (218)`)
			mustExec(t, d, `CREATE TABLE agent_prs (id INTEGER PRIMARY KEY) STRICT`)
			mustExec(t, d, `CREATE TABLE spawn_profiles (id INTEGER PRIMARY KEY) STRICT`)
			if tc.haveValidationColumn {
				mustExec(t, d, `ALTER TABLE agent_prs ADD COLUMN validated_repo_root TEXT NOT NULL DEFAULT ''`)
			}
			if tc.haveFetchLatestColumn {
				mustExec(t, d, `ALTER TABLE spawn_profiles ADD COLUMN fetch_latest_worktree INTEGER`)
			}

			require.NoError(t, migrateV218toV219(d))
			assert.Equal(t, 219, schemaVersion(d))
			assertTableHasColumn(t, d, "agent_prs", "validated_repo_root")
			assertTableHasColumn(t, d, "spawn_profiles", "fetch_latest_worktree")
			require.NoError(t, migrateV218toV219(d), "partially applied migration converges")
		})
	}
}

func assertTableHasColumn(t *testing.T, d *sql.DB, table, column string) {
	t.Helper()
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count))
	assert.Equal(t, 1, count, "%s.%s", table, column)
}
