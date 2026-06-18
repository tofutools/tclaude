package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentGroupCRUD(t *testing.T) {
	setupTestDB(t)

	id, err := CreateAgentGroup("alpha", "test group")
	require.NoError(t, err, "CreateAgentGroup")
	require.NotZero(t, id, "expected non-zero group id")

	g, err := GetAgentGroupByName("alpha")
	require.NoError(t, err, "GetAgentGroupByName")
	require.NotNil(t, g, "unexpected group: nil")
	assert.Equal(t, "alpha", g.Name, "unexpected group: %+v", g)
	assert.Equal(t, "test group", g.Descr, "unexpected group: %+v", g)

	// Duplicate names should fail at the UNIQUE constraint.
	_, err = CreateAgentGroup("alpha", "")
	require.Error(t, err, "expected error creating duplicate group")

	groups, err := ListAgentGroups()
	require.NoError(t, err, "ListAgentGroups")
	require.Len(t, groups, 1, "ListAgentGroups returned %+v", groups)
	assert.Equal(t, "alpha", groups[0].Name)

	// Delete with no members or messages: ok.
	require.NoError(t, DeleteAgentGroup("alpha"), "DeleteAgentGroup")
	g, _ = GetAgentGroupByName("alpha")
	assert.Nil(t, g, "expected group to be gone")
}

func TestAgentGroupMembershipAndShared(t *testing.T) {
	setupTestDB(t)

	a, _ := CreateAgentGroup("alpha", "")
	b, _ := CreateAgentGroup("beta", "")

	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{
		GroupID: a, ConvID: "conv-1", Role: "lead",
	}), "AddAgentGroupMember")
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{
		GroupID: a, ConvID: "conv-2", Role: "reviewer",
	}), "AddAgentGroupMember")
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{
		GroupID: b, ConvID: "conv-2",
	}), "AddAgentGroupMember")
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{
		GroupID: b, ConvID: "conv-3",
	}), "AddAgentGroupMember")

	// conv-1 and conv-2 share alpha; conv-2 and conv-3 share beta.
	shared, err := SharedGroupsForConvs("conv-1", "conv-2")
	require.NoError(t, err, "SharedGroupsForConvs")
	require.Len(t, shared, 1, "expected [alpha], got %+v", names(shared))
	assert.Equal(t, "alpha", shared[0].Name)

	shared, err = SharedGroupsForConvs("conv-1", "conv-3")
	require.NoError(t, err, "SharedGroupsForConvs")
	require.Len(t, shared, 0, "expected no shared groups for conv-1/conv-3, got %+v", names(shared))

	// ListGroupsForConv returns all groups for conv-2.
	gs, err := ListGroupsForConv("conv-2")
	require.NoError(t, err, "ListGroupsForConv")
	require.Len(t, gs, 2, "expected 2 groups for conv-2")

	// Remove conv-2 from beta and the shared set with conv-3 should empty.
	require.NoError(t, RemoveAgentGroupMember(b, "conv-2"), "RemoveAgentGroupMember")
	shared, _ = SharedGroupsForConvs("conv-2", "conv-3")
	require.Len(t, shared, 0, "expected no shared groups after remove, got %+v", names(shared))
}

func TestAgentPermissions_GrantRevokeIdempotent(t *testing.T) {
	setupTestDB(t)

	conv := "abcd1234-0000-0000-0000-000000000001"

	// Empty initially.
	perms, err := ListAgentPermissionsForConv(conv)
	require.NoError(t, err, "expected empty list")
	require.Len(t, perms, 0, "expected empty list, got %v", perms)
	ok, err := HasAgentPermissionRow(conv, "self.rename")
	require.NoError(t, err, "expected no perm")
	require.False(t, ok, "expected no perm")

	// Grant.
	require.NoError(t, GrantAgentPermission(conv, "self.rename", "<human>"), "GrantAgentPermission")
	// Idempotent.
	require.NoError(t, GrantAgentPermission(conv, "self.rename", "<human>"), "idempotent grant")
	ok, err = HasAgentPermissionRow(conv, "self.rename")
	require.NoError(t, err, "expected perm")
	require.True(t, ok, "expected perm")

	// Multiple slugs sort correctly.
	require.NoError(t, GrantAgentPermission(conv, "member.add", ""))
	perms, err = ListAgentPermissionsForConv(conv)
	require.NoError(t, err)
	require.Len(t, perms, 2, "expected sorted list [member.add self.rename], got %v", perms)
	assert.Equal(t, "member.add", perms[0])
	assert.Equal(t, "self.rename", perms[1])

	// Revoke.
	n, err := RevokeAgentPermission(conv, "self.rename")
	require.NoError(t, err, "RevokeAgentPermission")
	require.Equal(t, int64(1), n, "RevokeAgentPermission n")
	// Idempotent revoke returns 0.
	n, err = RevokeAgentPermission(conv, "self.rename")
	require.NoError(t, err, "idempotent revoke")
	require.Equal(t, int64(0), n, "idempotent revoke n")

	// ListAllAgentPermissions sees the remaining slug.
	all, err := ListAllAgentPermissions()
	require.NoError(t, err)
	got := all[conv]
	require.Len(t, got, 1, "expected [member.add], got %v", got)
	assert.Equal(t, "member.add", got[0])
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
	require.NoError(t, err, "InsertAgentMessage")
	id2, err := InsertAgentMessage(&AgentMessage{
		GroupID:  g,
		FromConv: "conv-1",
		ToConv:   "conv-2",
		Body:     "second",
	})
	require.NoError(t, err, "InsertAgentMessage")

	msgs, err := ListAgentMessagesForConv("conv-2", 0)
	require.NoError(t, err, "ListAgentMessagesForConv")
	require.Len(t, msgs, 2, "expected 2 messages")

	got, err := GetAgentMessage(id1)
	require.NoError(t, err, "GetAgentMessage(%d)", id1)
	require.NotNil(t, got, "GetAgentMessage(%d)", id1)
	assert.Equal(t, "hello", got.Subject, "unexpected message contents: %+v", got)
	assert.Equal(t, "first", got.Body, "unexpected message contents: %+v", got)

	require.NoError(t, MarkAgentMessageDelivered(id1), "MarkAgentMessageDelivered")
	require.NoError(t, MarkAgentMessageRead(id1), "MarkAgentMessageRead")
	got, _ = GetAgentMessage(id1)
	assert.False(t, got.DeliveredAt.IsZero(), "delivered_at should be set")
	assert.False(t, got.ReadAt.IsZero(), "read_at should be set")

	// Deleting a group cascades through membership/ownership but
	// PRESERVES its message history: the rows survive, rewritten to
	// group_id 0 (direct messages). agent_messages no longer has a
	// foreign key to agent_groups — see the universal-inbox change and
	// TestDeleteAgentGroup_PreservesMessagesAsDirect.
	require.NoError(t, DeleteAgentGroup("alpha"), "DeleteAgentGroup with messages still present")
	for _, mid := range []int64{id1, id2} {
		got, err := GetAgentMessage(mid)
		require.NoError(t, err, "GetAgentMessage(%d) after delete", mid)
		require.NotNil(t, got, "message %d should survive its group's deletion", mid)
		assert.Equal(t, int64(0), got.GroupID,
			"a deleted group's messages become direct (group_id 0)")
	}
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

	require.NoError(t, AddAgentGroupOwner(g, "boss", ""), "AddAgentGroupOwner")
	// Idempotent — second call must not error.
	require.NoError(t, AddAgentGroupOwner(g, "boss", ""), "AddAgentGroupOwner second")

	is, err := IsAgentGroupOwner(g, "boss")
	require.NoError(t, err, "IsAgentGroupOwner(g, boss)")
	require.True(t, is, "IsAgentGroupOwner(g, boss)")
	is, _ = IsAgentGroupOwner(g, "stranger")
	assert.False(t, is, "IsAgentGroupOwner(stranger) should be false")

	owners, err := ListAgentGroupOwners(g)
	require.NoError(t, err, "ListAgentGroupOwners")
	require.Len(t, owners, 1, "ListAgentGroupOwners")
	assert.Equal(t, "boss", owners[0].ConvID, "ListAgentGroupOwners")

	groups, err := ListGroupsOwnedBy("boss")
	require.NoError(t, err, "ListGroupsOwnedBy")
	require.Len(t, groups, 1, "ListGroupsOwnedBy")
	assert.Equal(t, g, groups[0], "ListGroupsOwnedBy")

	n, err := RemoveAgentGroupOwner(g, "boss")
	require.NoError(t, err, "RemoveAgentGroupOwner")
	require.Equal(t, int64(1), n, "RemoveAgentGroupOwner")
	// Second remove returns 0.
	n, _ = RemoveAgentGroupOwner(g, "boss")
	assert.Equal(t, int64(0), n, "second RemoveAgentGroupOwner")
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
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: alpha, ConvID: "alice"}))
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: alpha, ConvID: "bob"}))
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: beta, ConvID: "carl"}))
	require.NoError(t, AddAgentGroupOwner(beta, "dave", ""))

	// Shared-group: alice → bob picks alpha.
	via, reason, err := CanSenderReachTarget("alice", "bob")
	require.NoError(t, err, "alice→bob err")
	require.NotNil(t, via, "alice→bob failed: reason=%v", reason)
	assert.Equal(t, "alpha", via.Name, "alice→bob via")
	assert.Equal(t, "shared-group", reason, "alice→bob reason")

	// Owner-of: dave (no membership) → carl (member of beta).
	via, reason, err = CanSenderReachTarget("dave", "carl")
	require.NoError(t, err, "dave→carl err")
	require.NotNil(t, via, "dave→carl failed: reason=%v", reason)
	assert.Equal(t, "beta", via.Name, "dave→carl via")
	assert.Equal(t, "owner-of-group", reason, "dave→carl reason")

	// Neither: alice (not in beta, doesn't own it) → carl.
	via, reason, err = CanSenderReachTarget("alice", "carl")
	require.NoError(t, err, "alice→carl error")
	assert.Nil(t, via, "alice→carl should fail, got via reason=%q", reason)

	// Stranger → stranger: also nil.
	via, _, _ = CanSenderReachTarget("nobody", "nobody-else")
	assert.Nil(t, via, "stranger→stranger should fail, got %+v", via)
}

// TestAgentGroupOwner_AutoOwnAfterCreate: documents the daemon's
// auto-own behavior at the DB primitive level. The handler calls
// CreateAgentGroup followed by AddAgentGroupOwner with the creator's
// conv-id; this test confirms the same sequence yields a group whose
// creator can immediately reach members it adds.
func TestAgentGroupOwner_AutoOwnAfterCreate(t *testing.T) {
	setupTestDB(t)
	creator := "ab887fe0"

	id, err := CreateAgentGroup("auto-own-team", "")
	require.NoError(t, err, "CreateAgentGroup")
	require.NoError(t, AddAgentGroupOwner(id, creator, creator), "AddAgentGroupOwner")
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: id, ConvID: "peer"}), "AddAgentGroupMember")

	via, reason, err := CanSenderReachTarget(creator, "peer")
	require.NoError(t, err, "creator→peer err")
	require.NotNil(t, via, "creator→peer should reach via owner-of-group: reason=%q", reason)
	assert.Equal(t, "owner-of-group", reason, "reason")
}

// TestAgentGroupOwnerCascadesOnGroupDelete: deleting a group removes
// its owner rows too (FK ON DELETE CASCADE).
func TestAgentGroupOwnerCascadesOnGroupDelete(t *testing.T) {
	setupTestDB(t)
	g, _ := CreateAgentGroup("alpha", "")
	require.NoError(t, AddAgentGroupOwner(g, "boss", ""))
	require.NoError(t, DeleteAgentGroup("alpha"), "DeleteAgentGroup")
	owners, _ := ListGroupsOwnedBy("boss")
	assert.Len(t, owners, 0, "owner rows should cascade-delete with the group, got %v", owners)
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
	require.NoError(t, err, "InsertAgentMessage parent")
	replyID, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "b", ToConv: "a", Body: "pong", ParentID: parentID,
	})
	require.NoError(t, err, "InsertAgentMessage reply")

	parent, err := GetAgentMessage(parentID)
	require.NoError(t, err, "GetAgentMessage parent")
	require.NotNil(t, parent, "GetAgentMessage parent")
	assert.Equal(t, int64(0), parent.ParentID, "top-of-thread parent_id should be 0")

	reply, err := GetAgentMessage(replyID)
	require.NoError(t, err, "GetAgentMessage reply")
	require.NotNil(t, reply, "GetAgentMessage reply")
	assert.Equal(t, parentID, reply.ParentID, "reply.parent_id")

	// list endpoints should also surface parent_id.
	inbox, err := ListAgentMessagesForConv("a", 0)
	require.NoError(t, err, "ListAgentMessagesForConv")
	var foundReply *AgentMessage
	for _, m := range inbox {
		if m.ID == replyID {
			foundReply = m
		}
	}
	require.NotNil(t, foundReply, "reply not in inbox")
	assert.Equal(t, parentID, foundReply.ParentID, "inbox reply.parent_id")
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
		require.NoError(t, err, "insert")
		if read {
			require.NoError(t, MarkAgentMessageRead(id), "mark read")
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
	require.NoError(t, err, "prune readOnly")
	assert.Equal(t, int64(1), deleted, "readOnly deleted")
	got, _ := GetAgentMessage(oldRead)
	assert.Nil(t, got, "oldRead should have been deleted")
	got, _ = GetAgentMessage(oldUnread)
	assert.NotNil(t, got, "oldUnread should have survived --read-only")
	got, _ = GetAgentMessage(recent)
	assert.NotNil(t, got, "recent should not be touched")
	got, _ = GetAgentMessage(other)
	assert.NotNil(t, got, "other-caller's message should never be touched")

	// Default mode: deletes any old caller row.
	deleted, err = PruneAgentMessagesForConv("me", cutoff, false)
	require.NoError(t, err, "prune all")
	assert.Equal(t, int64(1), deleted, "all-mode deleted")
	got, _ = GetAgentMessage(oldUnread)
	assert.Nil(t, got, "oldUnread should have been deleted in all-mode")
	got, _ = GetAgentMessage(other)
	assert.NotNil(t, got, "other-caller's message must still be untouched")
}

// TestListUndeliveredAgentMessagesFor covers the queue read used by
// the flush-on-online path: returns only undelivered messages addressed
// to the given conv, in oldest-first order.
func TestListUndeliveredAgentMessagesFor(t *testing.T) {
	setupTestDB(t)
	g, _ := CreateAgentGroup("alpha", "")

	// Create three messages addressed to "me", in known order.
	// No sleeps between inserts: ListUndeliveredAgentMessagesFor orders by id
	// (insertion order), so the oldest-first order holds regardless of how the
	// created_at timestamps land (the #242 fix this test guards).
	id1, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "first",
	})
	require.NoError(t, err)
	id2, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "second",
	})
	require.NoError(t, err)
	id3, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "third",
	})
	require.NoError(t, err)

	// Mark id2 delivered. Should be excluded.
	require.NoError(t, MarkAgentMessageDelivered(id2))

	// Add a message to a different conv. Should be excluded.
	_, err = InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "x", ToConv: "y", Body: "noise",
	})
	require.NoError(t, err)

	out, err := ListUndeliveredAgentMessagesFor("me")
	require.NoError(t, err, "ListUndeliveredAgentMessagesFor")
	require.Len(t, out, 2, "expected 2 undelivered")
	assert.Equal(t, id1, out[0].ID, "expected oldest-first")
	assert.Equal(t, id3, out[1].ID, "expected oldest-first")

	// Empty toConv must short-circuit to nil (no SQL).
	out, err = ListUndeliveredAgentMessagesFor("")
	assert.NoError(t, err, "empty toConv err")
	assert.Nil(t, out, "empty toConv out")
}

// TestListUndeliveredAgentMessagesForWholeSecondOrdering locks in oldest-first
// ordering across the RFC3339Nano whole-second boundary — the macOS-CI flake
// behind TestListUndeliveredAgentMessagesFor.
//
// created_at is stored as an RFC3339Nano string. A time on an exact second
// serialises as "…:00Z" (no fraction) and, compared as text, sorts AFTER a
// later same-second value "…:00.004Z" because '.' < 'Z'. So under
// ORDER BY created_at the older message (id "older") comes back AFTER the newer
// one. Ordering by id (insertion order) is correct and total. This fails
// deterministically against the old query and passes with the fix.
func TestListUndeliveredAgentMessagesForWholeSecondOrdering(t *testing.T) {
	setupTestDB(t)
	g, _ := CreateAgentGroup("alpha", "")

	// older lands exactly on a second; newer is 4ms later in the SAME second.
	whole := time.Date(2026, 5, 30, 14, 29, 0, 0, time.UTC)
	older, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "older", CreatedAt: whole,
	})
	require.NoError(t, err)
	newer, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "newer",
		CreatedAt: whole.Add(4 * time.Millisecond),
	})
	require.NoError(t, err)

	out, err := ListUndeliveredAgentMessagesFor("me")
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, older, out[0].ID, "oldest (whole-second) message must come first")
	assert.Equal(t, newer, out[1].ID, "newer (fractional) message must come second")
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
	require.NoError(t, err)

	got, err := ClaimAgentMessageDelivery(id)
	require.NoError(t, err, "first claim err")
	require.True(t, got, "first claim: want true")

	// Second claim must lose: the row is no longer delivered_at = ''.
	got, err = ClaimAgentMessageDelivery(id)
	require.NoError(t, err, "second claim err")
	require.False(t, got, "second claim: want false")

	// And delivered_at must be populated.
	m, err := GetAgentMessage(id)
	require.NoError(t, err, "GetAgentMessage")
	require.NotNil(t, m, "GetAgentMessage")
	assert.False(t, m.DeliveredAt.IsZero(), "delivered_at should be set after a successful claim")

	// Claiming an already-delivered or unknown row must return false
	// (not error) so the flush goroutine can keep going.
	got, err = ClaimAgentMessageDelivery(99999)
	assert.NoError(t, err, "nonexistent id err")
	assert.False(t, got, "nonexistent id got")
}

// TestListAgentMessagesFromConv covers the outbox direction: rows
// from a given sender, most recent first.
func TestListAgentMessagesFromConv(t *testing.T) {
	setupTestDB(t)
	g, _ := CreateAgentGroup("alpha", "")

	_, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "me", ToConv: "peer", Body: "first",
	})
	require.NoError(t, err)
	// No sleep needed: ListAgentMessagesFromConv orders by id (insertion
	// order), so "second" sorts after "first" regardless of wall-clock spacing.
	_, err = InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "me", ToConv: "peer", Body: "second",
	})
	require.NoError(t, err)
	_, err = InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "other", ToConv: "peer", Body: "noise",
	})
	require.NoError(t, err)

	out, err := ListAgentMessagesFromConv("me", 0)
	require.NoError(t, err, "ListAgentMessagesFromConv")
	require.Len(t, out, 2, "expected 2 outgoing rows")
	assert.Equal(t, "second", out[0].Body, "expected most-recent-first")
	assert.Equal(t, "first", out[1].Body, "expected most-recent-first")
}

// TestListAgentMessages_WholeSecondBoundaryOrdering is the deterministic
// regression guard for the #242-class flake on the inbox/outbox path: a
// message stamped exactly on a whole second ("…:00Z") versus one a few ms
// later ("…:00.004Z"). As RFC3339Nano text, the whole-second value sorts
// AFTER the fractional one ('.' < 'Z'), so an ORDER BY created_at query would
// return the OLDER message as "most recent" — and drop the newer one under
// LIMIT. Ordering by id (insertion order) returns them correctly newest-first.
// This test fails on the pre-fix `ORDER BY created_at DESC` and passes on id.
func TestListAgentMessages_WholeSecondBoundaryOrdering(t *testing.T) {
	setupTestDB(t)
	g, _ := CreateAgentGroup("alpha", "")

	// base lands on a whole second → RFC3339Nano renders it with no fractional
	// part ("…:00Z"); the newer one renders as "…:00.004Z".
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "me", ToConv: "peer", Body: "older", CreatedAt: base,
	})
	require.NoError(t, err)
	_, err = InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "me", ToConv: "peer", Body: "newer", CreatedAt: base.Add(4 * time.Millisecond),
	})
	require.NoError(t, err)

	out, err := ListAgentMessagesFromConv("me", 0)
	require.NoError(t, err, "ListAgentMessagesFromConv")
	require.Len(t, out, 2, "expected 2 outgoing rows")
	assert.Equal(t, "newer", out[0].Body, "newest message must come first despite its created_at sorting lexically before the whole-second one")
	assert.Equal(t, "older", out[1].Body, "older message must come second")

	// Under LIMIT 1 the single row must be the genuinely-newest, not the older
	// one a created_at sort would surface at the boundary.
	limited, err := ListAgentMessagesFromConv("me", 1)
	require.NoError(t, err, "ListAgentMessagesFromConv limit 1")
	require.Len(t, limited, 1, "expected 1 row under LIMIT 1")
	assert.Equal(t, "newer", limited[0].Body, "LIMIT 1 must return the newest message")
}

// TestDeleteAgentByConvID_PurgesAllReferencingTables guards the
// "no leftover refs" promise of `tclaude agent delete`. Seeds a
// conv with rows in every table that holds a conv-id, then asserts
// the single transaction wipes them all and returns accurate
// counts. Idempotent re-run on the gone conv should be a no-op.
func TestDeleteAgentByConvID_PurgesAllReferencingTables(t *testing.T) {
	setupTestDB(t)
	const target = "victim-conv-1234"
	const peer = "peer-conv-5678"

	g, _ := CreateAgentGroup("alpha", "")

	// Membership + ownership.
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{
		GroupID: g, ConvID: target,
	}))
	require.NoError(t, AddAgentGroupOwner(g, target, ""))
	// A peer member so messages can route through the group.
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{
		GroupID: g, ConvID: peer,
	}))

	// Messages: one outgoing (target -> peer), one incoming (peer -> target).
	_, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: target, ToConv: peer, Body: "out",
	})
	require.NoError(t, err)
	_, err = InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: peer, ToConv: target, Body: "in",
	})
	require.NoError(t, err)

	// Permission grant.
	require.NoError(t, GrantAgentPermission(target, "self.rename", ""))

	// Sessions row.
	require.NoError(t, SaveSession(&SessionRow{
		ID: "label-victim", TmuxSession: "tmux-victim", ConvID: target,
	}))

	// Conv index row.
	require.NoError(t, UpsertConvIndex(&ConvIndexRow{
		ConvID: target, CustomTitle: "victim-title",
	}))

	// Succession trail (target was once the OLD side of a reincarnation).
	require.NoError(t, RecordConvSuccession(target, "newer-conv", "reincarnate"))

	counts, err := DeleteAgentByConvID(target)
	require.NoError(t, err, "DeleteAgentByConvID")

	// Spot-check the counts. Every populated table should be > 0; the
	// untouched ones (cron, embeddings, succession-new) stay at 0.
	assert.Equal(t, int64(1), counts.GroupMembers, "GroupMembers")
	assert.Equal(t, int64(1), counts.GroupOwners, "GroupOwners")
	assert.Equal(t, int64(1), counts.MessagesFrom, "MessagesFrom")
	assert.Equal(t, int64(1), counts.MessagesTo, "MessagesTo")
	assert.Equal(t, int64(1), counts.Permissions, "Permissions")
	assert.Equal(t, int64(1), counts.Sessions, "Sessions")
	assert.Equal(t, int64(1), counts.ConvIndex, "ConvIndex")
	assert.Equal(t, int64(1), counts.SuccessionOld, "SuccessionOld")

	// Tables are actually empty for this conv now.
	gotMsgs, _ := ListAgentMessagesForConv(target, 0)
	assert.Len(t, gotMsgs, 0, "messages-to remain after delete")
	gotFrom, _ := ListAgentMessagesFromConv(target, 0)
	assert.Len(t, gotFrom, 0, "messages-from remain after delete")
	row, _ := GetConvIndex(target)
	assert.Nil(t, row, "conv_index row remains: %+v", row)
	rows, _ := FindSessionsByConvID(target)
	assert.Len(t, rows, 0, "session rows remain")
	gotPerms, _ := ListAgentPermissionsForConv(target)
	assert.Len(t, gotPerms, 0, "permissions remain")

	// Peer's untouched: their inbox / outbox should still hold their
	// half of the (now-deleted) thread? No — the thread rows had
	// from_conv=target OR to_conv=target, so both got removed. The
	// peer's standalone presence (membership) is unaffected.
	peerMember, err := FindMemberInGroup(g, peer)
	require.NoError(t, err, "peer membership should survive")
	assert.NotNil(t, peerMember, "peer membership should survive")

	// Idempotent re-run: every count is zero, no error.
	again, err := DeleteAgentByConvID(target)
	assert.NoError(t, err, "idempotent re-delete errored")
	assert.Equal(t, AgentDeletionCounts{}, again, "idempotent re-delete returned non-zero counts")
}

// TestAgentMessageRecipientsRoundTrip verifies the email-style
// audience arrays (schema v18 to_recipients / cc_recipients) survive
// Insert -> Get and Insert -> List read paths. Empty arrays decode
// back as nil; populated arrays preserve order.
func TestAgentMessageRecipientsRoundTrip(t *testing.T) {
	setupTestDB(t)
	g, _ := CreateAgentGroup("alpha", "")

	// Multi-recipient: To=[primary], CC=[c1, c2]. Three rows would
	// normally be inserted (one per recipient); here we just round-trip
	// the audience on one row to keep the test focused.
	id, err := InsertAgentMessage(&AgentMessage{
		GroupID:      g,
		FromConv:     "sender",
		ToConv:       "primary",
		Body:         "hi all",
		ToRecipients: []string{"primary"},
		CcRecipients: []string{"c1", "c2"},
	})
	require.NoError(t, err, "Insert")

	got, err := GetAgentMessage(id)
	require.NoError(t, err, "GetAgentMessage")
	require.NotNil(t, got, "GetAgentMessage")
	require.Len(t, got.ToRecipients, 1, "ToRecipients = %v, want [primary]", got.ToRecipients)
	assert.Equal(t, "primary", got.ToRecipients[0], "ToRecipients")
	require.Len(t, got.CcRecipients, 2, "CcRecipients = %v, want [c1 c2]", got.CcRecipients)
	assert.Equal(t, "c1", got.CcRecipients[0], "CcRecipients[0]")
	assert.Equal(t, "c2", got.CcRecipients[1], "CcRecipients[1]")

	// List path scans the same columns; check it picks them up too.
	msgs, err := ListAgentMessagesForConv("primary", 0)
	require.NoError(t, err, "ListAgentMessagesForConv")
	require.Len(t, msgs, 1, "expected 1 inbox row")
	assert.Len(t, msgs[0].CcRecipients, 2, "list CcRecipients lost: %v", msgs[0].CcRecipients)

	// Legacy single-recipient (no audience): both arrays decode to nil.
	idLegacy, err := InsertAgentMessage(&AgentMessage{
		GroupID:  g,
		FromConv: "sender",
		ToConv:   "lone",
		Body:     "old shape",
	})
	require.NoError(t, err, "Insert legacy")
	legacy, err := GetAgentMessage(idLegacy)
	require.NoError(t, err, "GetAgentMessage legacy")
	require.NotNil(t, legacy, "GetAgentMessage legacy")
	assert.Nil(t, legacy.ToRecipients, "legacy row should decode to nil arrays, got to=%v", legacy.ToRecipients)
	assert.Nil(t, legacy.CcRecipients, "legacy row should decode to nil arrays, got cc=%v", legacy.CcRecipients)
}
