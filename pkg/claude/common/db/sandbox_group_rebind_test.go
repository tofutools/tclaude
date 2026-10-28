package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// TestSandboxResolutionGroupFollowsMembership pins the recorded sandbox source
// group across membership changes: a second group leaves it alone, a move
// (join the new group, leave the old) rebinds it, deleting the group clears it,
// and a later join picks the new group up. The rest of the snapshot is kept.
func TestSandboxResolutionGroupFollowsMembership(t *testing.T) {
	setupTestDB(t)
	const convID = "rebind-conv"
	agentID, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)

	groups := map[string]int64{}
	for _, name := range []string{"launch", "other", "target", "later"} {
		groups[name], err = CreateAgentGroup(name, "")
		require.NoError(t, err)
	}
	join := func(name string, at time.Time) {
		t.Helper()
		require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: groups[name], ConvID: convID, JoinedAt: at}))
	}
	recorded := func() *sandboxpolicy.Snapshot {
		t.Helper()
		snapshot, err := AgentEffectiveSandboxConfigForConv(convID)
		require.NoError(t, err)
		require.NotNil(t, snapshot)
		return snapshot
	}
	base := time.Now().Add(-time.Hour)
	join("launch", base)

	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.ResolutionGroupID = groups["launch"]
	snapshot.RefreshGroupEnvironment = true
	snapshot.LaunchEnvironment = []sandboxpolicy.EnvironmentEntry{{Name: "KEEP", Value: "me"}}
	require.NoError(t, SetAgentEffectiveSandboxConfig(agentID, &snapshot))

	join("other", base.Add(time.Minute))
	assert.Equal(t, groups["launch"], recorded().ResolutionGroupID, "a second group does not move the launch group")

	// The dashboard's move: add to the target, then remove from the source.
	join("target", base.Add(2*time.Minute))
	require.NoError(t, RemoveAgentGroupMember(groups["launch"], convID))
	got := recorded()
	assert.Equal(t, groups["target"], got.ResolutionGroupID, "a move rebinds to the group just joined")
	assert.Equal(t, snapshot.LaunchEnvironment, got.LaunchEnvironment, "the rest of the snapshot is untouched")

	require.NoError(t, DeleteAgentGroup("target"))
	assert.Equal(t, groups["other"], recorded().ResolutionGroupID, "deleting the group falls back to a remaining one")

	require.NoError(t, RemoveAgentGroupMember(groups["other"], convID))
	assert.Zero(t, recorded().ResolutionGroupID, "no groups left clears the record")

	join("later", base.Add(3*time.Minute))
	assert.Equal(t, groups["later"], recorded().ResolutionGroupID, "the next group joined is picked up")
}

// TestSandboxResolutionGroupLeavesLegacySnapshotsAlone: a snapshot that never
// recorded a source group is not assigned one by joining groups.
func TestSandboxResolutionGroupLeavesLegacySnapshotsAlone(t *testing.T) {
	setupTestDB(t)
	const convID = "legacy-rebind-conv"
	agentID, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	legacy := sandboxpolicy.EmptySnapshot()
	require.NoError(t, SetAgentEffectiveSandboxConfig(agentID, &legacy))
	groupID, err := CreateAgentGroup("joined", "")
	require.NoError(t, err)
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: groupID, ConvID: convID}))
	got, err := AgentEffectiveSandboxConfigForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Zero(t, got.ResolutionGroupID)
}

// TestSandboxResolutionGroupRebindSkipsAgentsWithoutSnapshot: membership changes
// on an agent with no recorded sandbox write nothing.
func TestSandboxResolutionGroupRebindSkipsAgentsWithoutSnapshot(t *testing.T) {
	setupTestDB(t)
	groupID, err := CreateAgentGroup("plain", "")
	require.NoError(t, err)
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: groupID, ConvID: "no-snapshot-conv"}))
	snapshot, err := AgentEffectiveSandboxConfigForConv("no-snapshot-conv")
	require.NoError(t, err)
	assert.Nil(t, snapshot)
}
