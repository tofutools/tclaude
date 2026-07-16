package db

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInsertAgentMessageBoundedRejectsAtomicallyAtLimit(t *testing.T) {
	setupTestDB(t)
	const target = "bounded-target"
	_, _, err := EnsureAgentForConv(target, "test")
	require.NoError(t, err)

	const limit = 3
	const senders = 20
	var wg sync.WaitGroup
	results := make(chan error, senders)
	for range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := InsertAgentMessageBounded(&AgentMessage{FromConv: "sender", ToConv: target, Body: "regular"}, limit)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	accepted := 0
	rejected := 0
	for err := range results {
		if err == nil {
			accepted++
			continue
		}
		var full *AgentMessageQueueFullError
		require.True(t, errors.As(err, &full), "unexpected insert error: %v", err)
		assert.Equal(t, limit, full.Pending)
		assert.Equal(t, limit, full.Limit)
		rejected++
	}
	assert.Equal(t, limit, accepted, "concurrent senders must not race past the cap")
	assert.Equal(t, senders-limit, rejected)
	pending, err := CountUndeliveredForAgent(backpressureAgentIDForConv(t, target))
	require.NoError(t, err)
	assert.Equal(t, limit, pending)
}

func TestInsertAgentMessageBoundedReopensAfterReadAndInternalInsertIsExempt(t *testing.T) {
	setupTestDB(t)
	const target = "bounded-reopen-target"
	_, _, err := EnsureAgentForConv(target, "test")
	require.NoError(t, err)

	first, pending, err := InsertAgentMessageBounded(&AgentMessage{ToConv: target, Body: "first"}, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, pending)

	_, pending, err = InsertAgentMessageBounded(&AgentMessage{ToConv: target, Body: "rejected"}, 1)
	var full *AgentMessageQueueFullError
	require.ErrorAs(t, err, &full)
	assert.Equal(t, 1, pending)
	require.NoError(t, MarkAgentMessageDelivered(first))

	_, pending, err = InsertAgentMessageBounded(&AgentMessage{ToConv: target, Body: "still rejected after delivery"}, 1)
	require.ErrorAs(t, err, &full, "nudge delivery alone must not free durable backlog capacity")
	assert.Equal(t, 1, pending)
	require.NoError(t, MarkAgentMessageRead(first))

	_, pending, err = InsertAgentMessageBounded(&AgentMessage{ToConv: target, Body: "after read"}, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, pending)

	// Internal correctness-critical paths deliberately use the unbounded API.
	// They may cross the regular-message threshold rather than being dropped.
	_, err = InsertAgentMessage(&AgentMessage{ToConv: target, Body: "lifecycle handoff"})
	require.NoError(t, err)
	count, err := CountUndeliveredForAgent(backpressureAgentIDForConv(t, target))
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

func TestInsertAgentMessageBoundedCountsPreEnrollmentBacklog(t *testing.T) {
	setupTestDB(t)
	const target = "later-enrolled-target"
	const successor = "later-enrolled-successor"
	const limit = 2

	for range limit {
		_, _, err := InsertAgentMessageBounded(&AgentMessage{ToConv: target, Body: "before enrollment"}, limit)
		require.NoError(t, err)
	}
	_, _, err := EnsureAgentForConv(target, "test")
	require.NoError(t, err)
	_, err = RotateAgentConv(target, successor, "clear")
	require.NoError(t, err)
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE agent_messages SET to_agent = '' WHERE to_conv = ?`, target)
	require.NoError(t, err, "preserve the pre-enrollment companion-column shape across rotation")

	_, pending, err := InsertAgentMessageBounded(&AgentMessage{ToConv: successor, Body: "after rotation"}, limit)
	var full *AgentMessageQueueFullError
	require.ErrorAs(t, err, &full)
	assert.Equal(t, limit, pending)
	assert.Equal(t, limit, full.Pending)
}

func TestExplicitReadProcessesAlreadyReadRegularMessage(t *testing.T) {
	setupTestDB(t)
	const target = "missed-submit-hook-target"
	_, _, err := EnsureAgentForConv(target, "test")
	require.NoError(t, err)

	id, _, err := InsertAgentMessageBounded(&AgentMessage{ToConv: target, Body: "inline"}, 1)
	require.NoError(t, err)
	claim, claimed, err := ClaimAgentMessageNudge(id, time.Now())
	require.NoError(t, err)
	require.True(t, claimed)
	completed, err := CompleteAgentMessageNudgeState(id, claim, time.Now(), true)
	require.NoError(t, err)
	require.True(t, completed)

	message, err := GetAgentMessage(id)
	require.NoError(t, err)
	require.False(t, message.ReadAt.IsZero(), "inline delivery pre-marks the archival row read")
	require.True(t, message.ProcessedAt.IsZero(), "missed UserPromptSubmit leaves it unprocessed")
	_, _, err = InsertAgentMessageBounded(&AgentMessage{ToConv: target, Body: "blocked"}, 1)
	require.Error(t, err)

	require.NoError(t, MarkAgentMessageRead(id))
	message, err = GetAgentMessage(id)
	require.NoError(t, err)
	assert.False(t, message.ProcessedAt.IsZero())
	_, pending, err := InsertAgentMessageBounded(&AgentMessage{ToConv: target, Body: "recovered"}, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, pending)
}

func TestRegularMessageProcessingTracksInlineAndPointerFlows(t *testing.T) {
	setupTestDB(t)
	const target = "processing-target"
	_, _, err := EnsureAgentForConv(target, "test")
	require.NoError(t, err)

	inlineID, _, err := InsertAgentMessageBounded(&AgentMessage{ToConv: target, Body: "inline"}, 10)
	require.NoError(t, err)
	pointerID, _, err := InsertAgentMessageBounded(&AgentMessage{ToConv: target, Body: "pointer"}, 10)
	require.NoError(t, err)

	started, err := MarkRegularAgentMessageStarted(inlineID, target, true, time.Now())
	require.NoError(t, err)
	require.True(t, started)
	inline, err := GetAgentMessage(inlineID)
	require.NoError(t, err)
	assert.False(t, inline.StartedAt.IsZero())
	assert.False(t, inline.ReadAt.IsZero())
	assert.True(t, inline.ProcessedAt.IsZero(), "inline prompt waits for a terminal turn hook")

	processed, err := MarkStartedRegularAgentMessagesProcessed(target, time.Now())
	require.NoError(t, err)
	assert.Equal(t, int64(1), processed)
	inline, err = GetAgentMessage(inlineID)
	require.NoError(t, err)
	assert.False(t, inline.ProcessedAt.IsZero())

	started, err = MarkRegularAgentMessageStarted(pointerID, target, false, time.Now())
	require.NoError(t, err)
	require.True(t, started)
	processed, err = MarkStartedRegularAgentMessagesProcessed(target, time.Now())
	require.NoError(t, err)
	assert.Zero(t, processed, "a pointer prompt does not consume the inbox body")
	pointer, err := GetAgentMessage(pointerID)
	require.NoError(t, err)
	assert.True(t, pointer.ReadAt.IsZero())
	assert.True(t, pointer.ProcessedAt.IsZero())

	require.NoError(t, MarkAgentMessageRead(pointerID))
	pointer, err = GetAgentMessage(pointerID)
	require.NoError(t, err)
	assert.False(t, pointer.ReadAt.IsZero())
	assert.False(t, pointer.ProcessedAt.IsZero())
}

func TestLaterRegularMessagePromptWatermarksEarlierInlineRows(t *testing.T) {
	setupTestDB(t)
	const target = "watermark-target"
	_, _, err := EnsureAgentForConv(target, "test")
	require.NoError(t, err)

	earlierID, _, err := InsertAgentMessageBounded(&AgentMessage{ToConv: target, Body: "earlier"}, 10)
	require.NoError(t, err)
	require.NoError(t, MarkAgentMessageDelivered(earlierID))
	started, err := MarkRegularAgentMessageStarted(earlierID, target, true, time.Now())
	require.NoError(t, err)
	require.True(t, started)

	laterID, _, err := InsertAgentMessageBounded(&AgentMessage{ToConv: target, Body: "later"}, 10)
	require.NoError(t, err)
	started, err = MarkRegularAgentMessageStarted(laterID, "different-recipient", true, time.Now())
	require.NoError(t, err)
	assert.False(t, started, "message id must be authorized against the receiving conversation")
	started, err = MarkRegularAgentMessageStarted(laterID, target, true, time.Now())
	require.NoError(t, err)
	require.True(t, started)

	earlier, err := GetAgentMessage(earlierID)
	require.NoError(t, err)
	assert.False(t, earlier.ProcessedAt.IsZero(), "later submitted prompt proves the prior inline prompt advanced")
	later, err := GetAgentMessage(laterID)
	require.NoError(t, err)
	assert.True(t, later.ProcessedAt.IsZero(), "current prompt still waits for its terminal hook")
}

func backpressureAgentIDForConv(t *testing.T, convID string) string {
	t.Helper()
	agentID, err := AgentIDForConv(convID)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	return agentID
}
