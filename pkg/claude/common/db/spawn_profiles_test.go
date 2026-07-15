package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func boolp(b bool) *bool { return &b }

// TestSpawnProfile_CRUDRoundTrip exercises create → get → list → update →
// delete, with particular attention to the tri-state toggles: a profile that
// sets only some fields must round-trip the unset ones as nil (not false), so
// the load-into-dialog layer can leave those dialog fields at their own
// default.
func TestSpawnProfile_CRUDRoundTrip(t *testing.T) {
	setupTestDB(t)

	// A mix: some launch fields set, AutoReview explicitly false, TrustDir
	// explicitly true, the dialog toggles left unset (nil).
	id, err := CreateSpawnProfile(&SpawnProfile{
		Name:           "codex-sandboxed",
		DisabledReason: "capacity temporarily exhausted",
		Harness:        "codex",
		Model:          "gpt-5",
		Effort:         "high",
		Sandbox:        "workspace-write",
		Approval:       "never",
		AutoReview:     boolp(false),
		TrustDir:       boolp(true),
		AgentName:      "worker",
		Role:           "dev",
		Descr:          "a sandboxed codex worker",
		InitialMessage: "hello",
		// SyncWorktree / AutoFocus / IncludeGroupDefaultContext left nil.
	})
	require.NoError(t, err)
	require.NotZero(t, id)

	got, err := GetSpawnProfile("codex-sandboxed")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "capacity temporarily exhausted", got.DisabledReason)
	assert.Equal(t, "codex", got.Harness)
	assert.Equal(t, "gpt-5", got.Model)
	assert.Equal(t, "high", got.Effort)
	assert.Equal(t, "workspace-write", got.Sandbox)
	assert.Equal(t, "never", got.Approval)
	assert.Equal(t, "worker", got.AgentName)
	assert.Equal(t, "dev", got.Role)
	assert.Equal(t, "a sandboxed codex worker", got.Descr)
	assert.Equal(t, "hello", got.InitialMessage)

	// Tri-state: explicit false / true round-trip distinctly from unset (nil).
	require.NotNil(t, got.AutoReview)
	assert.False(t, *got.AutoReview, "explicit false survives, not coerced to nil")
	require.NotNil(t, got.TrustDir)
	assert.True(t, *got.TrustDir)
	assert.Nil(t, got.SyncWorktree, "unset toggle round-trips as nil")
	assert.Nil(t, got.AutoFocus)
	assert.Nil(t, got.IncludeGroupDefaultContext)
}

// TestSpawnProfile_NameTaken pins the UNIQUE-name guard on both create and
// rename-on-update.
func TestSpawnProfile_NameTaken(t *testing.T) {
	setupTestDB(t)

	_, err := CreateSpawnProfile(&SpawnProfile{Name: "alpha"})
	require.NoError(t, err)
	_, err = CreateSpawnProfile(&SpawnProfile{Name: "alpha"})
	require.ErrorIs(t, err, ErrSpawnProfileNameTaken, "duplicate create is rejected")

	betaID, err := CreateSpawnProfile(&SpawnProfile{Name: "beta"})
	require.NoError(t, err)
	err = UpdateSpawnProfile(&SpawnProfile{ID: betaID, Name: "alpha"})
	require.ErrorIs(t, err, ErrSpawnProfileNameTaken, "renaming onto a taken name is rejected")
}

// TestSpawnProfile_GetMissingIsNilNil documents the not-found contract: a
// missing profile is (nil, nil), not an error — callers answer 404 on nil.
func TestSpawnProfile_GetMissingIsNilNil(t *testing.T) {
	setupTestDB(t)
	got, err := GetSpawnProfile("nope")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestSpawnProfile_Update mutates fields (including flipping a toggle from
// unset → set and a set → unset) and confirms Get reflects the new state; a
// missing ID surfaces sql.ErrNoRows.
func TestSpawnProfile_Update(t *testing.T) {
	setupTestDB(t)

	id, err := CreateSpawnProfile(&SpawnProfile{
		Name:      "p",
		Model:     "opus",
		AutoFocus: boolp(true),
		// SyncWorktree left nil.
	})
	require.NoError(t, err)

	err = UpdateSpawnProfile(&SpawnProfile{
		ID:             id,
		Name:           "p",
		DisabledReason: "paused for maintenance",
		Model:          "sonnet",    // changed
		SyncWorktree:   boolp(true), // nil -> true
		AutoFocus:      nil,         // true -> unset
	})
	require.NoError(t, err)

	got, err := GetSpawnProfile("p")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "sonnet", got.Model)
	assert.Equal(t, "paused for maintenance", got.DisabledReason)
	require.NotNil(t, got.SyncWorktree)
	assert.True(t, *got.SyncWorktree)
	assert.Nil(t, got.AutoFocus, "a toggle set back to nil clears to unset")

	err = UpdateSpawnProfile(&SpawnProfile{ID: 999999, Name: "ghost"})
	require.ErrorIs(t, err, sql.ErrNoRows, "updating a missing id is sql.ErrNoRows")
}

// TestSpawnProfile_ListAndDelete pins ordering (by name) and the delete
// rows-affected contract (0 = no such profile).
func TestSpawnProfile_ListAndDelete(t *testing.T) {
	setupTestDB(t)

	for _, n := range []string{"gamma", "alpha", "beta"} {
		_, err := CreateSpawnProfile(&SpawnProfile{Name: n})
		require.NoError(t, err)
	}

	list, err := ListSpawnProfiles()
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, "alpha", list[0].Name, "ordered by name")
	assert.Equal(t, "beta", list[1].Name)
	assert.Equal(t, "gamma", list[2].Name)

	n, err := DeleteSpawnProfile("beta")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "delete reports one row removed")

	got, err := GetSpawnProfile("beta")
	require.NoError(t, err)
	assert.Nil(t, got, "deleted profile is gone")

	n, err = DeleteSpawnProfile("beta")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "deleting a missing profile reports zero rows")
}

func TestSpawnProfile_AliasesResolveAndShareNamespace(t *testing.T) {
	setupTestDB(t)

	id, err := CreateSpawnProfile(&SpawnProfile{
		Name:    "gpt5.6-sol-high",
		Aliases: []string{"codex-reviewer", "cold-reviewer"},
	})
	require.NoError(t, err)

	got, err := ResolveSpawnProfile("codex-reviewer")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "gpt5.6-sol-high", got.Name)
	assert.Equal(t, []string{"codex-reviewer", "cold-reviewer"}, got.Aliases)

	exact, err := GetSpawnProfile("codex-reviewer")
	require.NoError(t, err)
	assert.Nil(t, exact, "management lookup distinguishes aliases from primary names")

	_, err = CreateSpawnProfile(&SpawnProfile{Name: "codex-reviewer"})
	require.ErrorIs(t, err, ErrSpawnProfileNameTaken)
	_, err = CreateSpawnProfile(&SpawnProfile{Name: "other", Aliases: []string{"gpt5.6-sol-high"}})
	require.ErrorIs(t, err, ErrSpawnProfileNameTaken)
}

func TestSpawnProfile_UpdateAndDeleteAliases(t *testing.T) {
	setupTestDB(t)
	id, err := CreateSpawnProfile(&SpawnProfile{Name: "primary", Aliases: []string{"old-alias"}})
	require.NoError(t, err)

	require.NoError(t, UpdateSpawnProfile(&SpawnProfile{ID: id, Name: "renamed", Aliases: []string{"new-alias"}}))
	old, err := ResolveSpawnProfile("old-alias")
	require.NoError(t, err)
	assert.Nil(t, old)
	updated, err := ResolveSpawnProfile("new-alias")
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, "renamed", updated.Name)

	n, err := DeleteSpawnProfile("renamed")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	deleted, err := ResolveSpawnProfile("new-alias")
	require.NoError(t, err)
	assert.Nil(t, deleted)
}

func TestSpawnProfile_UpdateCanPromoteOwnAlias(t *testing.T) {
	setupTestDB(t)
	id, err := CreateSpawnProfile(&SpawnProfile{Name: "primary", Aliases: []string{"reviewer"}})
	require.NoError(t, err)

	require.NoError(t, UpdateSpawnProfile(&SpawnProfile{
		ID: id, Name: "reviewer", Aliases: []string{"primary"},
	}))
	got, err := ResolveSpawnProfile("primary")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "reviewer", got.Name)
}
