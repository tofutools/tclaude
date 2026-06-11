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

// TestMigrateV51toV52_FreshSchemaRoundTrips builds a fresh DB through
// the full migrate() chain and round-trips a group default model
// through the production setter + getter. Carries the literal
// currentVersion pin — a tripwire the next migration's author moves
// forward into their own v53 test.
func TestMigrateV51toV52_FreshSchemaRoundTrips(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 52, currentVersion, "currentVersion is 52")

	_, err = CreateAgentGroup("g", "")
	require.NoError(t, err, "CreateAgentGroup")

	n, err := SetAgentGroupDefaultModel("g", "sonnet")
	require.NoError(t, err, "SetAgentGroupDefaultModel")
	require.EqualValues(t, 1, n, "one row updated")

	g, err := GetAgentGroupByName("g")
	require.NoError(t, err, "GetAgentGroupByName")
	require.NotNil(t, g)
	assert.Equal(t, "sonnet", g.DefaultModel, "default model round-trips")

	// Clearing: "" stores and reads back as unset.
	n, err = SetAgentGroupDefaultModel("g", "")
	require.NoError(t, err, "clear default model")
	require.EqualValues(t, 1, n)
	g, err = GetAgentGroupByName("g")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, "", g.DefaultModel, "cleared default reads back unset")

	// Unknown group: 0 rows so the HTTP layer can 404.
	n, err = SetAgentGroupDefaultModel("nope", "opus")
	require.NoError(t, err)
	assert.EqualValues(t, 0, n, "unknown group affects 0 rows")
}
