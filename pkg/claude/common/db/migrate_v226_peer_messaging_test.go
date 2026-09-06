package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func v225PeerMessagingFixture(t *testing.T) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v225.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (225);
		CREATE TABLE sessions (id TEXT PRIMARY KEY, auto_memory INTEGER NOT NULL DEFAULT 0);
		CREATE TABLE spawn_profiles (id INTEGER PRIMARY KEY, name TEXT NOT NULL, auto_memory INTEGER);
		INSERT INTO sessions (id) VALUES ('legacy-session');
		INSERT INTO spawn_profiles (id, name) VALUES (1, 'legacy-profile');
	`)
	return d
}

func TestMigrateV226AddsPeerMessagingColumns(t *testing.T) {
	d := v225PeerMessagingFixture(t)
	require.NoError(t, migrateV225toV226(d))

	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 226, version)

	// A legacy session row reads as peer messaging OFF, which is the posture
	// tclaude now launches every Claude Code session with — so a resumed legacy
	// session converges on the new default rather than keeping a channel open
	// that nobody chose.
	var sessionPosture int
	require.NoError(t, d.QueryRow(
		`SELECT peer_messaging FROM sessions WHERE id = 'legacy-session'`).Scan(&sessionPosture))
	assert.Equal(t, 0, sessionPosture)

	// A legacy profile stays UNSET (NULL) rather than pinned, so it keeps
	// deferring to whatever the default is — the tri-state the profile layer
	// depends on.
	var profilePosture sql.NullInt64
	require.NoError(t, d.QueryRow(
		`SELECT peer_messaging FROM spawn_profiles WHERE id = 1`).Scan(&profilePosture))
	assert.False(t, profilePosture.Valid, "an unset profile must stay NULL, not resolve to a pinned 0")
}

func TestMigrateV226ConvergesOnRerun(t *testing.T) {
	d := v225PeerMessagingFixture(t)
	require.NoError(t, migrateV225toV226(d))
	mustExec(t, d, `UPDATE schema_version SET version = 225`)
	require.NoError(t, migrateV225toV226(d), "a half-applied migration must converge on re-run")
}

// The probe guards exist so a fixture missing one of the tables migrates
// cleanly rather than aborting the whole upgrade.
func TestMigrateV226SkipsMissingTables(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "bare.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (225);
	`)
	require.NoError(t, migrateV225toV226(d))
}
