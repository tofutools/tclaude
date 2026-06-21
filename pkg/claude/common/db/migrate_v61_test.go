package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV60toV61_AddsSpawnProfiles seeds a bare v60 DB, runs the v61
// migration, and asserts the spawn_profiles table lands and accepts an insert.
func TestMigrateV60toV61_AddsSpawnProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v60.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (60);
	`)
	require.NoError(t, err, "seed v60 schema")

	require.NoError(t, migrateV60toV61(d), "migrateV60toV61")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 61, ver, "schema_version after migration")

	// NULL toggles are accepted (the unset tri-state).
	_, err = d.Exec(
		`INSERT INTO spawn_profiles (name, created_at, updated_at) VALUES ('p1', '2026-06-17T00:00:00Z', '2026-06-17T00:00:00Z')`)
	require.NoError(t, err, "spawn_profiles accepts an insert with NULL toggles")
}

// TestMigrateV60toV61_HealsHalfAppliedRun guards the wedge class: an
// interrupted earlier attempt created the table but never bumped
// schema_version. CREATE TABLE IF NOT EXISTS makes the re-run converge —
// existing rows survive and the version finally lands on 61.
func TestMigrateV60toV61_HealsHalfAppliedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v60-half.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// Half-applied: table already there with a row, version still 60.
	// A minimal sessions table is seeded too so the final migrate()-to-head
	// reaches the v65 sessions.remote_control ALTER (added after v58, the last
	// prior sessions-ALTER this v60 seed used to skip — see migrateV64toV65).
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (60);
		CREATE TABLE sessions (id TEXT PRIMARY KEY);
		CREATE TABLE spawn_profiles (
			id                            INTEGER PRIMARY KEY AUTOINCREMENT,
			name                          TEXT NOT NULL UNIQUE,
			harness                       TEXT NOT NULL DEFAULT '',
			model                         TEXT NOT NULL DEFAULT '',
			effort                        TEXT NOT NULL DEFAULT '',
			sandbox                       TEXT NOT NULL DEFAULT '',
			approval                      TEXT NOT NULL DEFAULT '',
			auto_review                   INTEGER,
			trust_dir                     INTEGER,
			agent_name                    TEXT NOT NULL DEFAULT '',
			role                          TEXT NOT NULL DEFAULT '',
			descr                         TEXT NOT NULL DEFAULT '',
			initial_message               TEXT NOT NULL DEFAULT '',
			sync_worktree                 INTEGER,
			auto_focus                    INTEGER,
			include_group_default_context INTEGER,
			created_at                    TEXT NOT NULL,
			updated_at                    TEXT NOT NULL
		);
		INSERT INTO spawn_profiles (name, created_at, updated_at) VALUES ('seeded', '2026-06-01T00:00:00Z', '2026-06-01T00:00:00Z');
	`)
	require.NoError(t, err, "seed half-applied v60 schema")

	require.NoError(t, migrateV60toV61(d), "re-run must converge, not fail on existing table")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 61, ver, "schema_version finally lands on 61")

	var name string
	require.NoError(t, d.QueryRow(`SELECT name FROM spawn_profiles WHERE name = 'seeded'`).Scan(&name))
	assert.Equal(t, "seeded", name, "existing row survives the healing run")

	require.NoError(t, migrate(d), "migrate() advances the healed DB to head")
}

// TestMigrateV60toV61_FreshSchemaRoundTrips builds a fresh DB through the full
// migrate() chain and round-trips a profile through the production CRUD
// helpers. (The literal currentVersion tripwire moved forward to the v62 test.)
func TestMigrateV60toV61_FreshSchemaRoundTrips(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

	id, err := CreateSpawnProfile(&SpawnProfile{Name: "codex-high", Harness: "codex", Effort: "high"})
	require.NoError(t, err, "CreateSpawnProfile")
	require.NotZero(t, id)

	got, err := GetSpawnProfile("codex-high")
	require.NoError(t, err, "GetSpawnProfile")
	require.NotNil(t, got)
	assert.Equal(t, "codex", got.Harness)
	assert.Equal(t, "high", got.Effort)
}
