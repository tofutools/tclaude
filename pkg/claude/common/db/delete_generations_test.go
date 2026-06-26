package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteAgentByConvID_PredecessorKeepsLiveActor guards the JOH-26 delete
// semantics: identity is actor-level, so deleting a PAST conversation
// generation (a reincarnate / Claude Code /clear leaves the old conv around)
// must NOT wipe the live actor's memberships, permissions or actor row — only
// the live generation's delete tears the actor down.
func TestDeleteAgentByConvID_PredecessorKeepsLiveActor(t *testing.T) {
	setupTestDB(t)

	groupID, err := CreateAgentGroup("alpha", "")
	require.NoError(t, err, "CreateAgentGroup")

	// One actor, two generations: old → new (new is the live head).
	require.NoError(t, EnrollAgent("old", "spawn"))
	_, err = MigrateAgentIdentity("old", "new", "reincarnate", "system:test")
	require.NoError(t, err, "MigrateAgentIdentity")

	// Actor-level identity: a membership and a permission override.
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: groupID, ConvID: "new", Role: "lead"}))
	require.NoError(t, GrantAgentPermission("new", "self.compact", "test"))

	actor, err := AgentIDForConv("new")
	require.NoError(t, err)
	require.NotEmpty(t, actor)

	// --- Delete the PREDECESSOR generation. ---
	counts, err := DeleteAgentByConvID("old")
	require.NoError(t, err, "delete predecessor")
	assert.Zero(t, counts.GroupMembers, "predecessor delete must not touch actor-level memberships")
	assert.Zero(t, counts.Permissions, "predecessor delete must not touch actor-level permissions")

	// The actor and its identity survive; only the old generation is unlinked.
	still, err := GetAgent(actor)
	require.NoError(t, err)
	require.NotNil(t, still, "live actor survives a predecessor delete")
	assert.Equal(t, "new", still.CurrentConvID, "live conv pointer is unchanged")

	oldA, err := AgentIDForConv("old")
	require.NoError(t, err)
	assert.Empty(t, oldA, "the predecessor generation is unlinked")
	newA, err := AgentIDForConv("new")
	require.NoError(t, err)
	assert.Equal(t, actor, newA, "the live generation still resolves to the actor")

	members, err := ListAgentGroupMembers(groupID)
	require.NoError(t, err)
	assert.Len(t, members, 1, "the actor keeps its membership after a predecessor delete")

	// --- Delete the LIVE generation → the actor is fully torn down. ---
	counts, err = DeleteAgentByConvID("new")
	require.NoError(t, err, "delete live generation")
	assert.Equal(t, int64(1), counts.GroupMembers, "live-generation delete removes the actor's membership")

	gone, err := GetAgent(actor)
	require.NoError(t, err)
	assert.Nil(t, gone, "deleting the live generation removes the actor")

	members, err = ListAgentGroupMembers(groupID)
	require.NoError(t, err)
	assert.Empty(t, members, "no memberships remain once the actor is gone")
}
