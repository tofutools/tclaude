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

	_, err = resolveResumeSandboxPolicy(convID, false, "")
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

	_, err = resolveResumeSandboxPolicy(convID, false, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot determine the sandbox source group")
}

func TestResolveResumeSandboxPolicyPreservesExplicitProfileOmission(t *testing.T) {
	setupTestDB(t)
	const convID = "omitted-profile-resume-conv"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	omitted := sandboxpolicy.OmittedProfilesSnapshot()
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &omitted))

	_, err = db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "ambient", Environment: []db.SandboxEnvironmentEntry{{Name: "AMBIENT", Value: "yes"}},
	})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("ambient"))

	resolved, err := resolveResumeSandboxPolicy(convID, false, "")
	require.NoError(t, err)
	require.NotNil(t, resolved)
	require.NotNil(t, resolved.Snapshot)
	assert.True(t, resolved.Snapshot.ProfilesOmitted)
	assert.Empty(t, resolved.Snapshot.Applied)
	assert.Empty(t, resolved.Snapshot.Effective.Environment)
}

func TestMergeResumeAccessNoticesDropsStaleDegradationAuthority(t *testing.T) {
	current := []sandboxpolicy.AccessNotice{{
		Class:  sandboxpolicy.AccessNoticeClassComposition,
		Axis:   "network",
		Reason: sandboxpolicy.AccessNoticeReasonEmptyIntersection,
		Detail: "current composition warning",
	}}
	previous := []sandboxpolicy.AccessNotice{
		{
			Class:  sandboxpolicy.AccessNoticeClassComposition,
			Axis:   "unix_sockets",
			Reason: sandboxpolicy.AccessNoticeReasonEmptyIntersection,
			Detail: "previous composition warning",
		},
		{
			Class:  sandboxpolicy.AccessNoticeClassDegradation,
			Axis:   "network",
			Reason: "no_mechanism",
			Effect: sandboxpolicy.AccessNoticeEffectNotEnforced,
			Detail: "stale launch widened the old list",
		},
	}
	got := mergeResumeAccessNotices(current, previous)
	require.Len(t, got, 2)
	assert.Equal(t, sandboxpolicy.AccessNoticeClassComposition, got[0].Class)
	assert.Equal(t, sandboxpolicy.AccessNoticeClassComposition, got[1].Class)
	assert.NotContains(t, got, previous[1])

	empty := mergeResumeAccessNotices([]sandboxpolicy.AccessNotice{}, nil)
	assert.NotNil(t, empty, "ordinary resume keeps the historical empty-slice snapshot shape")
}
