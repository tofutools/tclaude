package agentd_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestSandboxProfileSpawnFreezesValuesAndExplicitSelectionIsHumanOnly(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	profileID, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "literal-env",
		Environment: []db.SandboxEnvironmentEntry{{
			Name: "LITERAL", Value: "spaces '$HOME' $(touch nope); `echo nope`\nnext",
		}},
	})
	require.NoError(t, err)

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "sandbox_profile": "literal-env",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	snapshot, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	require.Len(t, snapshot.Applied, 1)
	assert.Equal(t, profileID, snapshot.Applied[0].ID)
	require.Len(t, snapshot.Effective.Environment, 1)
	assert.Equal(t, "LITERAL", snapshot.Effective.Environment[0].Name)
	assert.Contains(t, snapshot.Effective.Environment[0].Value, "$(touch nope)")

	persisted, err := db.AgentEffectiveSandboxConfigForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	assert.Equal(t, snapshot.Effective.Environment, persisted.Effective.Environment)

	require.NoError(t, db.GrantAgentPermission(spawn.ConvID, agentd.PermGroupsSpawn, "test"))
	require.NoError(t, db.GrantAgentPermission(spawn.ConvID, agentd.PermSandboxProfilesManage, "test"))
	denied := f.AsAgent(spawn.ConvID).SpawnWith("crew", map[string]any{
		"name": "child", "sandbox_profile": "literal-env",
	})
	require.Equal(t, http.StatusForbidden, denied.Code)
	assert.Contains(t, string(denied.Raw), "sandbox_profile_restricted")

	profile, err := db.GetSandboxProfile("literal-env")
	require.NoError(t, err)
	profile.Environment = []db.SandboxEnvironmentEntry{{Name: "LITERAL", Value: "mutated-after-launch"}}
	require.NoError(t, db.UpdateSandboxProfile(profile))
	f.MarkOffline(spawn.TmuxSession)
	resume := f.AsHuman().Resume(spawn.ConvID)
	f.AssertResumeSpawned(resume)
	resumedSnapshot, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	require.NotNil(t, resumedSnapshot)
	assert.Contains(t, resumedSnapshot.Effective.Environment[0].Value, "$(touch nope)")
}

func TestSandboxProfileSpawnRejectsAmbientCapabilityWideningAfterParentLaunch(t *testing.T) {
	for _, scope := range []string{"global", "group"} {
		t.Run(scope, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("crew")
			parent := f.AsHuman().SpawnWith("crew", map[string]any{"name": "parent"})
			require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
			require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsSpawn, "test"))

			writeRoot := t.TempDir()
			_, err := db.CreateSandboxProfile(&db.SandboxProfile{
				Name: "widened-" + scope,
				Filesystem: []db.SandboxFilesystemGrant{{
					Path: writeRoot, Access: "write",
				}},
			})
			require.NoError(t, err)
			if scope == "global" {
				require.NoError(t, db.SetGlobalSandboxProfile("widened-global"))
			} else {
				_, err = db.SetAgentGroupSandboxProfile("crew", "widened-group")
				require.NoError(t, err)
			}

			denied := f.AsAgent(parent.ConvID).SpawnWith("crew", map[string]any{
				"name": "child", "cwd": t.TempDir(),
			})
			require.Equal(t, http.StatusForbidden, denied.Code)
			assert.Contains(t, string(denied.Raw), "sandbox_profile_restricted")
			assert.Contains(t, string(denied.Raw), writeRoot)
		})
	}
}

func TestSandboxProfileWriteRootParticipatesInAgentSpawnProof(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	writeRoot, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "shared-write",
		Filesystem: []db.SandboxFilesystemGrant{{
			Path: writeRoot, Access: "write",
		}},
	})
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile("crew", "shared-write")
	require.NoError(t, err)

	parent := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "parent", "cwd": t.TempDir(),
	})
	require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsSpawn, "test"))

	childCwd, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	rec := agentReq(t, f, parent.ConvID, http.MethodPost,
		"/v1/groups/"+url.PathEscape("crew")+"/spawn",
		map[string]any{"name": "child", "cwd": childCwd})
	challenge := decodeWriteProofChallenge(t, rec)
	assert.ElementsMatch(t, []string{childCwd, writeRoot}, challenge.WriteProof.Dirs)

	// The challenge is observational in this test; ensure no marker was
	// accidentally materialised by the daemon itself.
	_, statErr := os.Lstat(filepath.Join(writeRoot, challenge.WriteProof.Filename))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}
