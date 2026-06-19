package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV51toV52_AddsDefaultModel seeds a bare v51 agent_groups
// table with an existing row, runs the v52 migration, and asserts the
// new default_model column lands with '' on the pre-existing group —
// "no group default, claude decides" — so old groups keep their exact
// spawn behaviour.
func TestMigrateV51toV52_AddsDefaultModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v51.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (51);
		CREATE TABLE agent_groups (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			name            TEXT NOT NULL UNIQUE,
			descr           TEXT NOT NULL DEFAULT '',
			default_cwd     TEXT NOT NULL DEFAULT '',
			default_context TEXT NOT NULL DEFAULT '',
			max_members     INTEGER NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL,
			archived_at     TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO agent_groups (name, created_at) VALUES ('pre-existing', '2026-01-01T00:00:00Z');
	`)
	require.NoError(t, err, "seed v51 schema")

	require.NoError(t, migrateV51toV52(d), "migrateV51toV52")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 52, ver, "schema_version after migration")

	var model string
	require.NoError(t, d.QueryRow(
		`SELECT default_model FROM agent_groups WHERE name = 'pre-existing'`).Scan(&model))
	assert.Equal(t, "", model, "pre-existing groups read back as unset")
}

// NOTE: the v52 default_model column is dropped at head by migrateV62toV63
// (JOH-220) — Spawn Profiles (default_profile) superseded it. The former
// fresh-schema round-trip test (production setter/getter for default_model)
// was retired with the column and its setter; the migration itself is still
// covered above on a raw v51 seed, and the column's removal is covered by the
// v63 migration test.
