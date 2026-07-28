package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV167toV168AddsGroupAttachment(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v168?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (167)`)
	mustExec(t, d, `CREATE TABLE agent_groups (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE
	)`)
	mustExec(t, d, `INSERT INTO agent_groups (name) VALUES ('legacy')`)

	require.NoError(t, migrateV167toV168(d))
	var refURL, label string
	require.NoError(t, d.QueryRow(
		`SELECT attachment_url, attachment_label FROM agent_groups WHERE name = 'legacy'`,
	).Scan(&refURL, &label))
	assert.Empty(t, refURL)
	assert.Empty(t, label)
	assert.Equal(t, 168, schemaVersion(d))
	require.NoError(t, migrateV167toV168(d), "partially applied migration converges")
}

func TestMigrateV167toV168ToleratesMissingAgentGroupsInHistoricalHealFixture(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v168-no-groups?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (167)`)

	require.NoError(t, migrateV167toV168(d))
	assert.Equal(t, 168, schemaVersion(d))
}
