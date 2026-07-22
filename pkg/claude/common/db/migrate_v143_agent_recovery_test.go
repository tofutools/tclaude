package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV142toV143AddsAgentRecoveryLedger(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v143?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (142)`)
	mustExec(t, d, `CREATE TABLE agents (agent_id TEXT PRIMARY KEY)`)

	require.NoError(t, migrateV142toV143(d))
	require.NoError(t, migrateV142toV143(d), "partial migration converges")
	assert.Equal(t, 143, schemaVersion(d))
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='agent_recovery'`).Scan(&count))
	assert.Equal(t, 1, count)
}
