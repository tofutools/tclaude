package agentd_test

import (
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// These pin the rule an agent's child gets its sandbox by, at the spawn API
// (tcl-pi13z9), beside the containment refusals the sandbox-profile spawn
// flow tests already pin. The rule is about to change in steps; these say
// what it is today.

// A parent from before effective snapshots may still spawn a child that
// carries no custom capabilities: only capabilities need a snapshot to be
// contained in.
func TestChildSandboxRuleAllowsAPreSnapshotParentAChildWithoutCapabilities(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const parent = "legacy-5555-6666-7777-888888888888"
	f.HaveMember("crew", parent)
	require.NoError(t, db.GrantAgentPermission(parent, agentd.PermGroupsMembersSpawn, "test"))

	child := f.AsAgent(parent).SpawnWith("crew", map[string]any{"name": "child"})
	require.Equalf(t, http.StatusOK, child.Code, "spawn body=%s", child.Raw)
}

// A child naming a profile explicitly is judged against the parent's
// configuration resolved now, as an inheriting one is
// (TestChildSandboxInheritsWithoutProbingTheParentsDirectories). The bound is
// judged, not launched, so the parent's own agent-owned directories are not
// probed or recreated for it: one that is broken on disk neither refuses the
// child nor is touched (tcl-n4294s).
func TestChildSandboxRuleDoesNotProbeTheParentsDirectoriesForAnExplicitProfile(t *testing.T) {
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
	// As in TestSandboxProfileAgentCanSpawnChildWithAdditionalAgentDirectories:
	// restore the launch mode the simulator drops, so the lineage gate passes
	// and the spawn reaches containment.
	parentSession, err := db.FindSessionByConvID(parent.ConvID)
	require.NoError(t, err)
	parentSession.HarnessBuiltinMode = "tclaude-agent"
	require.NoError(t, db.SaveSession(parentSession))
	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsMembersSpawn, "test"))

	parentSnapshot, err := db.AgentEffectiveSandboxConfigForConv(parent.ConvID)
	require.NoError(t, err)
	require.NotNil(t, parentSnapshot)
	var gocache string
	for _, entry := range parentSnapshot.Effective.Environment {
		if entry.Name == "GOCACHE" {
			gocache = entry.Value
		}
	}
	require.NotEmpty(t, gocache)
	// A file where the directory belongs, which a re-probe could not recreate.
	require.NoError(t, os.RemoveAll(gocache))
	require.NoError(t, os.WriteFile(gocache, []byte("not a directory"), 0o600))

	child := f.AsAgent(parent.ConvID).SpawnWith("crew", map[string]any{
		"name": "child", "harness": "codex", "sandbox": "tclaude-agent", "sandbox_profile": "parent-cache",
	})
	require.Equalf(t, http.StatusOK, child.Code, "spawn body=%s", child.Raw)
	content, err := os.ReadFile(gocache)
	require.NoError(t, err, "the parent's path is left as it was")
	assert.Equal(t, "not a directory", string(content))
}

// Row 7: a child naming a different profile must fit inside the parent's
// configuration as resolved now, not its stale launch snapshot. A write root
// the operator adds to the running parent's profile is the parent's now, so
// a child naming a profile with that root is allowed under re-resolve, and
// refused under reuse, where the parent's own relaunch would not have it.
func TestChildSandboxRuleJudgesAnExplicitProfileAgainstTheParentResolvedNow(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	writeRoot := testutil.CanonicalTempDir(t)
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "mine"})
	require.NoError(t, err)
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{Name: "other",
		Filesystem: []db.SandboxFilesystemGrant{{Path: writeRoot, Access: "write"}}})
	require.NoError(t, err)
	parent := spawnParentWithProfile(t, f, "mine")

	before := f.AsAgent(parent).SpawnWith("crew", map[string]any{"name": "before", "cwd": t.TempDir(), "sandbox_profile": "other"})
	assert.Equalf(t, http.StatusForbidden, before.Code, "wider than the parent: body=%s", before.Raw)

	profile, err := db.GetSandboxProfile("mine")
	require.NoError(t, err)
	profile.Filesystem = []db.SandboxFilesystemGrant{{Path: writeRoot, Access: "write"}}
	require.NoError(t, db.UpdateSandboxProfile(profile))

	after := f.AsAgent(parent).SpawnWith("crew", map[string]any{"name": "after", "cwd": t.TempDir(), "sandbox_profile": "other"})
	require.Equalf(t, http.StatusOK, after.Code, "the operator's widening is the parent's now: body=%s", after.Raw)

	setRelaunchPolicy(t, f, "sandbox_profile_contents", "reuse")
	reused := f.AsAgent(parent).SpawnWith("crew", map[string]any{"name": "reused", "cwd": t.TempDir(), "sandbox_profile": "other"})
	assert.Equalf(t, http.StatusForbidden, reused.Code, "reuse bounds by the stored contents: body=%s", reused.Raw)
}
