package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV155toV156AddsAutoCompactWindowColumns(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	mustExec(t, d, `ALTER TABLE spawn_profiles DROP COLUMN auto_compact_window`)
	mustExec(t, d, `ALTER TABLE sessions DROP COLUMN auto_compact_window`)
	mustExec(t, d, `UPDATE schema_version SET version = 155`)
	mustExec(t, d, `INSERT INTO spawn_profiles (name, created_at, updated_at)
		VALUES ('legacy-profile', '2026-07-25T09:00:00Z', '2026-07-25T09:00:00Z')`)
	mustExec(t, d, `INSERT INTO sessions (id, tmux_session, pid, cwd, conv_id, status, created_at, updated_at)
		VALUES ('legacy-session', 'tc-legacy', 0, '/tmp', 'conv-legacy', 'idle',
		        '2026-07-25T09:00:00Z', '2026-07-25T09:00:00Z')`)

	require.NoError(t, migrateV155toV156(d))

	// A legacy row reads as "no window pinned", which is the model-default
	// compaction threshold every pre-v156 agent actually launched with.
	var profileWindow, sessionWindow string
	require.NoError(t, d.QueryRow(
		`SELECT auto_compact_window FROM spawn_profiles WHERE name = 'legacy-profile'`).Scan(&profileWindow))
	require.NoError(t, d.QueryRow(
		`SELECT auto_compact_window FROM sessions WHERE id = 'legacy-session'`).Scan(&sessionWindow))
	assert.Empty(t, profileWindow)
	assert.Empty(t, sessionWindow)

	assert.Equal(t, 156, schemaVersion(d))
	require.NoError(t, migrateV155toV156(d), "partially applied migration converges")
}

// TestSetSessionAutoCompactWindowRoundTrips covers the two writers that share
// the sessions column: the launch path, which may assert "nothing pinned", and
// the status line, which may only ever ADD a window it observed.
func TestSetSessionAutoCompactWindowRoundTrips(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `INSERT INTO sessions (id, tmux_session, pid, cwd, conv_id, status, created_at, updated_at)
		VALUES ('s1', 'tc-s1', 0, '/tmp', 'conv-1', 'idle',
		        '2026-07-25T09:00:00Z', '2026-07-25T09:00:00Z')`)

	read := func() string {
		var got string
		require.NoError(t, d.QueryRow(`SELECT auto_compact_window FROM sessions WHERE id = 's1'`).Scan(&got))
		return got
	}

	require.NoError(t, SetSessionAutoCompactWindow("s1", "450000"))
	assert.Equal(t, "450000", read())

	// The observer may correct an out-of-date pin...
	require.NoError(t, UpdateSessionAutoCompactWindow("s1", "500000"))
	assert.Equal(t, "500000", read())

	// ...but must never erase one, or a relaunch would silently lose the window
	// the launch deliberately recorded.
	require.NoError(t, UpdateSessionAutoCompactWindow("s1", ""))
	assert.Equal(t, "500000", read())

	// The launch path IS allowed to assert "nothing pinned".
	require.NoError(t, SetSessionAutoCompactWindow("s1", ""))
	assert.Empty(t, read())
}

func TestMigrateV156IsTheCurrentHead(t *testing.T) {
	require.Equal(t, 156, currentVersion, "tripwire: bump this with the next migration")
}
