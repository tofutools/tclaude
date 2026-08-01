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
		VALUES ('legacy-profile', 1784970000000000000, 1784970000000000000)`)
	mustExec(t, d, `INSERT INTO sessions (id, tmux_session, pid, cwd, conv_id, status, created_at, updated_at)
		VALUES ('legacy-session', 'tc-legacy', 0, '/tmp', 'conv-legacy', 'idle',
		        1784970000000000000, 1784970000000000000)`)

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
		        1784970000000000000, 1784970000000000000)`)

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

// TestGetSessionAutoCompactWindow covers the read the status line uses when its
// hook process did not inherit the pane's CLAUDE_CODE_AUTO_COMPACT_WINDOW.
//
// The three "" cases matter more than the hit: an unpinned agent is the COMMON
// case, and a missing row / absent session id happen routinely (an unmanaged
// conversation, a render before the row exists). All three must read as "nothing
// pinned" with no error, so the caller leaves the model's own window — and Claude
// Code's own percentage — alone.
func TestGetSessionAutoCompactWindow(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `INSERT INTO sessions (id, tmux_session, pid, cwd, conv_id, status,
		created_at, updated_at, auto_compact_window)
		VALUES ('pinned', 'tc-pinned', 0, '/tmp', 'conv-pinned', 'idle',
		        1784970000000000000, 1784970000000000000, '450000')`)
	mustExec(t, d, `INSERT INTO sessions (id, tmux_session, pid, cwd, conv_id, status,
		created_at, updated_at)
		VALUES ('unpinned', 'tc-unpinned', 0, '/tmp', 'conv-unpinned', 'idle',
		        1784970000000000000, 1784970000000000000)`)

	got, err := GetSessionAutoCompactWindow("pinned")
	require.NoError(t, err)
	assert.Equal(t, "450000", got, "recorded window reads back canonically")

	for _, id := range []string{"unpinned", "no-such-session", "", "   "} {
		got, err := GetSessionAutoCompactWindow(id)
		require.NoError(t, err, "id %q must not error", id)
		assert.Empty(t, got, "id %q reads as nothing pinned", id)
	}
}
