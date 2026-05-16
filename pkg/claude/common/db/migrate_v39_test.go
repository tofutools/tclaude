package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV38toV39_AddsEffectColumn builds a v38-shape
// agent_permissions table seeded with the kind of rows that existed
// before the permanent-permission editor — plain per-conv grants — runs
// the v39 migration, and asserts the new `effect` column lands with
// every existing row backfilled to 'grant'. Pre-v39 every row was, by
// construction, an additive grant.
func TestMigrateV38toV39_AddsEffectColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v38.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// Minimal pre-v39 schema: schema_version + the v9-shape
	// agent_permissions table (no effect column).
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (38);
		CREATE TABLE agent_permissions (
			conv_id    TEXT NOT NULL,
			slug       TEXT NOT NULL,
			granted_at TEXT NOT NULL,
			granted_by TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (conv_id, slug)
		);
		INSERT INTO agent_permissions (conv_id, slug, granted_at, granted_by) VALUES
			('conv-1', 'groups.spawn', '2026-01-01T00:00:00Z', '<human>'),
			('conv-2', 'groups.create', '2026-01-02T00:00:00Z', '<human>');
	`)
	require.NoError(t, err, "seed v38 schema")

	require.NoError(t, migrateV38toV39(d), "migrateV38toV39")

	// schema_version bumped to 39.
	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 39, ver, "schema_version after migration")

	// Every pre-existing row backfilled to 'grant'.
	effects := map[string]string{}
	rows, err := d.Query(`SELECT conv_id, effect FROM agent_permissions`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var conv, effect string
		require.NoError(t, rows.Scan(&conv, &effect))
		effects[conv] = effect
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, "grant", effects["conv-1"], "existing row backfilled to grant")
	assert.Equal(t, "grant", effects["conv-2"], "existing row backfilled to grant")

	// The CHECK constraint accepts 'deny' and rejects anything else.
	_, err = d.Exec(`INSERT INTO agent_permissions (conv_id, slug, effect, granted_at, granted_by)
		VALUES ('conv-3', 'self.rename', 'deny', '2026-01-03T00:00:00Z', '<human>')`)
	require.NoError(t, err, "deny effect must be accepted")
	_, err = d.Exec(`INSERT INTO agent_permissions (conv_id, slug, effect, granted_at, granted_by)
		VALUES ('conv-4', 'self.rename', 'maybe', '2026-01-04T00:00:00Z', '<human>')`)
	assert.Error(t, err, "an effect other than grant/deny must violate the CHECK constraint")
}
