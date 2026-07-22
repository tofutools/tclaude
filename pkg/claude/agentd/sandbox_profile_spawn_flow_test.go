package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestSandboxProfileSpawnRefreshesExplicitValuesOnResumeAndSelectionIsHumanOnly(t *testing.T) {
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
		"name": "worker", "sandbox_profile": "literal-env", "approval": "bypassPermissions",
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
	var wire struct {
		Resolved *agent.ResolvedLaunch `json:"resolved"`
	}
	require.NoError(t, json.Unmarshal(spawn.Raw, &wire))
	require.NotNil(t, wire.Resolved)
	require.NotNil(t, wire.Resolved.SandboxPolicy)
	assert.Equal(t, []string{"LITERAL"}, wire.Resolved.SandboxPolicy.Environment)
	assert.Equal(t, profileID, wire.Resolved.SandboxPolicy.Applied[0].ID)
	assert.NotContains(t, string(spawn.Raw), "$(touch nope)", "resolved response must not expose environment values")

	persisted, err := db.AgentEffectiveSandboxConfigForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	assert.Equal(t, snapshot.Effective.Environment, persisted.Effective.Environment)

	require.NoError(t, db.GrantAgentPermission(spawn.ConvID, agentd.PermGroupsSpawn, "test"))
	require.NoError(t, db.GrantAgentPermission(spawn.ConvID, agentd.PermSandboxProfilesManage, "test"))
	denied := f.AsAgent(spawn.ConvID).SpawnWith("crew", map[string]any{
		"name": "child", "sandbox_profile": "literal-env", "approval": "default",
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
	assert.Equal(t, "mutated-after-launch", resumedSnapshot.Effective.Environment[0].Value)
}

// Restart re-resolves the currently selected profile rather than replaying the
// launch snapshot — with one exception: a deny the agent launched under is
// re-imposed, because dropping it would widen a running agent (see
// clampResumeDenyLineage).
func TestSandboxProfileRestartUsesCurrentRulesAndProvenance(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	launchDeny, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	editedDeny, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	profileID, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name:       "current-restrictions",
		Filesystem: []db.SandboxFilesystemGrant{{Path: launchDeny, Access: sandboxpolicy.AccessDeny}},
	})
	require.NoError(t, err)
	parent := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "parent", "harness": "claude", "sandbox": harness.ClaudeSandboxOn,
		"sandbox_profile": "current-restrictions",
	})
	require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
	before, ok := f.World.SpawnSandboxPolicy(parent.ConvID)
	require.True(t, ok)
	require.NotNil(t, before)
	assert.Equal(t, []sandboxpolicy.FilesystemGrant{{Path: launchDeny, Access: sandboxpolicy.AccessDeny}}, before.Effective.Filesystem)

	profile, err := db.GetSandboxProfileByID(profileID)
	require.NoError(t, err)
	profile.Filesystem = []db.SandboxFilesystemGrant{{Path: editedDeny, Access: sandboxpolicy.AccessDeny}}
	require.NoError(t, db.UpdateSandboxProfile(profile))

	f.MarkOffline(parent.TmuxSession)
	resume := f.AsHuman().Resume(parent.ConvID)
	f.AssertResumeSpawned(resume)
	after, ok := f.World.SpawnSandboxPolicy(parent.ConvID)
	require.True(t, ok)
	require.NotNil(t, after)
	paths := map[string]sandboxpolicy.Access{}
	for _, grant := range after.Effective.Filesystem {
		paths[grant.Path] = grant.Access
	}
	assert.Equal(t, sandboxpolicy.AccessDeny, paths[editedDeny], "restart picks up the newly authored restriction")
	assert.Equal(t, sandboxpolicy.AccessDeny, paths[launchDeny], "and keeps the deny the agent launched under")
	assert.Equal(t, map[string][]sandboxpolicy.ProfileSource{
		editedDeny: {{Scope: sandboxpolicy.ScopeExplicit, Profile: "current-restrictions"}},
	}, after.Effective.Provenance.Filesystem,
		"the restored deny has no current-registry source and must not claim one")

	persisted, err := db.AgentEffectiveSandboxConfigForConv(parent.ConvID)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	assert.Equal(t, after.Effective.Filesystem, persisted.Effective.Filesystem)
	assert.Equal(t, after.Effective.Provenance.Filesystem, persisted.Effective.Provenance.Filesystem)
}

func TestSandboxProfileResumeRefreshesComposedPolicyAndCanSpawnChild(t *testing.T) {
	for _, harnessCase := range []struct {
		name string
		body map[string]any
	}{
		{name: "codex-managed", body: map[string]any{"harness": "codex", "sandbox": "tclaude-agent"}},
		{name: "claude", body: map[string]any{"harness": "claude"}},
	} {
		for _, changedScope := range []string{"global", "group"} {
			t.Run(harnessCase.name+"/"+changedScope, func(t *testing.T) {
				f := newFlow(t)
				f.HaveGroup("crew")
				_, err := db.CreateSandboxProfile(&db.SandboxProfile{
					Name: "global-policy", Environment: []db.SandboxEnvironmentEntry{{Name: "GLOBAL_VALUE", Value: "v1"}},
				})
				require.NoError(t, err)
				_, err = db.CreateSandboxProfile(&db.SandboxProfile{
					Name: "group-policy", Environment: []db.SandboxEnvironmentEntry{{Name: "GROUP_VALUE", Value: "v1"}},
				})
				require.NoError(t, err)
				require.NoError(t, db.SetGlobalSandboxProfile("global-policy"))
				_, err = db.SetAgentGroupSandboxProfile("crew", "group-policy")
				require.NoError(t, err)

				parentBody := map[string]any{"name": "parent", "cwd": t.TempDir()}
				for key, value := range harnessCase.body {
					parentBody[key] = value
				}
				if harnessCase.name == "claude" {
					parentBody["approval"] = "bypassPermissions"
				}
				parent := f.AsHuman().SpawnWith("crew", parentBody)
				require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
				require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsSpawn, "test"))

				writeRoot, err := filepath.EvalSymlinks(t.TempDir())
				require.NoError(t, err)
				envName := "GLOBAL_VALUE"
				if changedScope == "group" {
					envName = "GROUP_VALUE"
				}
				replacement := changedScope + "-policy-v2"
				_, err = db.CreateSandboxProfile(&db.SandboxProfile{
					Name:        replacement,
					Environment: []db.SandboxEnvironmentEntry{{Name: envName, Value: "v2"}},
					Filesystem:  []db.SandboxFilesystemGrant{{Path: writeRoot, Access: "write"}},
				})
				require.NoError(t, err)
				if changedScope == "global" {
					require.NoError(t, db.SetGlobalSandboxProfile(replacement))
				} else {
					_, err = db.SetAgentGroupSandboxProfile("crew", replacement)
					require.NoError(t, err)
				}

				f.MarkOffline(parent.TmuxSession)
				resume := f.AsHuman().Resume(parent.ConvID)
				f.AssertResumeSpawned(resume)
				resumed, ok := f.World.SpawnSandboxPolicy(parent.ConvID)
				require.True(t, ok)
				require.NotNil(t, resumed)
				assert.Contains(t, resumed.Effective.Filesystem, db.SandboxFilesystemGrant{Path: writeRoot, Access: "write"})
				assert.Contains(t, resumed.Effective.Environment, db.SandboxEnvironmentEntry{Name: envName, Value: "v2"})
				require.Len(t, resumed.Applied, 2, "global and group policies remain composed")
				persisted, err := db.AgentEffectiveSandboxConfigForConv(parent.ConvID)
				require.NoError(t, err)
				require.NotNil(t, persisted)
				assert.Equal(t, resumed.Effective, persisted.Effective)

				childBody := map[string]any{"name": "child", "cwd": t.TempDir()}
				for key, value := range harnessCase.body {
					childBody[key] = value
				}
				if harnessCase.name == "claude" {
					childBody["approval"] = "default"
				}
				child := f.AsAgent(parent.ConvID).SpawnWith("crew", childBody)
				require.Equalf(t, http.StatusOK, child.Code, "child spawn body=%s", child.Raw)
				childSnapshot, ok := f.World.SpawnSandboxPolicy(child.ConvID)
				require.True(t, ok)
				require.NotNil(t, childSnapshot)
				assert.Contains(t, childSnapshot.Effective.Filesystem, db.SandboxFilesystemGrant{Path: writeRoot, Access: "write"})
			})
		}
	}
}

func TestSandboxProfileResumeFailsBeforeLaunchWhenExplicitProfileDisappears(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "explicit-policy"})
	require.NoError(t, err)
	parent := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "parent", "sandbox_profile": "explicit-policy",
	})
	require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
	_, err = db.DeleteSandboxProfile("explicit-policy")
	require.NoError(t, err)
	f.MarkOffline(parent.TmuxSession)

	resume := f.AsHuman().Resume(parent.ConvID)
	assert.Equal(t, "error", resume.Action)
	assert.Contains(t, resume.Detail, "sandbox_profile_changed")
	assert.Contains(t, resume.Detail, "recreate it under that name")

	// The recovery action is real: recreating the named profile gives it a new
	// stable ID, and the next controlled resume resolves and launches it.
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{Name: "explicit-policy"})
	require.NoError(t, err)
	resume = f.AsHuman().Resume(parent.ConvID)
	f.AssertResumeSpawned(resume)
}

func TestSandboxProfileSpawnAcceptsMissingFilesystemRule(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	canonicalParent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	missing := filepath.Join(canonicalParent, "future", "cache")
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "future-cache",
		Filesystem: []db.SandboxFilesystemGrant{{
			Path: missing, Access: "write",
		}},
	})
	require.NoError(t, err)

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "sandbox_profile": "future-cache",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	snapshot, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assert.Equal(t, []db.SandboxFilesystemGrant{{Path: missing, Access: "write"}}, snapshot.Effective.Filesystem)
	_, err = os.Stat(missing)
	require.ErrorIs(t, err, os.ErrNotExist, "spawn must not create a missing profile path")
}

func TestSandboxProfileSpawnMaterializesUniqueAgentDirectories(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "go-caches", AgentDirectories: []string{"GOCACHE", "GOLANGCI_LINT_CACHE"},
	})
	require.NoError(t, err)

	pathsBySpawn := make([]map[string]string, 0, 2)
	for _, name := range []string{"worker-one", "worker-two"} {
		spawn := f.AsHuman().SpawnWith("crew", map[string]any{
			"name": name, "sandbox_profile": "go-caches", "harness": "codex", "sandbox": "tclaude-agent",
		})
		require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
		snapshot, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
		require.True(t, ok)
		require.NotNil(t, snapshot)
		assert.Equal(t, []string{"GOCACHE", "GOLANGCI_LINT_CACHE"}, snapshot.Effective.AgentDirectories)
		require.Len(t, snapshot.Effective.Environment, 2)
		require.Len(t, snapshot.Effective.Filesystem, 1)
		paths := map[string]string{}
		for _, entry := range snapshot.Effective.Environment {
			paths[entry.Name] = entry.Value
			info, statErr := os.Stat(entry.Value)
			require.NoError(t, statErr)
			assert.True(t, info.IsDir())
		}
		grant := snapshot.Effective.Filesystem[0]
		assert.Equal(t, "write", string(grant.Access))
		assert.Equal(t, filepath.Dir(paths["GOCACHE"]), grant.Path)
		assert.Equal(t, grant.Path, filepath.Dir(paths["GOLANGCI_LINT_CACHE"]))
		pathsBySpawn = append(pathsBySpawn, paths)
	}
	assert.NotEqual(t, pathsBySpawn[0]["GOCACHE"], pathsBySpawn[1]["GOCACHE"])
	assert.NotEqual(t, filepath.Dir(pathsBySpawn[0]["GOCACHE"]), filepath.Dir(pathsBySpawn[1]["GOCACHE"]))
}

func TestSandboxProfileAgentCanSpawnChildWithAdditionalAgentDirectories(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "parent-cache", AgentDirectories: []string{"GOCACHE"},
	})
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile("crew", "parent-cache")
	require.NoError(t, err)

	parent := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "parent", "harness": "codex", "sandbox": "tclaude-agent",
	})
	require.Equalf(t, http.StatusOK, parent.Code, "parent spawn body=%s", parent.Raw)
	// The simulator's first status write does not carry the launch sandbox;
	// restore the production launch metadata so the nested-spawn lineage gate
	// exercises the managed Codex path before sandbox-profile containment.
	parentSession, err := db.FindSessionByConvID(parent.ConvID)
	require.NoError(t, err)
	parentSession.SandboxMode = "tclaude-agent"
	require.NoError(t, db.SaveSession(parentSession))
	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsSpawn, "test"))
	parentSnapshot, ok := f.World.SpawnSandboxPolicy(parent.ConvID)
	require.True(t, ok)
	require.NotNil(t, parentSnapshot)
	require.Equal(t, []string{"GOCACHE"}, parentSnapshot.Effective.AgentDirectories)

	_, err = db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "child-caches", AgentDirectories: []string{"GOCACHE", "GOTMPDIR"},
	})
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile("crew", "child-caches")
	require.NoError(t, err)

	child := f.AsAgent(parent.ConvID).SpawnWith("crew", map[string]any{
		"name": "child", "harness": "codex", "sandbox": "tclaude-agent",
	})
	require.Equalf(t, http.StatusOK, child.Code, "child spawn body=%s", child.Raw)
	childSnapshot, ok := f.World.SpawnSandboxPolicy(child.ConvID)
	require.True(t, ok)
	require.NotNil(t, childSnapshot)
	assert.Equal(t, []string{"GOCACHE", "GOTMPDIR"}, childSnapshot.Effective.AgentDirectories)
	require.Len(t, childSnapshot.Effective.Filesystem, 1)
	require.Len(t, childSnapshot.Effective.Environment, 2)

	parentGOCACHE := parentSnapshot.Effective.Environment[0].Value
	childRoot := childSnapshot.Effective.Filesystem[0]
	assert.Equal(t, "write", string(childRoot.Access))
	for _, entry := range childSnapshot.Effective.Environment {
		assert.NotEqual(t, parentGOCACHE, entry.Value, "%s must be private to the child", entry.Name)
		assert.Contains(t, entry.Value, string(filepath.Separator)+"agent-dirs"+string(filepath.Separator))
		assert.Equal(t, childRoot.Path, filepath.Dir(entry.Value), "%s must share the child's writable root", entry.Name)
		info, statErr := os.Stat(entry.Value)
		require.NoError(t, statErr)
		assert.True(t, info.IsDir())
	}
}

func TestSandboxProfileSpawnRejectsAmbientCapabilityWideningAfterParentLaunch(t *testing.T) {
	for _, scope := range []string{"global", "group"} {
		t.Run(scope, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("crew")
			parent := f.AsHuman().SpawnWith("crew", map[string]any{
				"name": "parent", "approval": "bypassPermissions",
			})
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
				"name": "child", "cwd": t.TempDir(), "approval": "default",
			})
			require.Equal(t, http.StatusForbidden, denied.Code)
			assert.Contains(t, string(denied.Raw), "sandbox_profile_restricted")
			assert.Contains(t, string(denied.Raw), writeRoot)
		})
	}
}

func TestSandboxProfileSpawnRejectsExplicitInternetWidening(t *testing.T) {
	makeInternetProfile := func(t *testing.T) {
		t.Helper()
		_, err := db.CreateSandboxProfile(&db.SandboxProfile{
			Name: "explicit-internet", NetworkAccess: sandboxpolicy.NetworkAccessInternet,
		})
		require.NoError(t, err)
		_, err = db.SetAgentGroupSandboxProfile("crew", "explicit-internet")
		require.NoError(t, err)
	}

	t.Run("current parent with inherited network posture", func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("crew")
		parent := f.AsHuman().SpawnWith("crew", map[string]any{
			"name": "parent", "harness": "codex", "sandbox": harness.SandboxManagedProfile,
		})
		require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
		// The simulator does not round-trip the managed permission-profile
		// pseudo-mode into its session row; pin the production launch posture so
		// the independent OS-sandbox lineage guard permits the child request.
		parentSession, err := db.FindSessionByConvID(parent.ConvID)
		require.NoError(t, err)
		require.NotNil(t, parentSession)
		parentSession.SandboxMode = harness.SandboxManagedProfile
		require.NoError(t, db.SaveSession(parentSession))
		require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsSpawn, "test"))
		makeInternetProfile(t)

		denied := f.AsAgent(parent.ConvID).SpawnWith("crew", map[string]any{
			"name": "child", "harness": "codex", "sandbox": harness.SandboxManagedProfile,
		})
		require.Equal(t, http.StatusForbidden, denied.Code)
		assert.Contains(t, string(denied.Raw), "sandbox_profile_restricted")
		assert.Contains(t, string(denied.Raw), "network access")
	})

	t.Run("parent predating effective snapshots", func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("crew")
		const parent = "legacy-1111-2222-3333-444444444444"
		f.HaveMember("crew", parent)
		require.NoError(t, db.GrantAgentPermission(parent, agentd.PermGroupsSpawn, "test"))
		makeInternetProfile(t)

		denied := f.AsAgent(parent).SpawnWith("crew", map[string]any{
			"name": "child", "harness": "codex", "sandbox": harness.SandboxManagedProfile,
		})
		require.Equal(t, http.StatusForbidden, denied.Code)
		assert.Contains(t, string(denied.Raw), "sandbox_profile_restricted")
		assert.Contains(t, string(denied.Raw), "predates effective sandbox snapshots")
	})
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
		"name": "parent", "cwd": t.TempDir(), "approval": "bypassPermissions",
	})
	require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsSpawn, "test"))

	childCwd, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	rec := agentReq(t, f, parent.ConvID, http.MethodPost,
		"/v1/groups/"+url.PathEscape("crew")+"/spawn",
		map[string]any{"name": "child", "cwd": childCwd, "approval": "default"})
	challenge := decodeWriteProofChallenge(t, rec)
	assert.ElementsMatch(t, []string{childCwd, writeRoot}, challenge.WriteProof.Dirs)

	// The challenge is observational in this test; ensure no marker was
	// accidentally materialised by the daemon itself.
	_, statErr := os.Lstat(filepath.Join(writeRoot, challenge.WriteProof.Filename))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestMissingSandboxProfileWriteRootProofsNearestExistingAncestor(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	writeParent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	missingWriteRoot := filepath.Join(writeParent, "future", "cache")
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "future-write",
		Filesystem: []db.SandboxFilesystemGrant{{
			Path: missingWriteRoot, Access: "write",
		}},
	})
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile("crew", "future-write")
	require.NoError(t, err)

	parent := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "parent", "cwd": t.TempDir(), "approval": "bypassPermissions",
	})
	require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsSpawn, "test"))

	childCwd, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	rec := agentReq(t, f, parent.ConvID, http.MethodPost,
		"/v1/groups/"+url.PathEscape("crew")+"/spawn",
		map[string]any{"name": "child", "cwd": childCwd, "approval": "default"})
	challenge := decodeWriteProofChallenge(t, rec)
	assert.ElementsMatch(t, []string{childCwd, writeParent}, challenge.WriteProof.Dirs)
	assert.NotContains(t, challenge.WriteProof.Dirs, missingWriteRoot)
}

func TestSandboxProfileUnsupportedFilesystemFailsTypedBeforeSpawn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	root := t.TempDir()
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "filesystem", Filesystem: []db.SandboxFilesystemGrant{{Path: root, Access: "read"}},
	})
	require.NoError(t, err)

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "harness": "codex", "sandbox": "read-only", "sandbox_profile": "filesystem",
	})
	require.Equal(t, http.StatusUnprocessableEntity, resp.Code)
	assert.Contains(t, string(resp.Raw), "unsupported_sandbox_profile_filesystem")
	assert.NotContains(t, string(resp.Raw), "timeout")
}

func TestDangerFullAccessOmitsAssignedAndExplicitSandboxProfiles(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	root := t.TempDir()
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "filesystem", Filesystem: []db.SandboxFilesystemGrant{{Path: root, Access: "read"}},
	})
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile("crew", "filesystem")
	require.NoError(t, err)

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "harness": "codex", "sandbox": "danger-full-access", "sandbox_profile": "filesystem",
	})
	require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)

	snapshot, err := db.AgentEffectiveSandboxConfigForConv(resp.ConvID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	assert.Empty(t, snapshot.Applied)
	assert.Empty(t, snapshot.Effective.Filesystem)
	assert.Empty(t, snapshot.Effective.Environment)
}
