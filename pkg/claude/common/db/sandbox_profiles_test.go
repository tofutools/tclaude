package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSandboxProfileCRUDRoundTrip(t *testing.T) {
	setupTestDB(t)
	work := filepath.Join(os.Getenv("HOME"), "work")
	for _, name := range []string{"a", "z", "new"} {
		require.NoError(t, os.MkdirAll(filepath.Join(work, name), 0o755))
	}
	canonicalWork, err := filepath.EvalSymlinks(work)
	require.NoError(t, err)

	sparseID, err := CreateSandboxProfile(&SandboxProfile{Name: "empty"})
	require.NoError(t, err)
	populatedID, err := CreateSandboxProfile(&SandboxProfile{
		Name: "populated",
		Filesystem: []SandboxFilesystemGrant{
			{Path: filepath.Join(work, "z"), Access: "read"},
			{Path: filepath.Join(work, "a"), Access: "write"},
		},
		Environment: []SandboxEnvironmentEntry{
			{Name: "ZED", Value: "last"},
			{Name: "ALPHA", Value: "first"},
		},
	})
	require.NoError(t, err)

	sparse, err := GetSandboxProfileByID(sparseID)
	require.NoError(t, err)
	require.NotNil(t, sparse)
	assert.Empty(t, sparse.Filesystem)
	assert.NotNil(t, sparse.Filesystem, "empty payload round-trips as []")
	assert.Empty(t, sparse.Environment)
	assert.NotNil(t, sparse.Environment, "empty payload round-trips as []")

	got, err := GetSandboxProfile("populated")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, populatedID, got.ID)
	assert.Equal(t, []SandboxFilesystemGrant{
		{Path: filepath.Join(canonicalWork, "a"), Access: "write"},
		{Path: filepath.Join(canonicalWork, "z"), Access: "read"},
	}, got.Filesystem, "payload is stored in deterministic canonical order")
	assert.Equal(t, []SandboxEnvironmentEntry{
		{Name: "ALPHA", Value: "first"},
		{Name: "ZED", Value: "last"},
	}, got.Environment)
	assert.False(t, got.CreatedAt.IsZero())
	assert.False(t, got.UpdatedAt.IsZero())

	got.Name = "renamed"
	got.Filesystem = []SandboxFilesystemGrant{{Path: filepath.Join(work, "new"), Access: "read"}}
	got.Environment = []SandboxEnvironmentEntry{}
	require.NoError(t, UpdateSandboxProfile(got))
	updated, err := GetSandboxProfileByID(populatedID)
	require.NoError(t, err)
	assert.Equal(t, "renamed", updated.Name)
	assert.Equal(t, []SandboxFilesystemGrant{{Path: filepath.Join(canonicalWork, "new"), Access: "read"}}, updated.Filesystem)
	assert.Empty(t, updated.Environment)

	list, err := ListSandboxProfiles()
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "empty", list[0].Name)
	assert.Equal(t, "renamed", list[1].Name)

	n, err := DeleteSandboxProfile("renamed")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	missing, err := GetSandboxProfileByID(populatedID)
	require.NoError(t, err)
	assert.Nil(t, missing)
}

func TestSandboxProfileNameAndPayloadIntegrity(t *testing.T) {
	setupTestDB(t)
	same := filepath.Join(os.Getenv("HOME"), "same")
	require.NoError(t, os.MkdirAll(same, 0o755))
	canonicalSame, err := filepath.EvalSymlinks(same)
	require.NoError(t, err)

	firstID, err := CreateSandboxProfile(&SandboxProfile{Name: "first"})
	require.NoError(t, err)
	_, err = CreateSandboxProfile(&SandboxProfile{Name: "first"})
	require.ErrorIs(t, err, ErrSandboxProfileNameTaken)
	secondID, err := CreateSandboxProfile(&SandboxProfile{Name: "second"})
	require.NoError(t, err)
	require.ErrorIs(t, UpdateSandboxProfile(&SandboxProfile{ID: secondID, Name: "first"}), ErrSandboxProfileNameTaken)
	require.ErrorIs(t, UpdateSandboxProfile(&SandboxProfile{ID: 999999, Name: "ghost"}), sql.ErrNoRows)

	// Already-canonical duplicate paths fold with write dominating read; exact
	// environment duplicates fold, while conflicting values are rejected.
	require.NoError(t, UpdateSandboxProfile(&SandboxProfile{
		ID: firstID, Name: "first",
		Filesystem: []SandboxFilesystemGrant{
			{Path: same, Access: "read"},
			{Path: same, Access: "write"},
		},
		Environment: []SandboxEnvironmentEntry{
			{Name: "A", Value: "1"}, {Name: "A", Value: "1"},
		},
	}))
	got, err := GetSandboxProfile("first")
	require.NoError(t, err)
	assert.Equal(t, []SandboxFilesystemGrant{{Path: canonicalSame, Access: "write"}}, got.Filesystem)
	assert.Equal(t, []SandboxEnvironmentEntry{{Name: "A", Value: "1"}}, got.Environment)

	_, err = CreateSandboxProfile(&SandboxProfile{Name: "bad-access", Filesystem: []SandboxFilesystemGrant{{Path: "/x", Access: "deny"}}})
	require.ErrorContains(t, err, "is invalid")
	_, err = CreateSandboxProfile(&SandboxProfile{Name: "bad-env", Environment: []SandboxEnvironmentEntry{{Name: "A", Value: "1"}, {Name: "A", Value: "2"}}})
	require.ErrorContains(t, err, "conflicting values")
}

func TestSandboxProfileCorruptJSONFailsLoudly(t *testing.T) {
	setupTestDB(t)
	id, err := CreateSandboxProfile(&SandboxProfile{Name: "corrupt"})
	require.NoError(t, err)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `UPDATE sandbox_profiles SET filesystem_json = '{' WHERE id = ?`, id)
	_, err = GetSandboxProfileByID(id)
	require.ErrorContains(t, err, `decode sandbox profile "corrupt" filesystem`)
}

func TestSandboxProfileAssignmentsSurviveRenameAndClearOnDelete(t *testing.T) {
	setupTestDB(t)
	profileID, err := CreateSandboxProfile(&SandboxProfile{Name: "original"})
	require.NoError(t, err)
	_, err = CreateAgentGroup("crew", "")
	require.NoError(t, err)
	require.NoError(t, SetGlobalSandboxProfile("original"))
	n, err := SetAgentGroupSandboxProfile("crew", "original")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	require.NoError(t, UpdateSandboxProfile(&SandboxProfile{ID: profileID, Name: "renamed"}))
	global, err := GetGlobalSandboxProfile()
	require.NoError(t, err)
	require.NotNil(t, global)
	assert.Equal(t, profileID, global.ID)
	assert.Equal(t, "renamed", global.Name)
	group, err := GetAgentGroupSandboxProfile("crew")
	require.NoError(t, err)
	require.NotNil(t, group)
	assert.Equal(t, profileID, group.ID)
	assert.Equal(t, "renamed", group.Name)

	d, err := Open()
	require.NoError(t, err)
	var globalName, groupName string
	var globalID, groupID int64
	require.NoError(t, d.QueryRow(`SELECT profile_name, profile_id FROM sandbox_profile_global_assignment WHERE id = 1`).Scan(&globalName, &globalID))
	require.NoError(t, d.QueryRow(`SELECT sandbox_profile, sandbox_profile_id FROM agent_groups WHERE name = 'crew'`).Scan(&groupName, &groupID))
	assert.Equal(t, "renamed", globalName)
	assert.Equal(t, profileID, globalID)
	assert.Equal(t, "renamed", groupName)
	assert.Equal(t, profileID, groupID)
	_, err = d.Exec(`UPDATE agent_groups SET sandbox_profile_id = 999999 WHERE name = 'crew'`)
	require.ErrorContains(t, err, "sandbox profile reference does not exist",
		"the trigger-backed FK equivalent rejects dangling references")
	sourceGroup, err := GetAgentGroupByName("crew")
	require.NoError(t, err)
	_, err = CreateAgentGroupFrom("crew-clone", *sourceGroup)
	require.NoError(t, err)
	cloneProfile, err := GetAgentGroupSandboxProfile("crew-clone")
	require.NoError(t, err)
	require.NotNil(t, cloneProfile)
	assert.Equal(t, profileID, cloneProfile.ID, "group clone preserves the stable sandbox-profile assignment")

	_, err = DeleteSandboxProfile("renamed")
	require.NoError(t, err)
	global, err = GetGlobalSandboxProfile()
	require.NoError(t, err)
	assert.Nil(t, global)
	group, err = GetAgentGroupSandboxProfile("crew")
	require.NoError(t, err)
	assert.Nil(t, group)

	// Reusing the display name cannot capture either cleared stable reference.
	_, err = CreateSandboxProfile(&SandboxProfile{Name: "renamed"})
	require.NoError(t, err)
	_, err = CreateAgentGroupFrom("stale-clone", *sourceGroup)
	require.NoError(t, err)
	staleCloneProfile, err := GetAgentGroupSandboxProfile("stale-clone")
	require.NoError(t, err)
	assert.Nil(t, staleCloneProfile, "a stale source ID cannot capture a new profile that reused the old name")
	global, err = GetGlobalSandboxProfile()
	require.NoError(t, err)
	assert.Nil(t, global)
	group, err = GetAgentGroupSandboxProfile("crew")
	require.NoError(t, err)
	assert.Nil(t, group)

	require.ErrorIs(t, SetGlobalSandboxProfile("missing"), ErrSandboxProfileNotFound)
	_, err = SetAgentGroupSandboxProfile("crew", "missing")
	require.ErrorIs(t, err, ErrSandboxProfileNotFound)
}
