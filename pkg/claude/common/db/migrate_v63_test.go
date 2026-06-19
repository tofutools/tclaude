package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV62toV63_DropsDefaultModel seeds a bare v62 DB whose
// agent_groups table still carries the vestigial Claude-only default_model
// column, runs the v63 migration, and asserts the column is gone, the
// surviving columns keep their values, and the version lands on 63.
func TestMigrateV62toV63_DropsDefaultModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v62.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (62);
		CREATE TABLE agent_groups (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			name            TEXT NOT NULL UNIQUE,
			default_model   TEXT NOT NULL DEFAULT '',
			default_profile TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO agent_groups (name, default_model, default_profile)
			VALUES ('team', 'sonnet', 'group-default-team');
	`)
	require.NoError(t, err, "seed v62 schema")

	require.NoError(t, migrateV62toV63(d), "migrateV62toV63")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 63, ver, "schema_version after migration")

	var haveCol int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'default_model'`,
	).Scan(&haveCol))
	assert.Equal(t, 0, haveCol, "default_model column is dropped")

	// The surviving column keeps its value through the drop.
	var profile string
	require.NoError(t, d.QueryRow(`SELECT default_profile FROM agent_groups WHERE name = 'team'`).Scan(&profile))
	assert.Equal(t, "group-default-team", profile, "default_profile survives the column drop")
}

// TestMigrateV62toV63_HealsMissingColumn guards the converge-on-re-run
// property: a DB whose agent_groups never had default_model (or a re-run
// after a prior successful drop) must not wedge on "no such column" — the
// pragma_table_info probe skips the DROP and the version still lands on 63.
// A second re-run is a clean no-op.
func TestMigrateV62toV63_HealsMissingColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v62-nocol.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// agent_groups exists but already lacks default_model.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (62);
		CREATE TABLE agent_groups (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			name            TEXT NOT NULL UNIQUE,
			default_profile TEXT NOT NULL DEFAULT ''
		);
	`)
	require.NoError(t, err, "seed v62 schema without default_model")

	require.NoError(t, migrateV62toV63(d), "re-run must converge, not fail on missing column")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 63, ver, "schema_version finally lands on 63")

	require.NoError(t, migrateV62toV63(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 63, ver, "second re-run stays at 63")
}

// TestMigrateV62toV63_HealsMissingTable guards a minimally-seeded
// migration-heal DB advancing past versions that predate agent_groups: the
// migration tolerates the table's absence and still lands the version.
func TestMigrateV62toV63_HealsMissingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v62-notable.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (62);
	`)
	require.NoError(t, err, "seed v62 schema without agent_groups")

	require.NoError(t, migrateV62toV63(d), "missing agent_groups must not wedge the migration")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 63, ver, "schema_version lands on 63 even with no agent_groups")
}

// TestMigrateV62toV63_FreshSchema builds a fresh DB through the full
// migrate() chain and asserts agent_groups has no default_model column. v63
// is head, so this is where the literal currentVersion pin lives — the
// tripwire the next migration's author moves forward into their own v64 test.
func TestMigrateV62toV63_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	// The literal currentVersion tripwire, moved forward from the v62 test —
	// the next migration's author moves it into their own v64 test.
	require.Equal(t, 63, currentVersion, "currentVersion is 63")

	var haveCol int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'default_model'`,
	).Scan(&haveCol))
	assert.Equal(t, 0, haveCol, "fresh schema has no default_model column")
}
