package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV106toV107_AddsTemplatePerAgentWorktreesDefault(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	require.Equal(t, 112, currentVersion, "tripwire: bump this with the next migration")

	mustExec(t, d, `ALTER TABLE group_templates DROP COLUMN per_agent_worktrees`)
	mustExec(t, d, `INSERT INTO group_templates (name, created_at, updated_at)
		VALUES ('legacy', '2026-07-10T00:00:00Z', '2026-07-10T00:00:00Z')`)
	mustExec(t, d, `UPDATE schema_version SET version = 106`)

	require.NoError(t, migrateV106toV107(d))
	require.NoError(t, migrateV106toV107(d), "half-applied/re-run converges")

	var got int
	require.NoError(t, d.QueryRow(
		`SELECT per_agent_worktrees FROM group_templates WHERE name = 'legacy'`).Scan(&got))
	assert.Zero(t, got, "legacy templates keep the per-agent-worktree option off")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 107, ver)
}
