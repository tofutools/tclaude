package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A clone's identity lands whole or not at all: a row that fails midway rolls
// back the memberships and grants written before it.
func TestCopyCloneIdentityIsAllOrNothing(t *testing.T) {
	setupTestDB(t)
	groupID, err := CreateAgentGroup("alpha", "")
	require.NoError(t, err)
	agentID, _, err := EnsureAgentForConv("clone-conv-0001", "clone")
	require.NoError(t, err)

	err = CopyCloneIdentity(agentID, CloneIdentity{
		Members: []*AgentGroupMember{{GroupID: groupID, Role: "dev"}},
		Perms: []AgentPermission{
			{Slug: "human.clipboard", Effect: PermEffectGrant},
			{Slug: "agent.stop", Effect: PermEffectDeny, ScopeJSON: `{"group":["alpha"]}`},
		},
	}, "system:clone")
	require.Error(t, err)

	groups, err := ListGroupsForConv("clone-conv-0001")
	require.NoError(t, err)
	assert.Empty(t, groups, "membership rolled back")
	perms, err := ListAgentPermissionOverrideRowsForConv("clone-conv-0001")
	require.NoError(t, err)
	assert.Empty(t, perms, "earlier grant rolled back")

	require.NoError(t, CopyCloneIdentity(agentID, CloneIdentity{
		Members:  []*AgentGroupMember{{GroupID: groupID, Role: "dev", Descr: "d"}},
		Perms:    []AgentPermission{{Slug: "human.clipboard", Effect: PermEffectGrant}},
		OwnedIDs: []int64{groupID},
	}, "system:clone"))
	member, err := FindMemberInGroup(groupID, "clone-conv-0001")
	require.NoError(t, err)
	require.NotNil(t, member)
	assert.Equal(t, "dev", member.Role)
	owned, err := ListGroupsOwnedBy("clone-conv-0001")
	require.NoError(t, err)
	assert.Equal(t, []int64{groupID}, owned)
}
