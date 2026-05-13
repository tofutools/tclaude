package db

import (
	"errors"
	"testing"
)

// TestAgentGroupLink_InsertAndList covers the bare CRUD: insert one
// link, list it back from both sides, and reject duplicates.
func TestAgentGroupLink_InsertAndList(t *testing.T) {
	setupTestDB(t)
	a, _ := CreateAgentGroup("alpha", "")
	b, _ := CreateAgentGroup("beta", "")

	id, err := InsertAgentGroupLink(a, b, LinkModeMembersToMembers, "")
	if err != nil || id == 0 {
		t.Fatalf("InsertAgentGroupLink = %d %v", id, err)
	}

	// Duplicate (same triple) → ErrLinkExists.
	if _, err := InsertAgentGroupLink(a, b, LinkModeMembersToMembers, ""); !errors.Is(err, ErrLinkExists) {
		t.Errorf("duplicate insert err = %v, want ErrLinkExists", err)
	}

	out, err := ListAgentGroupLinks(a, LinkOut)
	if err != nil || len(out) != 1 || out[0].ID != id {
		t.Fatalf("ListAgentGroupLinks(a, out) = %v %v", out, err)
	}
	in, err := ListAgentGroupLinks(b, LinkIn)
	if err != nil || len(in) != 1 || in[0].ID != id {
		t.Fatalf("ListAgentGroupLinks(b, in) = %v %v", in, err)
	}
	both, _ := ListAgentGroupLinks(a, LinkBoth)
	if len(both) != 1 {
		t.Errorf("ListAgentGroupLinks(a, both) len = %d, want 1", len(both))
	}
}

// TestAgentGroupLink_SelfLinkRejected: A→A is meaningless and refused.
func TestAgentGroupLink_SelfLinkRejected(t *testing.T) {
	setupTestDB(t)
	a, _ := CreateAgentGroup("alpha", "")
	if _, err := InsertAgentGroupLink(a, a, LinkModeMembersToMembers, ""); err == nil {
		t.Fatal("self-link should be rejected")
	}
}

// TestAgentGroupLink_InvalidModeRejected: unknown modes refused.
func TestAgentGroupLink_InvalidModeRejected(t *testing.T) {
	setupTestDB(t)
	a, _ := CreateAgentGroup("alpha", "")
	b, _ := CreateAgentGroup("beta", "")
	if _, err := InsertAgentGroupLink(a, b, "nonsense->stuff", ""); err == nil {
		t.Fatal("invalid mode should be rejected")
	}
}

// TestAgentGroupLink_UpdateMode: changing mode succeeds, rejects unknown
// modes, and surfaces ErrLinkExists on collision with another row.
func TestAgentGroupLink_UpdateMode(t *testing.T) {
	setupTestDB(t)
	a, _ := CreateAgentGroup("alpha", "")
	b, _ := CreateAgentGroup("beta", "")

	id, err := InsertAgentGroupLink(a, b, LinkModeMembersToMembers, "")
	if err != nil {
		t.Fatal(err)
	}

	n, err := UpdateAgentGroupLinkMode(id, LinkModeOwnersToMembers)
	if err != nil || n != 1 {
		t.Fatalf("UpdateAgentGroupLinkMode = %d %v", n, err)
	}
	got, err := GetAgentGroupLinkByID(id)
	if err != nil || got == nil || got.Mode != LinkModeOwnersToMembers {
		t.Fatalf("post-update link = %+v %v", got, err)
	}

	// Unknown mode rejected; row unchanged.
	if _, err := UpdateAgentGroupLinkMode(id, "garbage"); err == nil {
		t.Errorf("invalid mode should reject")
	}
	if got, _ := GetAgentGroupLinkByID(id); got == nil || got.Mode != LinkModeOwnersToMembers {
		t.Errorf("invalid update should leave row unchanged, got %+v", got)
	}

	// Insert a second row at the new mode, then try to update the
	// first into the same triple — should hit the UNIQUE constraint
	// and surface ErrLinkExists.
	id2, err := InsertAgentGroupLink(a, b, LinkModeMembersToMembers, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateAgentGroupLinkMode(id2, LinkModeOwnersToMembers); !errors.Is(err, ErrLinkExists) {
		t.Errorf("collision update err = %v, want ErrLinkExists", err)
	}

	// Update to the same mode is a no-op (rows-affected = 0 on SQLite
	// when nothing changes; the helper still returns the count).
	if _, err := UpdateAgentGroupLinkMode(id, LinkModeOwnersToMembers); err != nil {
		t.Errorf("no-op update err = %v", err)
	}

	// Updating a missing id returns 0 rows + no error.
	if n, err := UpdateAgentGroupLinkMode(9999, LinkModeMembersToMembers); err != nil || n != 0 {
		t.Errorf("missing id update = %d %v, want 0 nil", n, err)
	}
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
	if err != nil || n != 1 {
		t.Fatalf("DeleteAgentGroupLink = %d %v", n, err)
	}

	if err := DeleteAgentGroup("beta"); err != nil {
		t.Fatalf("DeleteAgentGroup: %v", err)
	}
	// Both link rows referencing beta should have cascaded.
	if l, _ := GetAgentGroupLinkByID(id2); l != nil {
		t.Errorf("link %d should cascade-delete with beta, got %+v", id2, l)
	}
}

// TestCanSenderReachTarget_ViaLink: alice in proj-falcon, security in
// proj-security, link proj-falcon→proj-security. alice→security
// resolves via-link.
func TestCanSenderReachTarget_ViaLink(t *testing.T) {
	setupTestDB(t)
	falcon, _ := CreateAgentGroup("proj-falcon", "")
	secReview, _ := CreateAgentGroup("proj-security", "")

	if err := AddAgentGroupMember(&AgentGroupMember{GroupID: falcon, ConvID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := AddAgentGroupMember(&AgentGroupMember{GroupID: secReview, ConvID: "security"}); err != nil {
		t.Fatal(err)
	}

	// Without the link, alice cannot reach security.
	via, _, _ := CanSenderReachTarget("alice", "security")
	if via != nil {
		t.Fatalf("pre-link alice→security should fail, got %q", via.Name)
	}

	// Add the link; alice→security should now succeed via-link.
	linkID, err := InsertAgentGroupLink(falcon, secReview, LinkModeMembersToMembers, "")
	if err != nil {
		t.Fatal(err)
	}

	via, reason, err := CanSenderReachTarget("alice", "security")
	if err != nil || via == nil {
		t.Fatalf("alice→security should reach via-link: %+v %q %v", via, reason, err)
	}
	if via.Name != "proj-falcon" {
		t.Errorf("via group = %q, want proj-falcon (sender's group)", via.Name)
	}
	wantReason := "via-link:" // followed by the id; prefix check is enough
	if len(reason) < len(wantReason) || reason[:len(wantReason)] != wantReason {
		t.Errorf("reason = %q, want prefix %q", reason, wantReason)
	}

	// Link direction matters: security→alice still fails (reverse not
	// configured).
	via, _, _ = CanSenderReachTarget("security", "alice")
	if via != nil {
		t.Errorf("reverse direction should fail, got %q", via.Name)
	}

	// Remove the link; alice→security fails again.
	if _, err := DeleteAgentGroupLink(linkID); err != nil {
		t.Fatal(err)
	}
	via, _, _ = CanSenderReachTarget("alice", "security")
	if via != nil {
		t.Errorf("post-delete alice→security should fail, got %q", via.Name)
	}
}

// TestCanSenderReachTarget_OwnerOnlyMode: owners->members lets an
// owner of the source group cross the link without being a member.
// A plain member of the source group cannot use the link.
func TestCanSenderReachTarget_OwnerOnlyMode(t *testing.T) {
	setupTestDB(t)
	mgmt, _ := CreateAgentGroup("mgmt", "")
	floor, _ := CreateAgentGroup("floor", "")

	// owner-only-member of mgmt; alice is just a member.
	if err := AddAgentGroupOwner(mgmt, "boss", ""); err != nil {
		t.Fatal(err)
	}
	if err := AddAgentGroupMember(&AgentGroupMember{GroupID: mgmt, ConvID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := AddAgentGroupMember(&AgentGroupMember{GroupID: floor, ConvID: "worker"}); err != nil {
		t.Fatal(err)
	}
	if _, err := InsertAgentGroupLink(mgmt, floor, LinkModeOwnersToMembers, ""); err != nil {
		t.Fatal(err)
	}

	// boss → worker: owner side satisfied, target member, via-link OK.
	via, reason, _ := CanSenderReachTarget("boss", "worker")
	if via == nil || via.Name != "mgmt" {
		t.Fatalf("boss→worker via=%+v reason=%q", via, reason)
	}

	// alice → worker: member of mgmt, but mode requires owner; fail.
	via, _, _ = CanSenderReachTarget("alice", "worker")
	if via != nil {
		t.Errorf("alice→worker should fail (members can't use owners->members), got %q", via.Name)
	}
}

// TestCanSenderReachTarget_SharedGroupPriority: when the sender shares
// a group with the target AND a link reaches them, shared-group wins
// (auth log stability).
func TestCanSenderReachTarget_SharedGroupPriority(t *testing.T) {
	setupTestDB(t)
	a, _ := CreateAgentGroup("alpha", "")
	b, _ := CreateAgentGroup("beta", "")
	if err := AddAgentGroupMember(&AgentGroupMember{GroupID: a, ConvID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := AddAgentGroupMember(&AgentGroupMember{GroupID: a, ConvID: "bob"}); err != nil {
		t.Fatal(err)
	}
	if err := AddAgentGroupMember(&AgentGroupMember{GroupID: b, ConvID: "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := InsertAgentGroupLink(a, b, LinkModeMembersToMembers, ""); err != nil {
		t.Fatal(err)
	}

	via, reason, _ := CanSenderReachTarget("alice", "bob")
	if via == nil || via.Name != "alpha" || reason != "shared-group" {
		t.Errorf("alice→bob via=%+v reason=%q, want alpha/shared-group", via, reason)
	}
}
