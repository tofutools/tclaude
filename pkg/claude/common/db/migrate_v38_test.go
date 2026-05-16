package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV37toV38_SanitizesSlashNames builds a v37-shape
// agent_groups table seeded with the kind of poison data that group
// create used to allow — names containing a forward or back slash —
// runs the v38 migration, and asserts every slashed name is folded to a
// slash-free one, with numeric suffixes resolving UNIQUE collisions.
// Clean names and the integer ids (the foreign keys every other table
// references) are left untouched.
func TestMigrateV37toV38_SanitizesSlashNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v37.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// Minimal pre-v38 schema: schema_version + agent_groups. The
	// migration only reads/writes agent_groups, so nothing else is
	// needed. Seeded rows cover: a clean name (untouched), a plain
	// slashed name, a slashed name whose sanitized form collides with
	// an existing clean group, a backslash name, and two differently-
	// slashed names that fold to the same base (one keeps the base,
	// the other takes a suffix).
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (37);
		CREATE TABLE agent_groups (
			id   INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE
		);
		INSERT INTO agent_groups (id, name) VALUES
			(1, 'normal'),
			(2, 'team/sub'),
			(3, 'a-b'),
			(4, 'a/b'),
			(5, 'win\path'),
			(6, 'x/y'),
			(7, 'x\y');
	`)
	require.NoError(t, err, "seed v37 schema")

	require.NoError(t, migrateV37toV38(d), "migrateV37toV38")

	// schema_version bumped to 38.
	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 38, ver, "schema_version after migration")

	// Read back the (id → name) mapping.
	byID := map[int64]string{}
	rows, err := d.Query(`SELECT id, name FROM agent_groups`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var name string
		require.NoError(t, rows.Scan(&id, &name))
		byID[id] = name
	}
	require.NoError(t, rows.Err())

	// ids are stable (foreign keys elsewhere keep pointing at them) and
	// every name is now slash-free, collision-resolved.
	assert.Equal(t, "normal", byID[1], "clean name untouched")
	assert.Equal(t, "team-sub", byID[2], "plain slashed name folded")
	assert.Equal(t, "a-b", byID[3], "pre-existing clean name untouched")
	assert.Equal(t, "a-b-2", byID[4], "sanitized name collided with id 3 → suffixed")
	assert.Equal(t, "win-path", byID[5], "backslash folded")
	assert.Equal(t, "x-y", byID[6], "lower id keeps the bare sanitized name")
	assert.Equal(t, "x-y-2", byID[7], "later same-base name takes the suffix")

	for id, name := range byID {
		assert.NotContains(t, name, "/", "group %d still has a slash: %q", id, name)
		assert.NotContains(t, name, `\`, "group %d still has a backslash: %q", id, name)
	}
}

// TestMigrateV37toV38_NoSlashesIsNoop confirms the migration is a clean
// no-op (beyond the version bump) when no group name needs repair —
// the common case on every healthy database.
func TestMigrateV37toV38_NoSlashesIsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v37-clean.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (37);
		CREATE TABLE agent_groups (
			id   INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE
		);
		INSERT INTO agent_groups (id, name) VALUES (1, 'alpha'), (2, 'beta-team');
	`)
	require.NoError(t, err, "seed clean v37 schema")

	require.NoError(t, migrateV37toV38(d), "migrateV37toV38")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 38, ver, "schema_version after migration")

	var alpha, beta string
	require.NoError(t, d.QueryRow(`SELECT name FROM agent_groups WHERE id = 1`).Scan(&alpha))
	require.NoError(t, d.QueryRow(`SELECT name FROM agent_groups WHERE id = 2`).Scan(&beta))
	assert.Equal(t, "alpha", alpha, "clean name untouched")
	assert.Equal(t, "beta-team", beta, "clean name untouched")
}
