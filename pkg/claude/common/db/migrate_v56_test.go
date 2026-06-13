package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV55toV56_AddsDashboardPrefs seeds a bare v55 DB, runs the
// v56 migration, and asserts the dashboard_prefs table lands and accepts
// an upsert.
func TestMigrateV55toV56_AddsDashboardPrefs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v55.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (55);
	`)
	require.NoError(t, err, "seed v55 schema")

	require.NoError(t, migrateV55toV56(d), "migrateV55toV56")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 56, ver, "schema_version after migration")

	_, err = d.Exec(
		`INSERT INTO dashboard_prefs (key, value, updated_at) VALUES ('tclaude.dash.sort', '{"col":"name"}', '2026-06-13T00:00:00Z')`)
	require.NoError(t, err, "dashboard_prefs accepts an upsert")
}

// TestMigrateV55toV56_HealsHalfAppliedRun guards the wedge class: an
// interrupted earlier attempt created the table but never bumped
// schema_version. CREATE TABLE IF NOT EXISTS makes the re-run converge —
// existing rows survive and the version finally lands on 56.
func TestMigrateV55toV56_HealsHalfAppliedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v55-half.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// Half-applied: table already there with a row, version still 55.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (55);
		CREATE TABLE dashboard_prefs (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		INSERT INTO dashboard_prefs (key, value, updated_at) VALUES ('tclaude.dash.group.x', '1', '2026-06-01T00:00:00Z');
	`)
	require.NoError(t, err, "seed half-applied v55 schema")

	require.NoError(t, migrateV55toV56(d), "re-run must converge, not fail on existing table")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 56, ver, "schema_version finally lands on 56")

	var value string
	require.NoError(t, d.QueryRow(`SELECT value FROM dashboard_prefs WHERE key = 'tclaude.dash.group.x'`).Scan(&value))
	assert.Equal(t, "1", value, "existing row survives the healing run")

	// A second run through migrate() is a clean no-op: version matches.
	require.NoError(t, migrate(d), "migrate() on the healed DB")
}

// TestMigrateV55toV56_FreshSchemaRoundTrips builds a fresh DB through the
// full migrate() chain and round-trips a pref through the production
// Set/List/Delete helpers. Carries the literal currentVersion pin — a
// tripwire the next migration's author moves forward into their own v57
// test.
func TestMigrateV55toV56_FreshSchemaRoundTrips(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 56, currentVersion, "currentVersion is 56")

	// Upsert two keys, overwrite one, delete the other; List reflects it.
	require.NoError(t, SetDashboardPref("tclaude.dash.sort", `{"col":"name"}`))
	require.NoError(t, SetDashboardPref("tclaude.dash.group.x", "1"))
	require.NoError(t, SetDashboardPref("tclaude.dash.group.x", "0"), "upsert overwrites")

	prefs, err := ListDashboardPrefs()
	require.NoError(t, err, "ListDashboardPrefs")
	assert.Equal(t, `{"col":"name"}`, prefs["tclaude.dash.sort"])
	assert.Equal(t, "0", prefs["tclaude.dash.group.x"], "overwritten value wins")

	require.NoError(t, DeleteDashboardPref("tclaude.dash.group.x"))
	prefs, err = ListDashboardPrefs()
	require.NoError(t, err, "ListDashboardPrefs after delete")
	_, present := prefs["tclaude.dash.group.x"]
	assert.False(t, present, "deleted key is gone")
	assert.Equal(t, `{"col":"name"}`, prefs["tclaude.dash.sort"], "the other key is untouched")

	// Deleting a missing key is a no-op, like the dashboard's removeItem.
	require.NoError(t, DeleteDashboardPref("tclaude.dash.nonexistent"))
}
