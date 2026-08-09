package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV202toV203AddsPendingCodexDriveTriState(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v202.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (202)`)
	mustExec(t, d, `CREATE TABLE pending_spawns (label TEXT PRIMARY KEY) STRICT`)

	require.NoError(t, migrateV202toV203(d))
	assert.Equal(t, 203, schemaVersion(d))
	for _, column := range []string{"codex_app_server", "codex_app_server_source", "codex_state_root", "codex_state_root_source"} {
		var count int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pending_spawns') WHERE name = ?`, column).Scan(&count))
		assert.Equal(t, 1, count, column)
	}
	require.NoError(t, migrateV202toV203(d), "partially applied migration converges")
}

func TestMigrateV202toV203ToleratesPartialLegacySchemaWithoutPendingSpawns(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v202-partial.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (202)`)

	require.NoError(t, migrateV202toV203(d))
	assert.Equal(t, 203, schemaVersion(d))
}

func TestPendingSpawnCodexDriveRoundTripsUnsetTrueAndFalse(t *testing.T) {
	setupTestDB(t)
	for i, value := range []*bool{nil, boolPtr(true), boolPtr(false)} {
		label := []string{"pending-unset", "pending-true", "pending-false"}[i]
		root := "/tmp/codex-state-" + label
		require.NoError(t, InsertPendingSpawn(&PendingSpawn{
			Label: label, GroupID: 1, CodexAppServer: value,
			CodexAppServerSource: "source-" + label,
			CodexStateRoot:       root, CodexStateRootSource: "CODEX_HOME",
		}))
		got, err := GetPendingSpawn(label)
		require.NoError(t, err)
		require.NotNil(t, got)
		if value == nil {
			assert.Nil(t, got.CodexAppServer)
		} else {
			require.NotNil(t, got.CodexAppServer)
			assert.Equal(t, *value, *got.CodexAppServer)
		}
		assert.Equal(t, "source-"+label, got.CodexAppServerSource)
		assert.Equal(t, root, got.CodexStateRoot)
		assert.Equal(t, "CODEX_HOME", got.CodexStateRootSource)
	}
}
