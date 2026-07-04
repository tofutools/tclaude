package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV58toV59_AddsPendingSpawns seeds a bare v58 DB, runs the v59
// migration, and asserts the pending_spawns table lands and accepts a row
// with the full enrollment intent.
func TestMigrateV58toV59_AddsPendingSpawns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v58.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (58);
	`)
	require.NoError(t, err, "seed v58 schema")

	require.NoError(t, migrateV58toV59(d), "migrateV58toV59")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 59, ver, "schema_version after migration")

	// The table exists and accepts a fully-populated pending row.
	_, err = d.Exec(`
		INSERT INTO pending_spawns
			(label, group_id, role, descr, name, initial_message, group_context,
			 reply_to_conv, spawned_by_conv, worktree_path, worktree_branch, created_at)
		VALUES ('spwn-abc', 7, 'worker', 'a worker', 'Alice', 'do the thing',
			'group ctx', 'conv-po', 'conv-po', '/tmp/wt', 'feat/x', '2026-06-14T00:00:00Z')`)
	require.NoError(t, err, "pending_spawns accepts a full row")

	// Columns that default — an INSERT naming only the required pair lands
	// with empty strings everywhere else (the prepopulate-then-backfill
	// shape never needs that, but the DEFAULTs keep the table forgiving).
	_, err = d.Exec(`INSERT INTO pending_spawns (label, group_id, created_at) VALUES ('spwn-min', 1, '2026-06-14T00:00:00Z')`)
	require.NoError(t, err, "pending_spawns row with only the NOT NULL columns")

	var role, descr string
	require.NoError(t, d.QueryRow(`SELECT role, descr FROM pending_spawns WHERE label = 'spwn-min'`).Scan(&role, &descr))
	assert.Equal(t, "", role, "role defaults to empty")
	assert.Equal(t, "", descr, "descr defaults to empty")
}

// TestMigrateV58toV59_HealsHalfAppliedRun guards the converge-on-re-run
// property: a half-applied earlier attempt that created the table but never
// bumped schema_version must not wedge — CREATE TABLE IF NOT EXISTS skips
// the existing table, preserves its data, and the version finally lands on
// 59. A second re-run is a clean no-op.
func TestMigrateV58toV59_HealsHalfAppliedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v58-half.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// Half-applied: pending_spawns already exists (with a row, to prove the
	// re-run preserves it), version still 58.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (58);
		CREATE TABLE pending_spawns (
			label           TEXT PRIMARY KEY,
			group_id        INTEGER NOT NULL,
			role            TEXT NOT NULL DEFAULT '',
			descr           TEXT NOT NULL DEFAULT '',
			name            TEXT NOT NULL DEFAULT '',
			initial_message TEXT NOT NULL DEFAULT '',
			group_context   TEXT NOT NULL DEFAULT '',
			reply_to_conv   TEXT NOT NULL DEFAULT '',
			spawned_by_conv TEXT NOT NULL DEFAULT '',
			worktree_path   TEXT NOT NULL DEFAULT '',
			worktree_branch TEXT NOT NULL DEFAULT '',
			created_at      TEXT NOT NULL
		);
		INSERT INTO pending_spawns (label, group_id, name, created_at)
			VALUES ('spwn-existing', 3, 'Bob', '2026-06-14T00:00:00Z');
	`)
	require.NoError(t, err, "seed half-applied v58 schema")

	require.NoError(t, migrateV58toV59(d), "re-run must converge, not fail on existing table")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 59, ver, "schema_version finally lands on 59")

	var name string
	require.NoError(t, d.QueryRow(`SELECT name FROM pending_spawns WHERE label = 'spwn-existing'`).Scan(&name))
	assert.Equal(t, "Bob", name, "existing pending_spawns data survives the healing run")

	require.NoError(t, migrateV58toV59(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 59, ver, "second re-run stays at 59")
}

// TestMigrateV58toV59_FreshSchemaRoundTrips builds a fresh DB through the
// full migrate() chain and round-trips a pending spawn through the
// production Insert/Get/List/Delete helpers. (The literal currentVersion
// pin moved forward to migrate_v60_test.go, per convention.)
func TestMigrateV58toV59_FreshSchemaRoundTrips(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

	ps := &PendingSpawn{
		Label:          "spwn-roundtrip",
		GroupID:        42,
		Role:           "reviewer",
		Descr:          "reviews diffs",
		Name:           "Rev",
		InitialMessage: "review the PR",
		GroupContext:   "be terse",
		ReplyToConv:    "conv-lead",
		SpawnedByConv:  "conv-lead",
		WorktreePath:   "/tmp/wt",
		WorktreeBranch: "feat/y",
	}
	require.NoError(t, InsertPendingSpawn(ps), "InsertPendingSpawn")

	got, err := GetPendingSpawn("spwn-roundtrip")
	require.NoError(t, err, "GetPendingSpawn")
	require.NotNil(t, got)
	assert.Equal(t, ps.Label, got.Label)
	assert.Equal(t, ps.GroupID, got.GroupID)
	assert.Equal(t, ps.Role, got.Role)
	assert.Equal(t, ps.Descr, got.Descr)
	assert.Equal(t, ps.Name, got.Name)
	assert.Equal(t, ps.InitialMessage, got.InitialMessage)
	assert.Equal(t, ps.GroupContext, got.GroupContext)
	assert.Equal(t, ps.ReplyToConv, got.ReplyToConv)
	assert.Equal(t, ps.SpawnedByConv, got.SpawnedByConv)
	assert.Equal(t, ps.WorktreePath, got.WorktreePath)
	assert.Equal(t, ps.WorktreeBranch, got.WorktreeBranch)
	assert.NotEmpty(t, got.CreatedAt, "InsertPendingSpawn stamps created_at")

	list, err := ListPendingSpawns()
	require.NoError(t, err, "ListPendingSpawns")
	require.Len(t, list, 1)
	assert.Equal(t, "spwn-roundtrip", list[0].Label)

	require.NoError(t, DeletePendingSpawn("spwn-roundtrip"), "DeletePendingSpawn")
	gone, err := GetPendingSpawn("spwn-roundtrip")
	require.NoError(t, err, "GetPendingSpawn after delete returns (nil, nil)")
	assert.Nil(t, gone, "deleted pending spawn is gone")

	// Deleting a missing row is idempotent.
	require.NoError(t, DeletePendingSpawn("spwn-roundtrip"), "DeletePendingSpawn is idempotent")
}

func TestPendingSpawnClaim(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, InsertPendingSpawn(&PendingSpawn{Label: "spwn-claim", GroupID: 42}))

	claimed, err := ClaimPendingSpawn("spwn-claim")
	require.NoError(t, err)
	assert.True(t, claimed, "first claim wins")

	gone, err := GetPendingSpawn("spwn-claim")
	require.NoError(t, err)
	assert.Nil(t, gone, "claim removes the pending row")

	claimed, err = ClaimPendingSpawn("spwn-claim")
	require.NoError(t, err)
	assert.False(t, claimed, "second claim loses cleanly")
}
