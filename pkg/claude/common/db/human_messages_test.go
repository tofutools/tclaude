package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHumanMessages_InsertListCount(t *testing.T) {
	setupTestDB(t)

	msgs, err := ListHumanMessagesPage(HumanMessageFilter{}, 0, MaxHumanMessagesPage)
	require.NoError(t, err)
	assert.Empty(t, msgs, "no messages to start")
	n, err := CountUnreadHumanMessages()
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	// Two messages; the second is the newer.
	id1, err := InsertHumanMessage(&HumanMessage{
		FromConv: "conv-a", FromTitle: "tclaude-PO", GroupName: "dev",
		Subject: "first", Body: "body one",
		CreatedAt: time.Now().Add(-time.Minute),
	})
	require.NoError(t, err)
	id2, err := InsertHumanMessage(&HumanMessage{
		FromConv: "conv-b", FromTitle: "tclaude-lead", Body: "body two",
		CreatedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NotEqual(t, id1, id2)

	msgs, err = ListHumanMessagesPage(HumanMessageFilter{}, 0, MaxHumanMessagesPage)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	// Newest first.
	assert.Equal(t, id2, msgs[0].ID)
	assert.Equal(t, "tclaude-lead", msgs[0].FromTitle)
	assert.Equal(t, id1, msgs[1].ID)
	assert.Equal(t, "first", msgs[1].Subject)
	assert.Equal(t, "dev", msgs[1].GroupName)
	assert.False(t, msgs[0].IsRead(), "a fresh message is unread")

	n, err = CountUnreadHumanMessages()
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

// TestHumanMessages_WholeSecondBoundaryOrdering is the deterministic
// regression guard for the #242-class flake on the human-notifications list:
// a message stamped exactly on a whole second ("…:00Z") versus one a few ms
// later ("…:00.004Z"). As RFC3339Nano text, the whole-second value sorts AFTER
// the fractional one ('.' < 'Z'), so an ORDER BY created_at query would render
// the OLDER message as "newest". A `, id DESC` tiebreak does not save it — the
// rows have different created_at strings, so the tiebreak never engages.
// Ordering by id (insertion order) returns them correctly newest-first.
// This test fails on the pre-fix `ORDER BY created_at DESC, id DESC` and passes
// on `ORDER BY id DESC`.
func TestHumanMessages_WholeSecondBoundaryOrdering(t *testing.T) {
	setupTestDB(t)

	// base lands on a whole second → RFC3339Nano renders it with no fractional
	// part ("…:00Z"); the newer one renders as "…:00.004Z".
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	olderID, err := InsertHumanMessage(&HumanMessage{
		FromConv: "c", Body: "older", CreatedAt: base,
	})
	require.NoError(t, err)
	newerID, err := InsertHumanMessage(&HumanMessage{
		FromConv: "c", Body: "newer", CreatedAt: base.Add(4 * time.Millisecond),
	})
	require.NoError(t, err)

	msgs, err := ListHumanMessagesPage(HumanMessageFilter{}, 0, MaxHumanMessagesPage)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, newerID, msgs[0].ID, "newer (fractional) message must come first")
	assert.Equal(t, olderID, msgs[1].ID, "older (whole-second) message must come second")
}

func TestHumanMessages_Get(t *testing.T) {
	setupTestDB(t)

	// A miss is (nil, nil) — the caller distinguishes not-found from error.
	got, err := GetHumanMessage(12345)
	require.NoError(t, err)
	assert.Nil(t, got, "no row → nil, nil")

	id, err := InsertHumanMessage(&HumanMessage{
		FromConv: "conv-a", FromTitle: "tclaude-worker", GroupName: "dev",
		Subject: "need a decision", Body: "which option?",
		CreatedAt: time.Now(),
	})
	require.NoError(t, err)

	got, err = GetHumanMessage(id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "conv-a", got.FromConv)
	assert.Equal(t, "tclaude-worker", got.FromTitle)
	assert.Equal(t, "dev", got.GroupName)
	assert.Equal(t, "need a decision", got.Subject)
	assert.Equal(t, "which option?", got.Body)
	assert.False(t, got.IsRead(), "a fresh message is unread")
}

func TestHumanMessages_MarkRead(t *testing.T) {
	setupTestDB(t)
	id, err := InsertHumanMessage(&HumanMessage{FromConv: "c", Body: "x"})
	require.NoError(t, err)

	changed, err := MarkHumanMessageRead(id)
	require.NoError(t, err)
	assert.True(t, changed, "first mark transitions the row")

	msgs, _ := ListHumanMessagesPage(HumanMessageFilter{}, 0, MaxHumanMessagesPage)
	require.Len(t, msgs, 1)
	firstReadAt := msgs[0].ReadAt
	require.False(t, firstReadAt.IsZero(), "read_at is stamped")

	// Idempotent: re-marking is a no-op and leaves the timestamp stable.
	changed, err = MarkHumanMessageRead(id)
	require.NoError(t, err)
	assert.False(t, changed, "re-marking an already-read message is a no-op")
	msgs, _ = ListHumanMessagesPage(HumanMessageFilter{}, 0, MaxHumanMessagesPage)
	assert.Equal(t, firstReadAt, msgs[0].ReadAt, "read timestamp stays stable")

	// A non-existent id is a no-op, not an error.
	changed, err = MarkHumanMessageRead(999999)
	require.NoError(t, err)
	assert.False(t, changed)

	n, _ := CountUnreadHumanMessages()
	assert.Equal(t, 0, n)
}

func TestHumanMessages_MarkUnread(t *testing.T) {
	setupTestDB(t)
	id, err := InsertHumanMessage(&HumanMessage{FromConv: "c", Body: "x"})
	require.NoError(t, err)

	// Marking an unread message unread is a no-op (nothing to transition).
	changed, err := MarkHumanMessageUnread(id)
	require.NoError(t, err)
	assert.False(t, changed, "an already-unread message doesn't transition")

	// Read it, then mark it unread — the read→unread opt-out.
	_, err = MarkHumanMessageRead(id)
	require.NoError(t, err)
	n, _ := CountUnreadHumanMessages()
	require.Equal(t, 0, n)

	changed, err = MarkHumanMessageUnread(id)
	require.NoError(t, err)
	assert.True(t, changed, "a read message transitions back to unread")
	msgs, _ := ListHumanMessagesPage(HumanMessageFilter{}, 0, MaxHumanMessagesPage)
	require.Len(t, msgs, 1)
	assert.False(t, msgs[0].IsRead(), "read_at is cleared")
	n, _ = CountUnreadHumanMessages()
	assert.Equal(t, 1, n, "it counts as unread again")

	// Idempotent: re-marking the now-unread message is a no-op.
	changed, err = MarkHumanMessageUnread(id)
	require.NoError(t, err)
	assert.False(t, changed)

	// A non-existent id is a no-op, not an error.
	changed, err = MarkHumanMessageUnread(999999)
	require.NoError(t, err)
	assert.False(t, changed)
}

func TestHumanMessages_MarkAllReadAndClear(t *testing.T) {
	setupTestDB(t)
	for range 3 {
		_, err := InsertHumanMessage(&HumanMessage{FromConv: "c", Body: "x"})
		require.NoError(t, err)
	}
	marked, err := MarkAllHumanMessagesRead()
	require.NoError(t, err)
	assert.Equal(t, 3, marked)
	n, _ := CountUnreadHumanMessages()
	assert.Equal(t, 0, n)

	// A fresh unread message arrives after the mark-all.
	_, err = InsertHumanMessage(&HumanMessage{FromConv: "c", Body: "fresh"})
	require.NoError(t, err)

	// A decision read but not answered is still waiting on the operator.
	undecided, err := InsertHumanMessage(&HumanMessage{FromConv: "c", Body: "which?", Options: []string{"a", "b"}})
	require.NoError(t, err)
	_, err = MarkHumanMessageRead(undecided)
	require.NoError(t, err)

	// Clear deletes only the read messages; the fresh unread survives.
	deleted, err := DeleteReadHumanMessages()
	require.NoError(t, err)
	assert.Equal(t, 3, deleted)
	msgs, _ := ListHumanMessagesPage(HumanMessageFilter{}, 0, MaxHumanMessagesPage)
	require.Len(t, msgs, 2)
	assert.ElementsMatch(t, []string{"fresh", "which?"}, []string{msgs[0].Body, msgs[1].Body})
}

func TestHumanMessages_DeleteOne(t *testing.T) {
	setupTestDB(t)

	// An unread message and a read message — per-message delete ignores
	// read state, unlike the bulk "clear read" sweep.
	unreadID, err := InsertHumanMessage(&HumanMessage{FromConv: "c", Body: "unread"})
	require.NoError(t, err)
	readID, err := InsertHumanMessage(&HumanMessage{FromConv: "c", Body: "read"})
	require.NoError(t, err)
	_, err = MarkHumanMessageRead(readID)
	require.NoError(t, err)

	// Delete the unread one directly — read state is irrelevant.
	removed, err := DeleteHumanMessage(unreadID)
	require.NoError(t, err)
	assert.True(t, removed, "an existing message is removed")
	msgs, _ := ListHumanMessagesPage(HumanMessageFilter{}, 0, MaxHumanMessagesPage)
	require.Len(t, msgs, 1)
	assert.Equal(t, "read", msgs[0].Body, "only the deleted message is gone")

	// Deleting a non-existent id is a no-op, not an error.
	removed, err = DeleteHumanMessage(999999)
	require.NoError(t, err)
	assert.False(t, removed)

	// And the still-read message can be deleted too.
	removed, err = DeleteHumanMessage(readID)
	require.NoError(t, err)
	assert.True(t, removed)
	msgs, _ = ListHumanMessagesPage(HumanMessageFilter{}, 0, MaxHumanMessagesPage)
	assert.Empty(t, msgs)
}

func TestHumanMessages_AttachmentInsertAndClearReturnExactPaths(t *testing.T) {
	setupTestDB(t)
	readID, err := InsertHumanMessageWithAttachments(
		&HumanMessage{FromConv: "c", Body: "read"},
		[]*HumanMessageAttachment{{Filename: "read.txt", SizeBytes: 1, StoragePath: "/private/read"}})
	require.NoError(t, err)
	unreadID, err := InsertHumanMessageWithAttachments(
		&HumanMessage{FromConv: "c", Body: "unread"},
		[]*HumanMessageAttachment{{Filename: "unread.txt", SizeBytes: 1, StoragePath: "/private/unread"}})
	require.NoError(t, err)
	_, err = MarkHumanMessageRead(readID)
	require.NoError(t, err)
	total, sender, totalCount, senderCount, err := HumanMessageAttachmentUsage("", "c")
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	assert.Equal(t, int64(2), sender)
	assert.Equal(t, 2, totalCount)
	assert.Equal(t, 2, senderCount)

	n, paths, err := DeleteReadHumanMessagesWithAttachments()
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{"/private/read"}, paths)
	left, err := GetHumanMessage(unreadID)
	require.NoError(t, err)
	require.NotNil(t, left)
	require.NotNil(t, left.Attachment())
	assert.Equal(t, "/private/unread", left.Attachment().StoragePath)

	// A decision read but not answered stays, with its attachment.
	pendingID, err := InsertHumanMessageWithAttachments(
		&HumanMessage{FromConv: "c", Body: "which?", Options: []string{"a", "b"}},
		[]*HumanMessageAttachment{{Filename: "mock.png", SizeBytes: 1, StoragePath: "/private/mock"}})
	require.NoError(t, err)
	_, err = MarkHumanMessageRead(pendingID)
	require.NoError(t, err)
	n, paths, err = DeleteReadHumanMessagesWithAttachments()
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, paths)

	claimed, err := DecideHumanMessage(pendingID, "a")
	require.NoError(t, err)
	require.True(t, claimed)
	n, paths, err = DeleteReadHumanMessagesWithAttachments()
	require.NoError(t, err)
	assert.Equal(t, 1, n, "an answered decision clears like any read message")
	assert.Equal(t, []string{"/private/mock"}, paths)
	total, sender, totalCount, senderCount, err = HumanMessageAttachmentUsage("", "c")
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Equal(t, int64(1), sender)
	assert.Equal(t, 1, totalCount)
	assert.Equal(t, 1, senderCount)
}

func TestHumanMessages_PagedFiltersRunInSQL(t *testing.T) {
	setupTestDB(t)
	var ids []int64
	insert := func(m *HumanMessage) {
		id, err := InsertHumanMessage(m)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	// Stored before its conversation was linked to an actor: from_agent is
	// blank and the sender filter must heal it through agent_conversations.
	insert(&HumanMessage{FromConv: "conv-legacy", FromTitle: "legacy", Subject: "old row"})
	agentID, _, err := EnsureAgentForConv("conv-legacy", "test")
	require.NoError(t, err)
	require.NoError(t, LinkConvToAgent("conv-new", agentID, ConvRoleGeneration, "test"))
	insert(&HumanMessage{FromConv: "conv-new", Subject: "Deploy DONE"})
	insert(&HumanMessage{FromAgent: "agt_other", Subject: "help with " + agentID, Body: "mentions"}) // mention only
	insert(&HumanMessage{GroupName: FederationHumanGroup("peer-1"), Subject: "operator mail"})       // remote operator

	page := func(f HumanMessageFilter) []int64 {
		rows, err := ListHumanMessagesPage(f, 0, 10)
		require.NoError(t, err)
		var out []int64
		for _, m := range rows {
			out = append(out, m.ID)
		}
		return out
	}
	assert.Equal(t, []int64{ids[1], ids[0]}, page(HumanMessageFilter{Sender: agentID}),
		"sender matches the stable id and heals legacy rows, but not a mention")
	assert.Equal(t, []int64{ids[1]}, page(HumanMessageFilter{Query: "deploy done"}), "search is case-insensitive")
	assert.Equal(t, []int64{ids[2], ids[1], ids[0]}, page(HumanMessageFilter{Query: agentID}),
		"search covers the healed sender id and the body")
	assert.Equal(t, []int64{ids[3]}, page(HumanMessageFilter{SenderlessGroupPrefix: FederationHumanGroup("")}))

	_, err = MarkHumanMessageRead(ids[1])
	require.NoError(t, err)
	assert.Equal(t, []int64{ids[3], ids[2], ids[0]}, page(HumanMessageFilter{Unread: true}))

	n, err := CountHumanMessages(HumanMessageFilter{Sender: agentID})
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	pos, err := HumanMessagePosition(HumanMessageFilter{Sender: agentID}, ids[0])
	require.NoError(t, err)
	assert.Equal(t, 1, pos, "the legacy row is second newest among the sender's")
	pos, err = HumanMessagePosition(HumanMessageFilter{Sender: agentID}, ids[2])
	require.NoError(t, err)
	assert.Equal(t, -1, pos, "a row outside the filter has no position")

	rows, err := ListHumanMessagesPage(HumanMessageFilter{}, 1, 2)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, ids[2], rows[0].ID, "offset skips the newest")
	rows, err = ListHumanMessagesPage(HumanMessageFilter{}, 0, 1_000_000)
	require.NoError(t, err)
	assert.Len(t, rows, 4, "an oversized limit is clamped, not unbounded")
}
