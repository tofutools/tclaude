package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV82toV83_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it lands at currentVersion. The literal currentVersion
// tripwire moved forward to the v84 test (head).
func TestMigrateV82toV83_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
}

// TestMigrateV82toV83_AddsColumns drives the real v82→v83 ALTER over a v82-pinned
// DB: it asserts the birth-time access-control columns appear on BOTH tables
// (pending_spawns NOT NULL, spawn_profiles nullable is_owner), that an existing
// pending row reads back as "no owner / no overrides", that the version advances,
// and that a re-run is a clean no-op.
func TestMigrateV82toV83_AddsColumns(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v82 and drop the new columns so we re-add them from a true
	// v82 shape (the fresh chain already ran v83). SQLite supports DROP COLUMN.
	mustExec(t, d, `ALTER TABLE pending_spawns DROP COLUMN permission_overrides`)
	mustExec(t, d, `ALTER TABLE pending_spawns DROP COLUMN is_owner`)
	mustExec(t, d, `ALTER TABLE spawn_profiles DROP COLUMN permission_overrides`)
	mustExec(t, d, `ALTER TABLE spawn_profiles DROP COLUMN is_owner`)
	mustExec(t, d, `UPDATE schema_version SET version = 82`)

	// A pre-existing pending row (without the new columns) must survive the
	// ALTER and read back with the defaults.
	mustExec(t, d, `INSERT INTO pending_spawns (label, group_id, created_at) VALUES ('lbl', 1, '2026-06-29T00:00:00Z')`)

	require.NoError(t, migrateV82toV83(d), "v82→v83")

	hasCol := func(table, name string) bool {
		var n int
		require.NoError(t, d.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, name).Scan(&n))
		return n > 0
	}
	assert.True(t, hasCol("pending_spawns", "is_owner"), "pending_spawns.is_owner added")
	assert.True(t, hasCol("pending_spawns", "permission_overrides"), "pending_spawns.permission_overrides added")
	assert.True(t, hasCol("spawn_profiles", "is_owner"), "spawn_profiles.is_owner added")
	assert.True(t, hasCol("spawn_profiles", "permission_overrides"), "spawn_profiles.permission_overrides added")

	var isOwner int
	var perms string
	require.NoError(t, d.QueryRow(
		`SELECT is_owner, permission_overrides FROM pending_spawns WHERE label = 'lbl'`).Scan(&isOwner, &perms))
	assert.Equal(t, 0, isOwner, "existing row defaults to not-owner")
	assert.Equal(t, "", perms, "existing row defaults to no overrides")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 83, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op (the
	// pragma guard skips the duplicate ADD COLUMN).
	require.NoError(t, migrateV82toV83(d), "v82→v83 re-run is a clean no-op")
}

// TestSpawnProfileOwnerPermsRoundTrip exercises the SpawnProfile CRUD: a profile
// carrying the tri-state owner flag + permission overrides survives Create →
// Get and Update → Get, and the JSON-encoded override map is lossless.
func TestSpawnProfileOwnerPermsRoundTrip(t *testing.T) {
	setupTestDB(t)

	owner := true
	id, err := CreateSpawnProfile(&SpawnProfile{
		Name:                "owns",
		Role:                "lead",
		IsOwner:             &owner,
		PermissionOverrides: map[string]string{"groups.spawn": "grant"},
	})
	require.NoError(t, err)

	got, err := GetSpawnProfile("owns")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.IsOwner)
	assert.True(t, *got.IsOwner, "owner flag round-trips")
	assert.Equal(t, map[string]string{"groups.spawn": "grant"}, got.PermissionOverrides)

	// Update clearing the overrides + owner → reads back nil/empty.
	require.NoError(t, UpdateSpawnProfile(&SpawnProfile{ID: id, Name: "owns", Role: "lead"}))
	got2, err := GetSpawnProfile("owns")
	require.NoError(t, err)
	require.NotNil(t, got2)
	assert.Nil(t, got2.IsOwner, "owner cleared to unset")
	assert.Empty(t, got2.PermissionOverrides, "overrides cleared")
}

// TestPendingSpawnOwnerPermsRoundTrip exercises the Go CRUD: a pending spawn
// carrying an owner flag + permission overrides survives an Insert → Get round
// trip, and the JSON encode/decode of the override map is lossless.
func TestPendingSpawnOwnerPermsRoundTrip(t *testing.T) {
	setupTestDB(t)

	in := &PendingSpawn{
		Label:               "spwn-abc",
		GroupID:             1,
		Role:                "researcher",
		IsOwner:             true,
		PermissionOverrides: map[string]string{"groups.spawn": "grant", "self.rename": "deny"},
	}
	require.NoError(t, InsertPendingSpawn(in))

	got, err := GetPendingSpawn("spwn-abc")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.IsOwner, "owner flag round-trips")
	assert.Equal(t, map[string]string{"groups.spawn": "grant", "self.rename": "deny"},
		got.PermissionOverrides, "override map round-trips")

	// A spawn with no birth-time controls stores "" and reads back nil/false.
	plain := &PendingSpawn{Label: "spwn-plain", GroupID: 1, Role: "worker"}
	require.NoError(t, InsertPendingSpawn(plain))
	gp, err := GetPendingSpawn("spwn-plain")
	require.NoError(t, err)
	require.NotNil(t, gp)
	assert.False(t, gp.IsOwner, "no owner flag")
	assert.Empty(t, gp.PermissionOverrides, "no overrides")
}
