package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStandingDebounceTrailingEdgeAndRevisionReplacement(t *testing.T) {
	setupTestDB(t)
	order := sampleOrder("debounced")
	order.Timing = StandingTimingNextTurn
	order.DebounceSeconds = 10
	orderID, err := InsertStandingOrder(order)
	require.NoError(t, err)

	base := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	first := &StandingDebounce{
		OrderID: orderID, OrderRevision: 1,
		TargetAgent: "agt_target", TargetConv: "conv_one",
		Harness: "claude", Detail: "first",
		DueAt: base.Add(10 * time.Second), MaxDueAt: base.Add(time.Minute),
		UpdatedAt: base,
	}
	require.NoError(t, ScheduleStandingDebounce(first))
	assert.Empty(t, mustDueStandingDebounces(t, base.Add(9*time.Second)))

	second := *first
	second.TargetConv = "conv_two"
	second.Detail = "latest"
	second.DueAt = base.Add(30 * time.Second)
	second.UpdatedAt = base.Add(20 * time.Second)
	require.NoError(t, ScheduleStandingDebounce(&second))

	got := mustDueStandingDebounces(t, base.Add(30*time.Second))
	require.Len(t, got, 1)
	assert.Equal(t, "conv_two", got[0].TargetConv)
	assert.Equal(t, "latest", got[0].Detail)
	assert.Equal(t, base.Add(30*time.Second), got[0].DueAt)
	assert.Equal(t, base.Add(time.Minute), got[0].MaxDueAt,
		"retriggering preserves the first event's maximum deadline")

	capped := second
	capped.DueAt = base.Add(2 * time.Minute)
	capped.MaxDueAt = base.Add(3 * time.Minute)
	capped.UpdatedAt = base.Add(40 * time.Second)
	require.NoError(t, ScheduleStandingDebounce(&capped))
	got = mustDueStandingDebounces(t, base.Add(time.Minute))
	require.Len(t, got, 1)
	assert.Equal(t, base.Add(time.Minute), got[0].DueAt)

	nextRevision := capped
	nextRevision.OrderRevision = 2
	nextRevision.DueAt = base.Add(3 * time.Minute)
	nextRevision.MaxDueAt = base.Add(4 * time.Minute)
	nextRevision.UpdatedAt = base.Add(2 * time.Minute)
	require.NoError(t, ScheduleStandingDebounce(&nextRevision))
	assert.Empty(t, mustDueStandingDebounces(t, base.Add(2*time.Minute)))
	got = mustDueStandingDebounces(t, base.Add(3*time.Minute))
	require.Len(t, got, 1)
	assert.Equal(t, int64(2), got[0].OrderRevision)
	assert.Equal(t, base.Add(4*time.Minute), got[0].MaxDueAt,
		"a delivery revision starts a fresh bounded debounce window")
}

func TestStandingDebounceAtomicConsumeRejectsStaleCandidate(t *testing.T) {
	setupTestDB(t)
	_, _, err := EnsureAgentForConv("conv_target", "test")
	require.NoError(t, err)
	order := sampleOrder("debounced-consume")
	order.Timing = StandingTimingNextTurn
	order.DebounceSeconds = 5
	orderID, err := InsertStandingOrder(order)
	require.NoError(t, err)

	base := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	pending := &StandingDebounce{
		OrderID: orderID, OrderRevision: 1,
		TargetAgent: "agt_target", TargetConv: "conv_target",
		Harness: "opencode", DueAt: base, MaxDueAt: base.Add(time.Minute),
		UpdatedAt: base,
	}
	require.NoError(t, ScheduleStandingDebounce(pending))
	newer := *pending
	newer.UpdatedAt = base.Add(time.Second)
	require.NoError(t, ScheduleStandingDebounce(&newer))

	message := &AgentMessage{
		ToConv: "conv_target", Subject: "[standing-order:debounced-consume]",
		Body: "durable reminder", OperatorAuthored: true,
	}
	delivery := &StandingDelivery{
		OrderID: orderID, OrderRevision: 1,
		TargetConv: "conv_target", TargetAgent: "agt_target",
		Outcome: StandingOutcomeDelivered, Transport: StandingTransportMessage,
		Harness: "opencode", Detail: "quiet edge",
	}
	_, err = ConsumeStandingDebounceIntoAgentMessage(
		pending, message, delivery)
	require.Error(t, err, "a retriggered candidate cannot be consumed by a stale tick")
	queued, err := ListUndeliveredAgentMessagesFor("conv_target")
	require.NoError(t, err)
	assert.Empty(t, queued, "message insertion rolls back with the stale delete")
	latest, err := LatestStandingDelivery(orderID)
	require.NoError(t, err)
	assert.Nil(t, latest, "delivery ledger insertion rolls back with the stale delete")

	current, err := GetDueStandingDebounce(orderID, "agt_target", base.Add(time.Minute))
	require.NoError(t, err)
	require.NotNil(t, current)
	messageID, err := ConsumeStandingDebounceIntoAgentMessage(
		current, message, delivery)
	require.NoError(t, err)
	assert.Positive(t, messageID)

	current, err = GetDueStandingDebounce(orderID, "agt_target", base.Add(time.Minute))
	require.NoError(t, err)
	assert.Nil(t, current)
	queued, err = ListUndeliveredAgentMessagesFor("conv_target")
	require.NoError(t, err)
	require.Len(t, queued, 1)
	origin, err := AgentMessageStandingOrderOrigin(queued[0].ID)
	require.NoError(t, err)
	require.NotNil(t, origin)
	assert.Equal(t, orderID, origin.OrderID)
	assert.Equal(t, int64(1), origin.OrderRevision)
	assert.True(t, IsOperatorAgentMessage(queued[0].ID))
	latest, err = LatestStandingDelivery(orderID)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, StandingOutcomeDelivered, latest.Outcome)
	assert.Equal(t, "agt_target", latest.TargetAgent)
}

func mustDueStandingDebounces(t *testing.T, now time.Time) []*StandingDebounce {
	t.Helper()
	got, err := ListDueStandingDebounces(now)
	require.NoError(t, err)
	return got
}
