package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV41toV42_AddsGroupTemplates seeds a bare v41 schema_version
// row, runs the v42 migration, and asserts the two template tables land
// and accept a template + agent row.
func TestMigrateV41toV42_AddsGroupTemplates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v41.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (41);
	`)
	require.NoError(t, err, "seed v41 schema_version")

	require.NoError(t, migrateV41toV42(d), "migrateV41toV42")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 42, ver, "schema_version after migration")

	// A template + an agent insert cleanly post-migration.
	res, err := d.Exec(`
		INSERT INTO group_templates (name, descr, default_context, created_at, updated_at)
		VALUES ('feature-team', 'a team', 'shared context', '2026-05-16T00:00:00Z', '2026-05-16T00:00:00Z')`)
	require.NoError(t, err, "insert template")
	tid, err := res.LastInsertId()
	require.NoError(t, err)

	_, err = d.Exec(`
		INSERT INTO group_template_agents
			(template_id, ordinal, name, role, descr, initial_message, is_owner, permissions)
		VALUES (?, 0, 'PO', 'product-owner', 'the owner', 'lead the team', 1, '["groups.spawn"]')`,
		tid)
	require.NoError(t, err, "insert template agent")

	// The UNIQUE constraint on name rejects a duplicate template.
	_, err = d.Exec(`
		INSERT INTO group_templates (name, descr, default_context, created_at, updated_at)
		VALUES ('feature-team', '', '', '2026-05-16T00:00:00Z', '2026-05-16T00:00:00Z')`)
	require.Error(t, err, "duplicate template name rejected by UNIQUE constraint")
}

// TestMigrateV41toV42_FreshSchema builds a fresh DB through the full
// migrate() chain and confirms the template tables exist, the schema is
// at currentVersion, and ON DELETE CASCADE drops a template's agents
// with it (foreign_keys is enabled in the DSN).
func TestMigrateV41toV42_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 42, currentVersion, "currentVersion is 42")

	res, err := d.Exec(`
		INSERT INTO group_templates (name, descr, default_context, created_at, updated_at)
		VALUES ('t', '', '', '2026-05-16T00:00:00Z', '2026-05-16T00:00:00Z')`)
	require.NoError(t, err, "insert template")
	tid, err := res.LastInsertId()
	require.NoError(t, err)
	_, err = d.Exec(`
		INSERT INTO group_template_agents (template_id, name) VALUES (?, 'a1')`, tid)
	require.NoError(t, err, "insert agent")

	// Deleting the template cascades to its agent rows.
	_, err = d.Exec(`DELETE FROM group_templates WHERE id = ?`, tid)
	require.NoError(t, err, "delete template")
	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM group_template_agents WHERE template_id = ?`, tid).Scan(&n))
	assert.Equal(t, 0, n, "agent rows cascade-deleted with the template")
}
