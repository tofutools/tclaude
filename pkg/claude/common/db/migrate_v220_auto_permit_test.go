package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV219toV220CreatesAutoPermitTable(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v219.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (219)`)
	mustExec(t, d, `CREATE TABLE agents (agent_id TEXT PRIMARY KEY) STRICT`)

	require.NoError(t, migrateV219toV220(d))
	assert.Equal(t, 220, schemaVersion(d))
	assertTableHasColumn(t, d, "agent_auto_permit", "agent_id")
	assertTableHasColumn(t, d, "agent_auto_permit", "condition")
	assertTableHasColumn(t, d, "agent_auto_permit", "granted_by")
	assertTableHasColumn(t, d, "agent_auto_permit", "created_at")

	require.NoError(t, migrateV219toV220(d), "re-running a migration converges")
}
