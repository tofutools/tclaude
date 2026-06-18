package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// seedV61ForProfileMigration builds a bare v61 DB carrying the spawn_profiles
// table (v61) and a minimal agent_groups table WITHOUT the default_profile
// column — the precondition the v62 migration adds it to. Only the columns the
// migration reads/writes are present; the real agent_groups has more, but a
// migration test needs only what the migration touches.
func seedV61ForProfileMigration(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v61.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	t.Cleanup(func() { _ = d.Close() })

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (61);

		CREATE TABLE agent_groups (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT NOT NULL UNIQUE,
			default_model TEXT NOT NULL DEFAULT ''
		);

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
	`)
	require.NoError(t, err, "seed v61 schema")
	return d
}

// TestMigrateV61toV62_AddsColumnAndSynthesizes seeds a v61 DB with a group
// carrying a legacy default_model and a group with none, runs the v62
// migration, and asserts: the default_profile column lands, the group with a
// model gets a synthesized claude profile it now points at, and the model-less
// group is left untouched.
func TestMigrateV61toV62_AddsColumnAndSynthesizes(t *testing.T) {
	d := seedV61ForProfileMigration(t)

	_, err := d.Exec(`
		INSERT INTO agent_groups (name, default_model) VALUES ('team', 'sonnet');
		INSERT INTO agent_groups (name, default_model) VALUES ('solo', '');
	`)
	require.NoError(t, err, "seed groups")

	require.NoError(t, migrateV61toV62(d), "migrateV61toV62")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 62, ver, "schema_version after migration")

	var haveCol int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'default_profile'`,
	).Scan(&haveCol))
	assert.Equal(t, 1, haveCol, "default_profile column is added")

	// The group with a legacy model now points at a synthesized profile.
	var profileName string
	require.NoError(t, d.QueryRow(`SELECT default_profile FROM agent_groups WHERE name = 'team'`).Scan(&profileName))
	assert.Equal(t, "group-default-team", profileName, "team points at its synthesized profile")

	var harness, model string
	require.NoError(t, d.QueryRow(
		`SELECT harness, model FROM spawn_profiles WHERE name = 'group-default-team'`,
	).Scan(&harness, &model))
	assert.Equal(t, "claude", harness, "synthesized profile is a claude profile")
	assert.Equal(t, "sonnet", model, "synthesized profile carries the legacy model")

	// The model-less group is left with no default profile.
	var soloProfile string
	require.NoError(t, d.QueryRow(`SELECT default_profile FROM agent_groups WHERE name = 'solo'`).Scan(&soloProfile))
	assert.Equal(t, "", soloProfile, "model-less group gets no synthesized profile")
}

// TestMigrateV61toV62_SynthesisDedupesName guards the UNIQUE(name) collision:
// when a human-made profile already holds "group-default-<group>", the
// synthesized one takes a numeric suffix instead of failing the migration.
func TestMigrateV61toV62_SynthesisDedupesName(t *testing.T) {
	d := seedV61ForProfileMigration(t)

	_, err := d.Exec(`
		INSERT INTO agent_groups (name, default_model) VALUES ('team', 'opus');
		INSERT INTO spawn_profiles (name, created_at, updated_at)
			VALUES ('group-default-team', '2026-06-01T00:00:00Z', '2026-06-01T00:00:00Z');
	`)
	require.NoError(t, err, "seed group + colliding profile name")

	require.NoError(t, migrateV61toV62(d), "migrateV61toV62 dedupes the name")

	var profileName string
	require.NoError(t, d.QueryRow(`SELECT default_profile FROM agent_groups WHERE name = 'team'`).Scan(&profileName))
	assert.Equal(t, "group-default-team-2", profileName, "synthesized profile takes a -2 suffix")

	var model string
	require.NoError(t, d.QueryRow(
		`SELECT model FROM spawn_profiles WHERE name = 'group-default-team-2'`,
	).Scan(&model))
	assert.Equal(t, "opus", model, "the suffixed profile carries the legacy model")
}

// TestMigrateV61toV62_HealsHalfAppliedRun guards the converge-on-re-run
// property: a prior attempt added the column and converted a group but never
// bumped schema_version. The re-run must not re-synthesize (the group already
// has a default_profile) nor wedge on "duplicate column name" — the version
// finally lands on 62 and no second profile appears.
func TestMigrateV61toV62_HealsHalfAppliedRun(t *testing.T) {
	d := seedV61ForProfileMigration(t)

	// Half-applied: column already added, group already converted, but version
	// still 61.
	_, err := d.Exec(`
		ALTER TABLE agent_groups ADD COLUMN default_profile TEXT NOT NULL DEFAULT '';
		INSERT INTO agent_groups (name, default_model, default_profile)
			VALUES ('team', 'sonnet', 'group-default-team');
		INSERT INTO spawn_profiles (name, harness, model, created_at, updated_at)
			VALUES ('group-default-team', 'claude', 'sonnet', '2026-06-01T00:00:00Z', '2026-06-01T00:00:00Z');
	`)
	require.NoError(t, err, "seed half-applied v61 state")

	require.NoError(t, migrateV61toV62(d), "re-run must converge, not fail on existing column")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 62, ver, "schema_version finally lands on 62")

	// Exactly one synthesized profile — the heal did not re-synthesize.
	var count int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM spawn_profiles WHERE name LIKE 'group-default-team%'`,
	).Scan(&count))
	assert.Equal(t, 1, count, "no duplicate profile synthesized on re-run")

	require.NoError(t, migrateV61toV62(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 62, ver, "second re-run stays at 62")
}

// TestMigrateV61toV62_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts agent_groups carries the default_profile column. v62 is
// head, so this is where the literal currentVersion pin lives — the tripwire
// the next migration's author moves forward into their own v63 test.
func TestMigrateV61toV62_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	// The literal currentVersion tripwire, moved forward from the v61 test —
	// the next migration's author moves it into their own v63 test.
	require.Equal(t, 62, currentVersion, "currentVersion is 62")

	var haveCol int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'default_profile'`,
	).Scan(&haveCol))
	assert.Equal(t, 1, haveCol, "fresh schema has the default_profile column")
}
