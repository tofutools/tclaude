package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHumanMessages_InsertListCount(t *testing.T) {
	setupTestDB(t)

	msgs, err := ListHumanMessages()
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

	msgs, err = ListHumanMessages()
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

func TestHumanMessages_MarkRead(t *testing.T) {
	setupTestDB(t)
	id, err := InsertHumanMessage(&HumanMessage{FromConv: "c", Body: "x"})
	require.NoError(t, err)

	changed, err := MarkHumanMessageRead(id)
	require.NoError(t, err)
	assert.True(t, changed, "first mark transitions the row")

	msgs, _ := ListHumanMessages()
	require.Len(t, msgs, 1)
	firstReadAt := msgs[0].ReadAt
	require.False(t, firstReadAt.IsZero(), "read_at is stamped")

	// Idempotent: re-marking is a no-op and leaves the timestamp stable.
	changed, err = MarkHumanMessageRead(id)
	require.NoError(t, err)
	assert.False(t, changed, "re-marking an already-read message is a no-op")
	msgs, _ = ListHumanMessages()
	assert.Equal(t, firstReadAt, msgs[0].ReadAt, "read timestamp stays stable")

	// A non-existent id is a no-op, not an error.
	changed, err = MarkHumanMessageRead(999999)
	require.NoError(t, err)
	assert.False(t, changed)

	n, _ := CountUnreadHumanMessages()
	assert.Equal(t, 0, n)
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

	// Clear deletes only the read messages; the fresh unread survives.
	deleted, err := DeleteReadHumanMessages()
	require.NoError(t, err)
	assert.Equal(t, 3, deleted)
	msgs, _ := ListHumanMessages()
	require.Len(t, msgs, 1)
	assert.Equal(t, "fresh", msgs[0].Body)
	assert.False(t, msgs[0].IsRead())
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
	msgs, _ := ListHumanMessages()
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
	msgs, _ = ListHumanMessages()
	assert.Empty(t, msgs)
}
