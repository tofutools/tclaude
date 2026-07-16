package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV124toV125AddsSpawnHarnessRules(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v125?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (124)`)

	require.NoError(t, migrateV124toV125(d))
	mustExec(t, d, `INSERT INTO spawn_harness_rules
		(group_id, source_harness, target_harness, decision, reason, updated_at)
		VALUES (0, 'claude', 'codex', 'deny', 'budget', 'now')`)
	assert.Equal(t, 125, schemaVersion(d))
	require.NoError(t, migrateV124toV125(d), "half-applied migration converges")
}
