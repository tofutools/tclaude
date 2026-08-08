package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInsertLatestCronAgentMessageCoalescesOnlyBufferedSameJobRecipient(t *testing.T) {
	setupTestDB(t)
	const target = "cron-coalesce-target"
	_, _, err := EnsureAgentForConv(target, "test")
	require.NoError(t, err)

	deliveredID, replaced, err := InsertLatestCronAgentMessage(
		&AgentMessage{ToConv: target, Body: "delivered history"}, 11)
	require.NoError(t, err)
	assert.Zero(t, replaced)
	require.NoError(t, MarkAgentMessageDelivered(deliveredID))

	oldID, replaced, err := InsertLatestCronAgentMessage(
		&AgentMessage{ToConv: target, Body: "old buffered tick"}, 11)
	require.NoError(t, err)
	assert.Zero(t, replaced, "delivered history is not a buffered tick")

	otherJobID, _, err := InsertLatestCronAgentMessage(
		&AgentMessage{ToConv: target, Body: "other job"}, 22)
	require.NoError(t, err)

	latestID, replaced, err := InsertLatestCronAgentMessage(
		&AgentMessage{ToConv: target, Body: "latest buffered tick"}, 11)
	require.NoError(t, err)
	assert.EqualValues(t, 1, replaced)

	old, err := GetAgentMessage(oldID)
	require.NoError(t, err)
	assert.Nil(t, old, "the stale buffered tick is removed")
	for _, id := range []int64{deliveredID, otherJobID, latestID} {
		message, getErr := GetAgentMessage(id)
		require.NoError(t, getErr)
		assert.NotNil(t, message, "unrelated or delivered row %d is preserved", id)
	}
	origin, err := AgentMessageCronJobID(latestID)
	require.NoError(t, err)
	assert.EqualValues(t, 11, origin)
}

func TestInsertLatestCronAgentMessageLeavesClaimedTickInFlight(t *testing.T) {
	setupTestDB(t)
	const target = "cron-claimed-target"
	_, _, err := EnsureAgentForConv(target, "test")
	require.NoError(t, err)

	claimedID, _, err := InsertLatestCronAgentMessage(
		&AgentMessage{ToConv: target, Body: "already in flight"}, 33)
	require.NoError(t, err)
	token, claimed, err := ClaimAgentMessageNudge(claimedID, time.Now())
	require.NoError(t, err)
	require.True(t, claimed)

	latestID, replaced, err := InsertLatestCronAgentMessage(
		&AgentMessage{ToConv: target, Body: "next tick"}, 33)
	require.NoError(t, err)
	assert.Zero(t, replaced, "an in-flight pane injection cannot be retracted")

	for _, id := range []int64{claimedID, latestID} {
		message, getErr := GetAgentMessage(id)
		require.NoError(t, getErr)
		assert.NotNil(t, message)
	}

	released, err := ReleaseAgentMessageNudge(claimedID, token)
	require.NoError(t, err)
	require.True(t, released)
	claimedMessage, err := GetAgentMessage(claimedID)
	require.NoError(t, err)
	assert.Nil(t, claimedMessage, "a failed old delivery is discarded once a newer tick exists")
	latestMessage, err := GetAgentMessage(latestID)
	require.NoError(t, err)
	assert.NotNil(t, latestMessage)
}
