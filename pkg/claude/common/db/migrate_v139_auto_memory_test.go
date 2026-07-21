package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV138toV139AddsAutoMemoryColumns(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v139?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (138)`)
	mustExec(t, d, `CREATE TABLE spawn_profiles (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	mustExec(t, d, `INSERT INTO spawn_profiles (id, name) VALUES (1, 'legacy')`)
	mustExec(t, d, `CREATE TABLE sessions (id TEXT PRIMARY KEY)`)
	mustExec(t, d, `INSERT INTO sessions (id) VALUES ('legacy-sess')`)

	require.NoError(t, migrateV138toV139(d))
	assert.Equal(t, 139, schemaVersion(d))
	require.NoError(t, migrateV138toV139(d), "migration converges after a partial application")

	// A legacy profile reads NULL: "unset", which the spawn path resolves to
	// off. Distinct from an explicit 0, which the tri-state also allows.
	var profileMem sql.NullInt64
	require.NoError(t, d.QueryRow(`SELECT auto_memory FROM spawn_profiles WHERE id = 1`).Scan(&profileMem))
	assert.False(t, profileMem.Valid, "legacy profile auto_memory is unset, not an explicit value")

	// A legacy session reads 0 = memory off, which is the posture a resumed
	// legacy session should get.
	var sessionMem int
	require.NoError(t, d.QueryRow(`SELECT auto_memory FROM sessions WHERE id = 'legacy-sess'`).Scan(&sessionMem))
	assert.Zero(t, sessionMem, "legacy session resumes with auto memory off")
}

// TestMigrateV138toV139SkipsMissingTables covers the probe guards: a DB where
// neither table exists still bumps the version rather than failing.
func TestMigrateV138toV139SkipsMissingTables(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v139-bare?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (138)`)

	require.NoError(t, migrateV138toV139(d))
	assert.Equal(t, 139, schemaVersion(d))
}

// TestSpawnProfileAutoMemoryRoundTrips pins the tri-state through the real
// CRUD path: unset stays NULL, and both explicit values survive a write/read.
func TestSpawnProfileAutoMemoryRoundTrips(t *testing.T) {
	setupTestDB(t)

	on, off := true, false
	for _, tc := range []struct {
		name string
		want *bool
	}{
		{"mem-unset", nil},
		{"mem-on", &on},
		{"mem-off", &off},
	} {
		_, err := CreateSpawnProfile(&SpawnProfile{Name: tc.name, AutoMemory: tc.want})
		require.NoError(t, err, tc.name)
		got, err := GetSpawnProfile(tc.name)
		require.NoError(t, err, tc.name)
		require.NotNil(t, got, tc.name)
		if tc.want == nil {
			assert.Nil(t, got.AutoMemory, tc.name)
			continue
		}
		require.NotNil(t, got.AutoMemory, tc.name)
		assert.Equal(t, *tc.want, *got.AutoMemory, tc.name)
	}
}

// TestSessionAutoMemoryOutOfBand pins the discipline that keeps the recorded
// posture alive: SetSessionAutoMemory writes it, and a later SaveSession (the
// shape every state-tracking hook tick uses) must NOT clobber it back to false.
func TestSessionAutoMemoryOutOfBand(t *testing.T) {
	setupTestDB(t)

	row := &SessionRow{ID: "sess-mem", TmuxSession: "tmux-mem", ConvID: "conv-mem", Status: "running"}
	require.NoError(t, SaveSession(row))
	require.NoError(t, SetSessionAutoMemory("sess-mem", true))

	got, err := LoadSession("sess-mem")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.AutoMemory, "recorded posture reads back")

	// A hook-shaped upsert that knows nothing about auto memory.
	require.NoError(t, SaveSession(&SessionRow{
		ID: "sess-mem", TmuxSession: "tmux-mem", ConvID: "conv-mem", Status: "idle",
	}))
	got, err = LoadSession("sess-mem")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.AutoMemory, "a hook tick must not reset the recorded posture")

	byConv, err := AutoMemoryForConv("conv-mem")
	require.NoError(t, err)
	assert.True(t, byConv)

	// An unknown conv degrades to the recommended posture rather than erroring.
	missing, err := AutoMemoryForConv("conv-does-not-exist")
	require.NoError(t, err)
	assert.False(t, missing)
}
