package db

import (
	"strings"
	"testing"
	"time"
)

func TestAgentGroupCRUD(t *testing.T) {
	setupTestDB(t)

	id, err := CreateAgentGroup("alpha", "test group")
	if err != nil {
		t.Fatalf("CreateAgentGroup: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero group id")
	}

	g, err := GetAgentGroupByName("alpha")
	if err != nil {
		t.Fatalf("GetAgentGroupByName: %v", err)
	}
	if g == nil || g.Name != "alpha" || g.Descr != "test group" {
		t.Fatalf("unexpected group: %+v", g)
	}

	// Duplicate names should fail at the UNIQUE constraint.
	if _, err := CreateAgentGroup("alpha", ""); err == nil {
		t.Fatal("expected error creating duplicate group")
	}

	groups, err := ListAgentGroups()
	if err != nil {
		t.Fatalf("ListAgentGroups: %v", err)
	}
	if len(groups) != 1 || groups[0].Name != "alpha" {
		t.Fatalf("ListAgentGroups returned %+v", groups)
	}

	// Delete with no members or messages: ok.
	if err := DeleteAgentGroup("alpha"); err != nil {
		t.Fatalf("DeleteAgentGroup: %v", err)
	}
	if g, _ := GetAgentGroupByName("alpha"); g != nil {
		t.Fatal("expected group to be gone")
	}
}

func TestAgentGroupMembershipAndShared(t *testing.T) {
	setupTestDB(t)

	a, _ := CreateAgentGroup("alpha", "")
	b, _ := CreateAgentGroup("beta", "")

	if err := AddAgentGroupMember(&AgentGroupMember{
		GroupID: a, ConvID: "conv-1", Alias: "planner", Role: "lead",
	}); err != nil {
		t.Fatalf("AddAgentGroupMember: %v", err)
	}
	if err := AddAgentGroupMember(&AgentGroupMember{
		GroupID: a, ConvID: "conv-2", Alias: "reviewer",
	}); err != nil {
		t.Fatalf("AddAgentGroupMember: %v", err)
	}
	if err := AddAgentGroupMember(&AgentGroupMember{
		GroupID: b, ConvID: "conv-2",
	}); err != nil {
		t.Fatalf("AddAgentGroupMember: %v", err)
	}
	if err := AddAgentGroupMember(&AgentGroupMember{
		GroupID: b, ConvID: "conv-3",
	}); err != nil {
		t.Fatalf("AddAgentGroupMember: %v", err)
	}

	// conv-1 and conv-2 share alpha; conv-2 and conv-3 share beta.
	shared, err := SharedGroupsForConvs("conv-1", "conv-2")
	if err != nil {
		t.Fatalf("SharedGroupsForConvs: %v", err)
	}
	if len(shared) != 1 || shared[0].Name != "alpha" {
		t.Fatalf("expected [alpha], got %+v", names(shared))
	}

	shared, err = SharedGroupsForConvs("conv-1", "conv-3")
	if err != nil {
		t.Fatalf("SharedGroupsForConvs: %v", err)
	}
	if len(shared) != 0 {
		t.Fatalf("expected no shared groups for conv-1/conv-3, got %+v", names(shared))
	}

	// ListGroupsForConv returns all groups for conv-2.
	gs, err := ListGroupsForConv("conv-2")
	if err != nil {
		t.Fatalf("ListGroupsForConv: %v", err)
	}
	if len(gs) != 2 {
		t.Fatalf("expected 2 groups for conv-2, got %d", len(gs))
	}

	// Remove conv-2 from beta and the shared set with conv-3 should empty.
	if err := RemoveAgentGroupMember(b, "conv-2"); err != nil {
		t.Fatalf("RemoveAgentGroupMember: %v", err)
	}
	shared, _ = SharedGroupsForConvs("conv-2", "conv-3")
	if len(shared) != 0 {
		t.Fatalf("expected no shared groups after remove, got %+v", names(shared))
	}
}

func TestAgentPermissions_GrantRevokeIdempotent(t *testing.T) {
	setupTestDB(t)

	conv := "abcd1234-0000-0000-0000-000000000001"

	// Empty initially.
	if perms, err := ListAgentPermissionsForConv(conv); err != nil || len(perms) != 0 {
		t.Fatalf("expected empty list, got %v err=%v", perms, err)
	}
	if ok, err := HasAgentPermissionRow(conv, "self.rename"); err != nil || ok {
		t.Fatalf("expected no perm, got ok=%v err=%v", ok, err)
	}

	// Grant.
	if err := GrantAgentPermission(conv, "self.rename", "<human>"); err != nil {
		t.Fatalf("GrantAgentPermission: %v", err)
	}
	// Idempotent.
	if err := GrantAgentPermission(conv, "self.rename", "<human>"); err != nil {
		t.Fatalf("idempotent grant: %v", err)
	}
	if ok, err := HasAgentPermissionRow(conv, "self.rename"); err != nil || !ok {
		t.Fatalf("expected perm, got ok=%v err=%v", ok, err)
	}

	// Multiple slugs sort correctly.
	if err := GrantAgentPermission(conv, "member.add", ""); err != nil {
		t.Fatal(err)
	}
	perms, err := ListAgentPermissionsForConv(conv)
	if err != nil {
		t.Fatal(err)
	}
	if len(perms) != 2 || perms[0] != "member.add" || perms[1] != "self.rename" {
		t.Fatalf("expected sorted list [member.add self.rename], got %v", perms)
	}

	// Revoke.
	n, err := RevokeAgentPermission(conv, "self.rename")
	if err != nil || n != 1 {
		t.Fatalf("RevokeAgentPermission: n=%d err=%v", n, err)
	}
	// Idempotent revoke returns 0.
	n, err = RevokeAgentPermission(conv, "self.rename")
	if err != nil || n != 0 {
		t.Fatalf("idempotent revoke: n=%d err=%v", n, err)
	}

	// ListAllAgentPermissions sees the remaining slug.
	all, err := ListAllAgentPermissions()
	if err != nil {
		t.Fatal(err)
	}
	if got := all[conv]; len(got) != 1 || got[0] != "member.add" {
		t.Fatalf("expected [member.add], got %v", got)
	}
}

func TestAgentMessageInsertAndList(t *testing.T) {
	setupTestDB(t)

	g, _ := CreateAgentGroup("alpha", "")

	id1, err := InsertAgentMessage(&AgentMessage{
		GroupID:  g,
		FromConv: "conv-1",
		ToConv:   "conv-2",
		Subject:  "hello",
		Body:     "first",
	})
	if err != nil {
		t.Fatalf("InsertAgentMessage: %v", err)
	}
	id2, err := InsertAgentMessage(&AgentMessage{
		GroupID:  g,
		FromConv: "conv-1",
		ToConv:   "conv-2",
		Body:     "second",
	})
	if err != nil {
		t.Fatalf("InsertAgentMessage: %v", err)
	}

	msgs, err := ListAgentMessagesForConv("conv-2", 0)
	if err != nil {
		t.Fatalf("ListAgentMessagesForConv: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	got, err := GetAgentMessage(id1)
	if err != nil || got == nil {
		t.Fatalf("GetAgentMessage(%d): %v %+v", id1, err, got)
	}
	if got.Subject != "hello" || got.Body != "first" {
		t.Fatalf("unexpected message contents: %+v", got)
	}

	if err := MarkAgentMessageDelivered(id1); err != nil {
		t.Fatalf("MarkAgentMessageDelivered: %v", err)
	}
	if err := MarkAgentMessageRead(id1); err != nil {
		t.Fatalf("MarkAgentMessageRead: %v", err)
	}
	got, _ = GetAgentMessage(id1)
	if got.DeliveredAt.IsZero() {
		t.Error("delivered_at should be set")
	}
	if got.ReadAt.IsZero() {
		t.Error("read_at should be set")
	}

	// Deleting a group while messages reference it must fail
	// (ON DELETE RESTRICT).
	if err := DeleteAgentGroup("alpha"); err == nil {
		t.Fatal("expected DeleteAgentGroup to fail because messages reference the group")
	} else if !strings.Contains(strings.ToLower(err.Error()), "foreign") &&
		!strings.Contains(strings.ToLower(err.Error()), "constraint") {
		// modernc/sqlite returns "FOREIGN KEY constraint failed" — we
		// just want any constraint-shaped error.
		t.Logf("delete error message: %v", err)
	}

	_ = id2
}

func names(gs []*AgentGroup) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out
}

// TestAgentGroupOwnerCRUD covers the basic ownership round-trip:
// add (idempotent), is-check, list, remove (count), list-by-conv.
func TestAgentGroupOwnerCRUD(t *testing.T) {
	setupTestDB(t)
	g, _ := CreateAgentGroup("alpha", "")

	if err := AddAgentGroupOwner(g, "boss", ""); err != nil {
		t.Fatalf("AddAgentGroupOwner: %v", err)
	}
	// Idempotent — second call must not error.
	if err := AddAgentGroupOwner(g, "boss", ""); err != nil {
		t.Fatalf("AddAgentGroupOwner second: %v", err)
	}

	is, err := IsAgentGroupOwner(g, "boss")
	if err != nil || !is {
		t.Fatalf("IsAgentGroupOwner(g, boss) = %v %v, want true", is, err)
	}
	is, _ = IsAgentGroupOwner(g, "stranger")
	if is {
		t.Errorf("IsAgentGroupOwner(stranger) should be false")
	}

	owners, err := ListAgentGroupOwners(g)
	if err != nil || len(owners) != 1 || owners[0].ConvID != "boss" {
		t.Fatalf("ListAgentGroupOwners = %+v %v", owners, err)
	}

	groups, err := ListGroupsOwnedBy("boss")
	if err != nil || len(groups) != 1 || groups[0] != g {
		t.Fatalf("ListGroupsOwnedBy = %+v %v", groups, err)
	}

	n, err := RemoveAgentGroupOwner(g, "boss")
	if err != nil || n != 1 {
		t.Fatalf("RemoveAgentGroupOwner = %d %v, want 1", n, err)
	}
	// Second remove returns 0.
	n, _ = RemoveAgentGroupOwner(g, "boss")
	if n != 0 {
		t.Errorf("second RemoveAgentGroupOwner = %d, want 0", n)
	}
}

// TestCanSenderReachTarget exercises the auth helper that drives
// `agent message`. Three interesting cases:
//
//   - shared membership: returns the shared group.
//   - owner-of-target's-group: returns the owned group.
//   - neither: returns nil.
func TestCanSenderReachTarget(t *testing.T) {
	setupTestDB(t)
	alpha, _ := CreateAgentGroup("alpha", "")
	beta, _ := CreateAgentGroup("beta", "")

	// alice and bob both in alpha; carl alone in beta; dave is owner
	// of beta but not a member.
	if err := AddAgentGroupMember(&AgentGroupMember{GroupID: alpha, ConvID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := AddAgentGroupMember(&AgentGroupMember{GroupID: alpha, ConvID: "bob"}); err != nil {
		t.Fatal(err)
	}
	if err := AddAgentGroupMember(&AgentGroupMember{GroupID: beta, ConvID: "carl"}); err != nil {
		t.Fatal(err)
	}
	if err := AddAgentGroupOwner(beta, "dave", ""); err != nil {
		t.Fatal(err)
	}

	// Shared-group: alice → bob picks alpha.
	via, reason, err := CanSenderReachTarget("alice", "bob")
	if err != nil || via == nil {
		t.Fatalf("alice→bob failed: %v %v %v", via, reason, err)
	}
	if via.Name != "alpha" || reason != "shared-group" {
		t.Errorf("alice→bob via=%q reason=%q, want alpha/shared-group", via.Name, reason)
	}

	// Owner-of: dave (no membership) → carl (member of beta).
	via, reason, err = CanSenderReachTarget("dave", "carl")
	if err != nil || via == nil {
		t.Fatalf("dave→carl failed: %v %v %v", via, reason, err)
	}
	if via.Name != "beta" || reason != "owner-of-group" {
		t.Errorf("dave→carl via=%q reason=%q, want beta/owner-of-group", via.Name, reason)
	}

	// Neither: alice (not in beta, doesn't own it) → carl.
	via, reason, err = CanSenderReachTarget("alice", "carl")
	if err != nil {
		t.Fatalf("alice→carl error: %v", err)
	}
	if via != nil {
		t.Errorf("alice→carl should fail, got via=%q reason=%q", via.Name, reason)
	}

	// Stranger → stranger: also nil.
	via, _, _ = CanSenderReachTarget("nobody", "nobody-else")
	if via != nil {
		t.Errorf("stranger→stranger should fail, got %+v", via)
	}
}

// TestAgentGroupOwnerCascadesOnGroupDelete: deleting a group removes
// its owner rows too (FK ON DELETE CASCADE).
func TestAgentGroupOwnerCascadesOnGroupDelete(t *testing.T) {
	setupTestDB(t)
	g, _ := CreateAgentGroup("alpha", "")
	if err := AddAgentGroupOwner(g, "boss", ""); err != nil {
		t.Fatal(err)
	}
	if err := DeleteAgentGroup("alpha"); err != nil {
		t.Fatalf("DeleteAgentGroup: %v", err)
	}
	owners, _ := ListGroupsOwnedBy("boss")
	if len(owners) != 0 {
		t.Errorf("owner rows should cascade-delete with the group, got %v", owners)
	}
}

// TestAgentMessageThreading checks the parent_id round-trip through
// insert / GetAgentMessage / list. parent_id = 0 stays 0 (top of
// thread), and a non-zero value is preserved on read.
func TestAgentMessageThreading(t *testing.T) {
	setupTestDB(t)

	g, _ := CreateAgentGroup("alpha", "")

	parentID, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "a", ToConv: "b", Body: "ping",
	})
	if err != nil {
		t.Fatalf("InsertAgentMessage parent: %v", err)
	}
	replyID, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "b", ToConv: "a", Body: "pong", ParentID: parentID,
	})
	if err != nil {
		t.Fatalf("InsertAgentMessage reply: %v", err)
	}

	parent, err := GetAgentMessage(parentID)
	if err != nil || parent == nil {
		t.Fatalf("GetAgentMessage parent: %v %+v", err, parent)
	}
	if parent.ParentID != 0 {
		t.Errorf("top-of-thread parent_id should be 0, got %d", parent.ParentID)
	}

	reply, err := GetAgentMessage(replyID)
	if err != nil || reply == nil {
		t.Fatalf("GetAgentMessage reply: %v %+v", err, reply)
	}
	if reply.ParentID != parentID {
		t.Errorf("reply.parent_id = %d, want %d", reply.ParentID, parentID)
	}

	// list endpoints should also surface parent_id.
	inbox, err := ListAgentMessagesForConv("a", 0)
	if err != nil {
		t.Fatalf("ListAgentMessagesForConv: %v", err)
	}
	var foundReply *AgentMessage
	for _, m := range inbox {
		if m.ID == replyID {
			foundReply = m
		}
	}
	if foundReply == nil {
		t.Fatalf("reply not in inbox")
	}
	if foundReply.ParentID != parentID {
		t.Errorf("inbox reply.parent_id = %d, want %d", foundReply.ParentID, parentID)
	}
}

// TestPruneAgentMessagesForConv exercises caller-scoping (only rows
// where the caller is from_conv or to_conv are deleted), the cutoff,
// and the --read-only filter.
func TestPruneAgentMessagesForConv(t *testing.T) {
	setupTestDB(t)

	g, _ := CreateAgentGroup("alpha", "")
	now := time.Now()

	// Helper: insert with explicit timestamps.
	insert := func(from, to string, createdAt time.Time, read bool) int64 {
		id, err := InsertAgentMessage(&AgentMessage{
			GroupID: g, FromConv: from, ToConv: to,
			Body: "x", CreatedAt: createdAt,
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if read {
			if err := MarkAgentMessageRead(id); err != nil {
				t.Fatalf("mark read: %v", err)
			}
		}
		return id
	}

	// Old + read in caller's pile (will be pruned by both modes).
	oldRead := insert("me", "peer", now.Add(-40*24*time.Hour), true)
	// Old + unread in caller's pile (pruned by default, kept in --read-only).
	oldUnread := insert("peer", "me", now.Add(-40*24*time.Hour), false)
	// Recent: never pruned.
	recent := insert("me", "peer", now.Add(-1*time.Hour), true)
	// Old but caller is not involved: must never be pruned.
	other := insert("x", "y", now.Add(-40*24*time.Hour), true)

	// --read-only mode: deletes only old + read in caller's pile.
	cutoff := now.Add(-30 * 24 * time.Hour)
	deleted, err := PruneAgentMessagesForConv("me", cutoff, true)
	if err != nil {
		t.Fatalf("prune readOnly: %v", err)
	}
	if deleted != 1 {
		t.Errorf("readOnly deleted = %d, want 1", deleted)
	}
	if got, _ := GetAgentMessage(oldRead); got != nil {
		t.Error("oldRead should have been deleted")
	}
	if got, _ := GetAgentMessage(oldUnread); got == nil {
		t.Error("oldUnread should have survived --read-only")
	}
	if got, _ := GetAgentMessage(recent); got == nil {
		t.Error("recent should not be touched")
	}
	if got, _ := GetAgentMessage(other); got == nil {
		t.Error("other-caller's message should never be touched")
	}

	// Default mode: deletes any old caller row.
	deleted, err = PruneAgentMessagesForConv("me", cutoff, false)
	if err != nil {
		t.Fatalf("prune all: %v", err)
	}
	if deleted != 1 {
		t.Errorf("all-mode deleted = %d, want 1", deleted)
	}
	if got, _ := GetAgentMessage(oldUnread); got != nil {
		t.Error("oldUnread should have been deleted in all-mode")
	}
	if got, _ := GetAgentMessage(other); got == nil {
		t.Error("other-caller's message must still be untouched")
	}
}

// TestListUndeliveredAgentMessagesFor covers the queue read used by
// the flush-on-online path: returns only undelivered messages addressed
// to the given conv, in oldest-first order.
func TestListUndeliveredAgentMessagesFor(t *testing.T) {
	setupTestDB(t)
	g, _ := CreateAgentGroup("alpha", "")

	// Create three messages addressed to "me", in known order.
	id1, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	id2, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "second",
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	id3, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "third",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Mark id2 delivered. Should be excluded.
	if err := MarkAgentMessageDelivered(id2); err != nil {
		t.Fatal(err)
	}

	// Add a message to a different conv. Should be excluded.
	if _, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "x", ToConv: "y", Body: "noise",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := ListUndeliveredAgentMessagesFor("me")
	if err != nil {
		t.Fatalf("ListUndeliveredAgentMessagesFor: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 undelivered, got %d", len(out))
	}
	if out[0].ID != id1 || out[1].ID != id3 {
		t.Errorf("expected oldest-first [%d, %d], got [%d, %d]",
			id1, id3, out[0].ID, out[1].ID)
	}

	// Empty toConv must short-circuit to nil (no SQL).
	out, err = ListUndeliveredAgentMessagesFor("")
	if err != nil || out != nil {
		t.Errorf("empty toConv: got %v, %v; want nil, nil", out, err)
	}
}

// TestClaimAgentMessageDelivery validates the race-safe claim
// primitive: first claim returns true, subsequent claims return
// false without error.
func TestClaimAgentMessageDelivery(t *testing.T) {
	setupTestDB(t)
	g, _ := CreateAgentGroup("alpha", "")

	id, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "queued",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := ClaimAgentMessageDelivery(id)
	if err != nil || !got {
		t.Fatalf("first claim: got=%v err=%v, want true/nil", got, err)
	}

	// Second claim must lose: the row is no longer delivered_at = ''.
	got, err = ClaimAgentMessageDelivery(id)
	if err != nil || got {
		t.Fatalf("second claim: got=%v err=%v, want false/nil", got, err)
	}

	// And delivered_at must be populated.
	m, err := GetAgentMessage(id)
	if err != nil || m == nil {
		t.Fatalf("GetAgentMessage: %v %+v", err, m)
	}
	if m.DeliveredAt.IsZero() {
		t.Errorf("delivered_at should be set after a successful claim")
	}

	// Claiming an already-delivered or unknown row must return false
	// (not error) so the flush goroutine can keep going.
	got, err = ClaimAgentMessageDelivery(99999)
	if err != nil || got {
		t.Errorf("nonexistent id: got=%v err=%v, want false/nil", got, err)
	}
}

// TestListAgentMessagesFromConv covers the outbox direction: rows
// from a given sender, most recent first.
func TestListAgentMessagesFromConv(t *testing.T) {
	setupTestDB(t)
	g, _ := CreateAgentGroup("alpha", "")

	if _, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "me", ToConv: "peer", Body: "first",
	}); err != nil {
		t.Fatal(err)
	}
	// Sleep a tick so created_at differs and ORDER BY is meaningful.
	time.Sleep(2 * time.Millisecond)
	if _, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "me", ToConv: "peer", Body: "second",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "other", ToConv: "peer", Body: "noise",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := ListAgentMessagesFromConv("me", 0)
	if err != nil {
		t.Fatalf("ListAgentMessagesFromConv: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 outgoing rows, got %d", len(out))
	}
	if out[0].Body != "second" || out[1].Body != "first" {
		t.Errorf("expected most-recent-first, got %q then %q", out[0].Body, out[1].Body)
	}
}
