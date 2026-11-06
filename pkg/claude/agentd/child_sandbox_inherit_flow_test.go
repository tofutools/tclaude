package agentd_test

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Child sandbox step 2 (tcl-23i0pp): an agent's child naming no profile,
// spawned into its parent's own group, inherits the parent's selected
// profiles, resolved the way the parent's own next relaunch would resolve
// them. The rows refer to tcl-a0ba04's scenario table.

// spawnParentWithProfile spawns a parent in crew under an explicitly selected
// sandbox profile, allowed to spawn into crew.
func spawnParentWithProfile(t *testing.T, f *testharness.Flow, profile string) string {
	t.Helper()
	conv := spawnWithSandboxProfile(t, f, profile)
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermGroupsMembersSpawn, "test"))
	return conv
}

func childPolicy(t *testing.T, f *testharness.Flow, conv string) *sandboxpolicy.Snapshot {
	t.Helper()
	policy, ok := f.World.SpawnSandboxPolicy(conv)
	require.True(t, ok)
	return policy
}

// Row 1: the parent's own selection is the child's, not the group's.
func TestChildSandboxInheritsTheParentsSelectedProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	for name, value := range map[string]string{"mine": "parent's", "groups": "group's"} {
		_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: name,
			Environment: []db.SandboxEnvironmentEntry{{Name: "PROFILE_ENV", Value: value}}})
		require.NoError(t, err)
	}
	_, err := db.SetAgentGroupSandboxProfile("crew", "groups")
	require.NoError(t, err)
	parent := spawnParentWithProfile(t, f, "mine")

	child := f.AsAgent(parent).SpawnWith("crew", map[string]any{"name": "child", "cwd": t.TempDir()})
	require.Equalf(t, http.StatusOK, child.Code, "spawn body=%s", child.Raw)
	policy := childPolicy(t, f, child.ConvID)
	assert.Equal(t, "parent's", profileEnvValue(t, policy))
	var names []string
	for _, applied := range policy.Applied {
		names = append(names, string(applied.Scope)+":"+applied.Name)
	}
	assert.Contains(t, names, string(sandboxpolicy.ScopeExplicit)+":mine", "the child's own relaunch keeps the inherited selection")

	// A child that names a profile still gets that profile.
	named := f.AsAgent(parent).SpawnWith("crew", map[string]any{"name": "named", "cwd": t.TempDir(), "sandbox_profile": "mine"})
	require.Equalf(t, http.StatusOK, named.Code, "spawn body=%s", named.Raw)
	assert.Equal(t, "parent's", profileEnvValue(t, childPolicy(t, f, named.ConvID)))
}

// Rows 3 and 4: an operator's edit to the parent's profile reaches the child
// as it would reach the parent's next relaunch. Re-resolve (the default)
// gives the edited contents, reuse the contents stored at the parent's launch.
func TestChildSandboxFollowsTheParentsProfileContentsPolicy(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "mine",
		Environment: []db.SandboxEnvironmentEntry{{Name: "PROFILE_ENV", Value: "v1"}}})
	require.NoError(t, err)
	parent := spawnParentWithProfile(t, f, "mine")
	editProfileEnv(t, "mine", "v2")

	child := f.AsAgent(parent).SpawnWith("crew", map[string]any{"name": "child", "cwd": t.TempDir()})
	require.Equalf(t, http.StatusOK, child.Code, "spawn body=%s", child.Raw)
	assert.Equal(t, "v2", profileEnvValue(t, childPolicy(t, f, child.ConvID)), "re-resolve: the edited profile")

	setRelaunchPolicy(t, f, "sandbox_profile_contents", "reuse")
	child = f.AsAgent(parent).SpawnWith("crew", map[string]any{"name": "child-two", "cwd": t.TempDir()})
	require.Equalf(t, http.StatusOK, child.Code, "spawn body=%s", child.Raw)
	assert.Equal(t, "v1", profileEnvValue(t, childPolicy(t, f, child.ConvID)), "reuse: the parent's stored contents")
}

// Row 3, widening: a write root the operator adds to the running parent's
// profile reaches its child under re-resolve, and does not under reuse.
func TestChildSandboxInheritsAnOperatorWideningOfTheParentsProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "mine"})
	require.NoError(t, err)
	parent := spawnParentWithProfile(t, f, "mine")
	writeRoot := testutil.CanonicalTempDir(t)
	profile, err := db.GetSandboxProfile("mine")
	require.NoError(t, err)
	profile.Filesystem = []db.SandboxFilesystemGrant{{Path: writeRoot, Access: "write"}}
	require.NoError(t, db.UpdateSandboxProfile(profile))

	child := f.AsAgent(parent).SpawnWith("crew", map[string]any{"name": "child", "cwd": t.TempDir()})
	require.Equalf(t, http.StatusOK, child.Code, "spawn body=%s", child.Raw)
	assertSnapshotGrants(t, child.ConvID, writeRoot, "write")

	setRelaunchPolicy(t, f, "sandbox_profile_contents", "reuse")
	child = f.AsAgent(parent).SpawnWith("crew", map[string]any{"name": "child-two", "cwd": t.TempDir()})
	require.Equalf(t, http.StatusOK, child.Code, "spawn body=%s", child.Raw)
	snapshot, err := db.AgentEffectiveSandboxConfigForConv(child.ConvID)
	require.NoError(t, err)
	for _, grant := range snapshot.Effective.Filesystem {
		assert.NotEqual(t, writeRoot, grant.Path, "reuse keeps the parent's stored contents")
	}
}

// Inheriting needs nothing from the parent's agent-owned directories: a
// parent whose cache directory cannot be recreated still has children that
// name no profile (an explicit one is still judged against the re-probed
// stored snapshot: TestChildSandboxRuleRefusesWhenTheParentsDirectoriesCannotBeRecreated).
func TestChildSandboxInheritsWithoutProbingTheParentsDirectories(t *testing.T) {
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
	parentSession, err := db.FindSessionByConvID(parent.ConvID)
	require.NoError(t, err)
	parentSession.HarnessBuiltinMode = "tclaude-agent"
	require.NoError(t, db.SaveSession(parentSession))
	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsMembersSpawn, "test"))
	parentSnapshot, err := db.AgentEffectiveSandboxConfigForConv(parent.ConvID)
	require.NoError(t, err)
	var gocache string
	for _, entry := range parentSnapshot.Effective.Environment {
		if entry.Name == "GOCACHE" {
			gocache = entry.Value
		}
	}
	require.NotEmpty(t, gocache)
	require.NoError(t, os.RemoveAll(gocache))
	require.NoError(t, os.WriteFile(gocache, []byte("not a directory"), 0o600))

	child := f.AsAgent(parent.ConvID).SpawnWith("crew", map[string]any{
		"name": "child", "harness": "codex", "sandbox": "tclaude-agent",
	})
	require.Equalf(t, http.StatusOK, child.Code, "spawn body=%s", child.Raw)
}

// Federation and peer spawns have no parent sandbox here: a peer's spawn
// takes the target group's assignment as it always has, neither inherited
// from nor capped by any local agent.
func TestChildSandboxRuleLeavesFederationSpawnsUnchanged(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("team")
	writeRoot := testutil.CanonicalTempDir(t)
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "team-wide",
		Filesystem:  []db.SandboxFilesystemGrant{{Path: writeRoot, Access: "write"}},
		Environment: []db.SandboxEnvironmentEntry{{Name: "PROFILE_ENV", Value: "team"}}})
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile("team", "team-wide")
	require.NoError(t, err)
	worker := fedAutoSpawn(t, fh, "team", "worker-one")
	assert.Equal(t, "team", profileEnvValue(t, childPolicy(t, f, worker)))
	assertSnapshotGrants(t, worker, writeRoot, "write")
}

// fedAutoSpawn has the peer request a spawn into group under a spawn grant,
// waits for its automatic approval, and returns the worker's conversation.
func fedAutoSpawn(t *testing.T, fh *fedHarness, group, name string) string {
	t.Helper()
	f, p := fh.f, fh.peer
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawn, "scope": "group=" + group})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	env := p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: group, Name: name, Brief: "work"})
	p.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, env.ID).Status)
	var approved *db.FederationSpawnRequest
	fedEventually(t, "automatic approval", func() bool {
		rows, err := db.ListFederationSpawnRequests(100)
		if err != nil {
			return false
		}
		for _, row := range rows {
			if row.EnvelopeID == env.ID && row.Status == db.FedSpawnApproved {
				approved = row
				return true
			}
		}
		return false
	})
	actor, err := db.GetAgent(approved.ResultAgent)
	require.NoError(t, err)
	f.AssertGroupMember(group, actor.CurrentConvID, name, 5*time.Second)
	return actor.CurrentConvID
}

// A spawn grant scoped to a sandbox profile is judged against the target
// group's assignment, so the child it admits gets that assignment: it does
// not inherit a parent profile the grant never named (cold review).
func TestChildSandboxDoesNotInheritPastASandboxProfileScopedGrant(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	shared, extra := testutil.CanonicalTempDir(t), testutil.CanonicalTempDir(t)
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "restricted",
		Filesystem: []db.SandboxFilesystemGrant{{Path: shared, Access: "read"}}})
	require.NoError(t, err)
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{Name: "wide",
		Filesystem: []db.SandboxFilesystemGrant{{Path: shared, Access: "write"}, {Path: extra, Access: "write"}}})
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile("crew", "restricted")
	require.NoError(t, err)
	parent := spawnWithSandboxProfile(t, f, "wide")
	require.NoError(t, db.SetAgentPermissionOverrideWithScope(parent, agentd.PermAgentSpawn, db.PermEffectGrant,
		`{"group":["crew"],"sandbox_profile":["restricted"]}`, "test"))

	child := f.AsAgent(parent).SpawnWith("crew", map[string]any{"name": "child", "cwd": t.TempDir()})
	require.Equalf(t, http.StatusOK, child.Code, "spawn body=%s", child.Raw)
	assertSnapshotGrants(t, child.ConvID, shared, "read")
	snapshot, err := db.AgentEffectiveSandboxConfigForConv(child.ConvID)
	require.NoError(t, err)
	for _, grant := range snapshot.Effective.Filesystem {
		assert.NotEqual(t, extra, grant.Path, "the parent's wider profile is not inherited past the grant's scope")
	}
}

// The parent's launch-time access notices, an operator's one-launch override
// among them, are not part of what its child inherits, even under reuse
// (cold review).
func TestChildSandboxDoesNotInheritTheParentsLaunchOverrides(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "mine",
		Environment: []db.SandboxEnvironmentEntry{{Name: "PROFILE_ENV", Value: "v1"}}})
	require.NoError(t, err)
	parent := spawnParentWithProfile(t, f, "mine")
	setRelaunchPolicy(t, f, "sandbox_profile_contents", "reuse")
	agentID, err := db.AgentIDForConv(parent)
	require.NoError(t, err)
	stored, err := db.AgentEffectiveSandboxConfigForConv(parent)
	require.NoError(t, err)
	stored.Effective.AccessNotices = append(stored.Effective.AccessNotices, sandboxpolicy.AccessNotice{
		Class: sandboxpolicy.AccessNoticeClassDegradation, Axis: "network",
		Reason: sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride,
		Effect: "widened", Detail: "operator allowed an unenforced launch",
	})
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, stored))

	child := f.AsAgent(parent).SpawnWith("crew", map[string]any{"name": "child", "cwd": t.TempDir()})
	require.Equalf(t, http.StatusOK, child.Code, "spawn body=%s", child.Raw)
	policy := childPolicy(t, f, child.ConvID)
	assert.Equal(t, "v1", profileEnvValue(t, policy), "reuse: the stored contents")
	for _, notice := range policy.Effective.AccessNotices {
		assert.NotEqual(t, sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride, notice.Reason)
	}
}
