package db

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentGroupLink_InsertAndList covers the bare CRUD: insert one
// link, list it back from both sides, and reject duplicates.
func TestAgentGroupLink_InsertAndList(t *testing.T) {
	setupTestDB(t)
	a, _ := CreateAgentGroup("alpha", "")
	b, _ := CreateAgentGroup("beta", "")

	id, err := InsertAgentGroupLink(a, b, LinkModeMembersToMembers, "")
	require.NoError(t, err, "InsertAgentGroupLink")
	require.NotZero(t, id, "InsertAgentGroupLink id")

	// Duplicate (same triple) → ErrLinkExists.
	_, err = InsertAgentGroupLink(a, b, LinkModeMembersToMembers, "")
	assert.True(t, errors.Is(err, ErrLinkExists), "duplicate insert err = %v, want ErrLinkExists", err)

	out, err := ListAgentGroupLinks(a, LinkOut)
	require.NoError(t, err, "ListAgentGroupLinks(a, out)")
	require.Len(t, out, 1, "ListAgentGroupLinks(a, out)")
	require.Equal(t, id, out[0].ID)
	in, err := ListAgentGroupLinks(b, LinkIn)
	require.NoError(t, err, "ListAgentGroupLinks(b, in)")
	require.Len(t, in, 1, "ListAgentGroupLinks(b, in)")
	require.Equal(t, id, in[0].ID)
	both, _ := ListAgentGroupLinks(a, LinkBoth)
	assert.Len(t, both, 1, "ListAgentGroupLinks(a, both)")
}

// TestAgentGroupLink_SelfLinkRejected: A→A is meaningless and refused.
func TestAgentGroupLink_SelfLinkRejected(t *testing.T) {
	setupTestDB(t)
	a, _ := CreateAgentGroup("alpha", "")
	_, err := InsertAgentGroupLink(a, a, LinkModeMembersToMembers, "")
	require.Error(t, err, "self-link should be rejected")
}

// TestAgentGroupLink_InvalidModeRejected: unknown modes refused.
func TestAgentGroupLink_InvalidModeRejected(t *testing.T) {
	setupTestDB(t)
	a, _ := CreateAgentGroup("alpha", "")
	b, _ := CreateAgentGroup("beta", "")
	_, err := InsertAgentGroupLink(a, b, "nonsense->stuff", "")
	require.Error(t, err, "invalid mode should be rejected")
}

// TestAgentGroupLink_UpdateMode: changing mode succeeds, rejects unknown
// modes, and surfaces ErrLinkExists on collision with another row.
func TestAgentGroupLink_UpdateMode(t *testing.T) {
	setupTestDB(t)
	a, _ := CreateAgentGroup("alpha", "")
	b, _ := CreateAgentGroup("beta", "")

	id, err := InsertAgentGroupLink(a, b, LinkModeMembersToMembers, "")
	require.NoError(t, err)

	n, err := UpdateAgentGroupLinkMode(id, LinkModeOwnersToMembers)
	require.NoError(t, err, "UpdateAgentGroupLinkMode")
	require.Equal(t, int64(1), n, "UpdateAgentGroupLinkMode rows")
	got, err := GetAgentGroupLinkByID(id)
	require.NoError(t, err, "GetAgentGroupLinkByID")
	require.NotNil(t, got, "post-update link")
	require.Equal(t, LinkModeOwnersToMembers, got.Mode, "post-update link mode")

	// Unknown mode rejected; row unchanged.
	_, err = UpdateAgentGroupLinkMode(id, "garbage")
	assert.Error(t, err, "invalid mode should reject")
	got, _ = GetAgentGroupLinkByID(id)
	if assert.NotNil(t, got, "invalid update should leave row unchanged") {
		assert.Equal(t, LinkModeOwnersToMembers, got.Mode, "invalid update should leave row unchanged, got %+v", got)
	}

	// Insert a second row at the new mode, then try to update the
	// first into the same triple — should hit the UNIQUE constraint
	// and surface ErrLinkExists.
	id2, err := InsertAgentGroupLink(a, b, LinkModeMembersToMembers, "")
	require.NoError(t, err)
	_, err = UpdateAgentGroupLinkMode(id2, LinkModeOwnersToMembers)
	assert.True(t, errors.Is(err, ErrLinkExists), "collision update err = %v, want ErrLinkExists", err)

	// Update to the same mode is a no-op (rows-affected = 0 on SQLite
	// when nothing changes; the helper still returns the count).
	_, err = UpdateAgentGroupLinkMode(id, LinkModeOwnersToMembers)
	assert.NoError(t, err, "no-op update")

	// Updating a missing id returns 0 rows + no error.
	n, err = UpdateAgentGroupLinkMode(9999, LinkModeMembersToMembers)
	assert.NoError(t, err, "missing id update")
	assert.Equal(t, int64(0), n, "missing id update rows")
}

// TestAgentGroupLink_DeleteAndCascade: explicit delete works; deleting
// the source group cascades.
func TestAgentGroupLink_DeleteAndCascade(t *testing.T) {
	setupTestDB(t)
	a, _ := CreateAgentGroup("alpha", "")
	b, _ := CreateAgentGroup("beta", "")
	id, _ := InsertAgentGroupLink(a, b, LinkModeMembersToMembers, "")
	id2, _ := InsertAgentGroupLink(b, a, LinkModeMembersToMembers, "")

	n, err := DeleteAgentGroupLink(id)
	require.NoError(t, err, "DeleteAgentGroupLink")
	require.Equal(t, int64(1), n, "DeleteAgentGroupLink rows")

	require.NoError(t, DeleteAgentGroup("beta"), "DeleteAgentGroup")
	// Both link rows referencing beta should have cascaded.
	l, _ := GetAgentGroupLinkByID(id2)
	assert.Nil(t, l, "link %d should cascade-delete with beta, got %+v", id2, l)
}

// TestCanSenderReachTarget_ViaLink: alice in proj-falcon, security in
// proj-security, link proj-falcon→proj-security. alice→security
// resolves via-link.
func TestCanSenderReachTarget_ViaLink(t *testing.T) {
	setupTestDB(t)
	falcon, _ := CreateAgentGroup("proj-falcon", "")
	secReview, _ := CreateAgentGroup("proj-security", "")

	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: falcon, ConvID: "alice"}))
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: secReview, ConvID: "security"}))

	// Without the link, alice cannot reach security.
	via, _, _ := CanSenderReachTarget("alice", "security")
	require.Nil(t, via, "pre-link alice→security should fail")

	// Add the link; alice→security should now succeed via-link.
	linkID, err := InsertAgentGroupLink(falcon, secReview, LinkModeMembersToMembers, "")
	require.NoError(t, err)

	via, reason, err := CanSenderReachTarget("alice", "security")
	require.NoError(t, err, "alice→security should reach via-link")
	require.NotNil(t, via, "alice→security should reach via-link: reason=%q", reason)
	assert.Equal(t, "proj-falcon", via.Name, "via group, want proj-falcon (sender's group)")
	wantReason := "via-link:" // followed by the id; prefix check is enough
	assert.True(t, len(reason) >= len(wantReason) && reason[:len(wantReason)] == wantReason, "reason = %q, want prefix %q", reason, wantReason)

	// Link direction matters: security→alice still fails (reverse not
	// configured).
	via, _, _ = CanSenderReachTarget("security", "alice")
	assert.Nil(t, via, "reverse direction should fail")

	// Remove the link; alice→security fails again.
	_, err = DeleteAgentGroupLink(linkID)
	require.NoError(t, err)
	via, _, _ = CanSenderReachTarget("alice", "security")
	assert.Nil(t, via, "post-delete alice→security should fail")
}

// TestCanSenderReachTarget_OwnerOnlyMode: owners->members lets an
// owner of the source group cross the link without being a member.
// A plain member of the source group cannot use the link.
func TestCanSenderReachTarget_OwnerOnlyMode(t *testing.T) {
	setupTestDB(t)
	mgmt, _ := CreateAgentGroup("mgmt", "")
	floor, _ := CreateAgentGroup("floor", "")

	// owner-only-member of mgmt; alice is just a member.
	require.NoError(t, AddAgentGroupOwner(mgmt, "boss", ""))
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: mgmt, ConvID: "alice"}))
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: floor, ConvID: "worker"}))
	_, err := InsertAgentGroupLink(mgmt, floor, LinkModeOwnersToMembers, "")
	require.NoError(t, err)

	// boss → worker: owner side satisfied, target member, via-link OK.
	via, reason, _ := CanSenderReachTarget("boss", "worker")
	require.NotNil(t, via, "boss→worker reason=%q", reason)
	assert.Equal(t, "mgmt", via.Name, "boss→worker via")

	// alice → worker: member of mgmt, but mode requires owner; fail.
	via, _, _ = CanSenderReachTarget("alice", "worker")
	assert.Nil(t, via, "alice→worker should fail (members can't use owners->members)")
}

// TestCanSenderReachTarget_DualRole: a conv that is BOTH a member and
// an owner of the source group should be treated as an owner for link-
// traversal purposes. Owner is the stronger role (satisfies every mode
// "member" does plus owners->members), so an owners->members link is
// reachable.
//
// Regression: original LinkReachableTargetsFor kept the member role,
// stranding dual-role senders behind owners-only links they should
// have been able to use.
func TestCanSenderReachTarget_DualRole(t *testing.T) {
	setupTestDB(t)
	mgmt, _ := CreateAgentGroup("mgmt", "")
	floor, _ := CreateAgentGroup("floor", "")

	// alice is both a member AND an owner of mgmt.
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: mgmt, ConvID: "alice"}))
	require.NoError(t, AddAgentGroupOwner(mgmt, "alice", ""))
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: floor, ConvID: "worker"}))
	_, err := InsertAgentGroupLink(mgmt, floor, LinkModeOwnersToMembers, "")
	require.NoError(t, err)

	via, reason, _ := CanSenderReachTarget("alice", "worker")
	if assert.NotNil(t, via, "alice (member+owner) → worker via owners->members link should succeed, reason=%q", reason) {
		assert.Equal(t, "mgmt", via.Name, "alice (member+owner) → worker via owners->members link should succeed, got via=%+v reason=%q", via, reason)
	}
}

// TestCanSenderReachTarget_SharedGroupPriority: when the sender shares
// a group with the target AND a link reaches them, shared-group wins
// (auth log stability).
func TestCanSenderReachTarget_SharedGroupPriority(t *testing.T) {
	setupTestDB(t)
	a, _ := CreateAgentGroup("alpha", "")
	b, _ := CreateAgentGroup("beta", "")
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: a, ConvID: "alice"}))
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: a, ConvID: "bob"}))
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: b, ConvID: "bob"}))
	_, err := InsertAgentGroupLink(a, b, LinkModeMembersToMembers, "")
	require.NoError(t, err)

	via, reason, _ := CanSenderReachTarget("alice", "bob")
	if assert.NotNil(t, via, "alice→bob reason=%q, want alpha/shared-group", reason) {
		assert.Equal(t, "alpha", via.Name, "alice→bob via")
		assert.Equal(t, "shared-group", reason, "alice→bob reason")
	}
}
