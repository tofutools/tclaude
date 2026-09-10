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

	_, err = resolveResumeSandboxPolicy(convID, false, "", "", "", "")
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

	_, err = resolveResumeSandboxPolicy(convID, false, "", "", "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot determine the sandbox source group")
}

func TestResolveResumeSandboxPolicyPreservesExplicitProfileOmission(t *testing.T) {
	setupTestDB(t)
	const convID = "omitted-profile-resume-conv"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup("omitted-profile-group", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupEnvironment("omitted-profile-group", []sandboxpolicy.EnvironmentEntry{{Name: "GROUP", Value: "current"}})
	require.NoError(t, err)
	omitted := sandboxpolicy.OmittedProfilesSnapshot()
	omitted.ResolutionGroupID = groupID
	omitted.RefreshGroupEnvironment = true
	omitted.LaunchEnvironment = []sandboxpolicy.EnvironmentEntry{{Name: "GROUP", Value: "old"}, {Name: "EXPLICIT", Value: "frozen"}}
	omitted.LaunchEnvironmentOverrides = []sandboxpolicy.EnvironmentEntry{{Name: "EXPLICIT", Value: "frozen"}}
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &omitted))

	_, err = db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "ambient", Environment: []db.SandboxEnvironmentEntry{{Name: "AMBIENT", Value: "yes"}},
	})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("ambient"))

	resolved, err := resolveResumeSandboxPolicy(convID, false, "", "", "", "")
	require.NoError(t, err)
	require.NotNil(t, resolved)
	require.NotNil(t, resolved.Snapshot)
	assert.True(t, resolved.Snapshot.ProfilesOmitted)
	assert.Empty(t, resolved.Snapshot.Applied)
	assert.Empty(t, resolved.Snapshot.Effective.Environment)
	assert.Equal(t, []sandboxpolicy.EnvironmentEntry{{Name: "EXPLICIT", Value: "frozen"}, {Name: "GROUP", Value: "current"}},
		resolved.Snapshot.LaunchEnvironment,
		"sandbox-profile omission must not suppress the separate common group environment")
}

func TestResolveResumeSandboxPolicyDoesNotInferGroupForLegacyProfileOmission(t *testing.T) {
	setupTestDB(t)
	const convID = "legacy-omitted-profile-resume-conv"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	omitted := sandboxpolicy.OmittedProfilesSnapshot()
	omitted.Version = 13
	omitted.LaunchEnvironment = []sandboxpolicy.EnvironmentEntry{{Name: "FROZEN", Value: "yes"}}
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &omitted))

	for _, name := range []string{"alpha", "beta"} {
		groupID, createErr := db.CreateAgentGroup(name, "")
		require.NoError(t, createErr)
		require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: convID}))
		_, setErr := db.SetAgentGroupEnvironment(name, []sandboxpolicy.EnvironmentEntry{{Name: "GROUP", Value: name}})
		require.NoError(t, setErr)
	}

	resolved, err := resolveResumeSandboxPolicy(convID, false, "", "", "", "")
	require.NoError(t, err)
	require.NotNil(t, resolved.Snapshot)
	assert.Equal(t, omitted.LaunchEnvironment, resolved.Snapshot.LaunchEnvironment)
}

func TestRefreshResumeGroupEnvironmentUpgradesFlattenedSnapshot(t *testing.T) {
	setupTestDB(t)
	groupID, err := db.CreateAgentGroup("legacy-environment", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupEnvironment("legacy-environment", []sandboxpolicy.EnvironmentEntry{
		{Name: "GROUP_CHANGED", Value: "current"},
		{Name: "GROUP_NEW", Value: "added"},
	})
	require.NoError(t, err)

	previous := sandboxpolicy.EmptySnapshot()
	previous.ResolutionGroupID = groupID
	previous.RefreshGroupEnvironment = false
	previous.LaunchEnvironment = []sandboxpolicy.EnvironmentEntry{
		{Name: "FROZEN", Value: "keep"},
		{Name: "GROUP_CHANGED", Value: "old"},
	}
	current := sandboxpolicy.EmptySnapshot()
	got, err := refreshResumeGroupEnvironment("legacy-conv", current, &previous)
	require.NoError(t, err)
	assert.True(t, got.RefreshGroupEnvironment)
	assert.Equal(t, []sandboxpolicy.EnvironmentEntry{
		{Name: "FROZEN", Value: "keep"},
		{Name: "GROUP_CHANGED", Value: "current"},
		{Name: "GROUP_NEW", Value: "added"},
	}, got.LaunchEnvironment)
	assert.Equal(t, []sandboxpolicy.EnvironmentEntry{{Name: "FROZEN", Value: "keep"}},
		got.LaunchEnvironmentOverrides)
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
