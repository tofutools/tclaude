package agentd_test

import (
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

// The parent's own agent-owned directories are re-probed before its child is
// judged against it. One that cannot be recreated at its frozen path refuses
// the child as a changed profile, not as a widening.
func TestChildSandboxRuleRefusesWhenTheParentsDirectoriesCannotBeRecreated(t *testing.T) {
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
	// A file where the directory belongs: the re-probe never retargets or
	// replaces, so it cannot recreate it.
	require.NoError(t, os.RemoveAll(gocache))
	require.NoError(t, os.WriteFile(gocache, []byte("not a directory"), 0o600))

	child := f.AsAgent(parent.ConvID).SpawnWith("crew", map[string]any{
		"name": "child", "harness": "codex", "sandbox": "tclaude-agent",
	})
	require.Equalf(t, http.StatusConflict, child.Code, "spawn body=%s", child.Raw)
	assert.Contains(t, string(child.Raw), "sandbox_profile_changed")
}
