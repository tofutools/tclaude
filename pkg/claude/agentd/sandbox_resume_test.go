package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestResolveResumeSandboxPolicyRejectsAmbiguousMultiGroupAssignment(t *testing.T) {
	setupTestDB(t)
	const convID = "ambiguous-resume-sandbox-conv"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	empty := sandboxpolicy.EmptySnapshot()
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &empty))
	for _, name := range []string{"alpha", "beta"} {
		groupID, err := db.CreateAgentGroup(name, "")
		require.NoError(t, err)
		require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: convID}))
		_, err = db.CreateSandboxProfile(&db.SandboxProfile{Name: name + "-policy"})
		require.NoError(t, err)
		_, err = db.SetAgentGroupSandboxProfile(name, name+"-policy")
		require.NoError(t, err)
	}

	_, err = resolveResumeSandboxPolicy(convID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot determine the sandbox source group")
}

func TestResolveResumeSandboxPolicyDoesNotInferLegacyGroupFromStaleProfileID(t *testing.T) {
	setupTestDB(t)
	const convID = "stale-profile-id-resume-conv"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)

	oldID, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "old-policy"})
	require.NoError(t, err)
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{Name: "new-policy"})
	require.NoError(t, err)
	previous := sandboxpolicy.EmptySnapshot()
	previous.Applied = []sandboxpolicy.AppliedProfile{{
		Scope: sandboxpolicy.ScopeGroup, ID: oldID, Name: "old-policy",
	}}
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &previous))

	for _, group := range []struct{ name, profile string }{
		{name: "launch-group", profile: "new-policy"},
		{name: "other-group", profile: "old-policy"},
	} {
		groupID, createErr := db.CreateAgentGroup(group.name, "")
		require.NoError(t, createErr)
		require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: convID}))
		_, assignErr := db.SetAgentGroupSandboxProfile(group.name, group.profile)
		require.NoError(t, assignErr)
	}

	_, err = resolveResumeSandboxPolicy(convID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot determine the sandbox source group")
}
