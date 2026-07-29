package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestStandingOrderDebounceQueuesOnceToCurrentAgentGeneration(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.WaitForBackgroundForTest)
	group := f.HaveGroup("debounce-team")
	f.HaveMemberWithRole(group.Name, "conv-old", "worker")
	agentID, err := db.AgentIDForConv("conv-old")
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "debounce-old", ConvID: "conv-old", Harness: harness.CodexName,
		Status: "idle",
	}))

	orderID, err := db.InsertStandingOrder(&db.StandingOrder{
		Name: "coalesced", TargetKind: db.StandingTargetGroup,
		GroupID: group.ID, TargetRole: "worker",
		Summary:      "Run the expensive check once after the burst.",
		TriggerEvent: db.StandingTriggerToolAfter,
		Timing:       db.StandingTimingNextTurn, Cadence: db.StandingCadenceAlways,
		DebounceSeconds: 5, Enabled: true, OperatorAuthored: true,
	})
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, db.ScheduleStandingDebounce(&db.StandingDebounce{
		OrderID: orderID, OrderRevision: 1,
		TargetAgent: agentID, TargetConv: "conv-old",
		Harness: harness.CodexName, Detail: "matched tool.after",
		DueAt: now, MaxDueAt: now.Add(time.Minute), UpdatedAt: now,
	}))

	_, err = db.RotateAgentConv("conv-old", "conv-current", "clear")
	require.NoError(t, err)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "debounce-current", ConvID: "conv-current", Harness: harness.CodexName,
		Status: "idle",
	}))

	agentd.RunStandingOrderDebounceTickForTest(now)

	pending, err := db.GetDueStandingDebounce(orderID, agentID, now)
	require.NoError(t, err)
	assert.Nil(t, pending)
	oldMessages, err := db.ListAgentMessagesForConv("conv-old", 0)
	require.NoError(t, err)
	assert.Empty(t, oldMessages, "the observed conversation is only a routing snapshot")
	currentMessages, err := db.ListAgentMessagesForConv("conv-current", 0)
	require.NoError(t, err)
	require.Len(t, currentMessages, 1)
	assert.Contains(t, currentMessages[0].Body, "Run the expensive check once")
	origin, err := db.AgentMessageStandingOrderOrigin(currentMessages[0].ID)
	require.NoError(t, err)
	require.NotNil(t, origin)
	assert.Equal(t, orderID, origin.OrderID)

	latest, err := db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingOutcomeDelivered, latest.Outcome)
	assert.Equal(t, db.StandingTransportMessage, latest.Transport)
	assert.Equal(t, agentID, latest.TargetAgent)
	assert.Equal(t, "conv-current", latest.TargetConv)

	agentd.RunStandingOrderDebounceTickForTest(now.Add(time.Second))
	again, err := db.ListAgentMessagesForConv("conv-current", 0)
	require.NoError(t, err)
	assert.Len(t, again, 1, "a consumed trailing edge cannot queue twice")
}

func TestStandingOrderDebounceRechecksCooldownAtQueueTime(t *testing.T) {
	f := newFlow(t)
	group := f.HaveGroup("debounce-cooldown-team")
	f.HaveMember(group.Name, "conv-cooldown")
	agentID, err := db.AgentIDForConv("conv-cooldown")
	require.NoError(t, err)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "debounce-cooldown", ConvID: "conv-cooldown",
		Harness: harness.DefaultName, Status: "idle",
	}))
	orderID, err := db.InsertStandingOrder(&db.StandingOrder{
		Name: "cooldown-edge", TargetKind: db.StandingTargetGroup,
		GroupID: group.ID, Summary: "Do this at most once per minute.",
		TriggerEvent: db.StandingTriggerSessionStart,
		Timing:       db.StandingTimingNextTurn, Cadence: db.StandingCadenceAlways,
		CooldownSeconds: 60, DebounceSeconds: 5,
		Enabled: true, OperatorAuthored: true,
	})
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, db.ScheduleStandingDebounce(&db.StandingDebounce{
		OrderID: orderID, OrderRevision: 1,
		TargetAgent: agentID, TargetConv: "conv-cooldown",
		Harness: harness.DefaultName,
		DueAt:   now, MaxDueAt: now.Add(time.Minute), UpdatedAt: now,
	}))
	_, err = db.RecordStandingDelivery(&db.StandingDelivery{
		OrderID: orderID, OrderRevision: 1,
		TargetConv: "conv-cooldown", TargetAgent: agentID,
		Outcome:   db.StandingOutcomeDelivered,
		Transport: db.StandingTransportMessage, Harness: harness.DefaultName,
	})
	require.NoError(t, err)

	agentd.RunStandingOrderDebounceTickForTest(now)

	candidate, err := db.GetDueStandingDebounce(orderID, agentID, now)
	require.NoError(t, err)
	assert.Nil(t, candidate)
	messages, err := db.ListAgentMessagesForConv("conv-cooldown", 0)
	require.NoError(t, err)
	assert.Empty(t, messages)
	latest, err := db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingOutcomeSuppressedCooldown, latest.Outcome)
}
