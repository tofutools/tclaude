package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStandingOrderTurnOriginLifecycle(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	spoofedID, err := InsertAgentMessage(&AgentMessage{
		ToConv: "conv_target", Subject: "[standing-order:spoofed]", Body: "not trusted metadata",
	})
	require.NoError(t, err)
	origin, err := AgentMessageStandingOrderOrigin(spoofedID)
	require.NoError(t, err)
	assert.Nil(t, origin, "a sender-controlled subject cannot suppress automations")

	trustedID, err := InsertStandingOrderAgentMessage(&AgentMessage{
		ToConv: "conv_target", Subject: "durable reminder", Body: "trusted",
		OperatorAuthored: true,
	}, 10, 4)
	require.NoError(t, err)
	origin, err = AgentMessageStandingOrderOrigin(trustedID)
	require.NoError(t, err)
	require.NotNil(t, origin)
	assert.Equal(t, int64(10), origin.OrderID)
	assert.Equal(t, int64(4), origin.OrderRevision)
	assert.True(t, strings.HasPrefix(origin.OpenCodeMessageID, "msg_tclaude_"))
	assert.True(t, IsOperatorAgentMessage(trustedID),
		"the atomic helper must preserve real operator authorship")

	require.NoError(t, ArmStandingOrderTurnOrigin(
		"agt_target", "conv_target", trustedID, origin.OpenCodeMessageID,
		now, time.Minute))
	pending, err := GetStandingOrderTurnOrigin("agt_target", "conv_target", now)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, StandingOrderTurnOriginPending, pending.State)
	assert.Nil(t, mustGetTurnOrigin(t, "agt_target", "conv_old", now),
		"an old conversation generation cannot observe the marker")

	activated, err := ActivateStandingOrderTurnOrigin(
		"agt_target", "conv_target", "msg_wrong_parent",
		now.Add(time.Second), time.Hour)
	require.NoError(t, err)
	assert.False(t, activated, "another prompt cannot steal the marker")
	activated, err = ActivateStandingOrderTurnOrigin(
		"agt_target", "conv_old", origin.OpenCodeMessageID,
		now.Add(time.Second), time.Hour)
	require.NoError(t, err)
	assert.False(t, activated, "an old generation cannot activate the marker")
	activated, err = ActivateStandingOrderTurnOrigin(
		"agt_target", "conv_target", origin.OpenCodeMessageID,
		now.Add(time.Second), time.Hour)
	require.NoError(t, err)
	assert.True(t, activated)

	active := mustGetTurnOrigin(t, "agt_target", "conv_target", now.Add(30*time.Minute))
	require.NotNil(t, active)
	assert.Equal(t, StandingOrderTurnOriginActive, active.State,
		"active origin survives projector restart")
	assert.Error(t, ArmStandingOrderTurnOrigin(
		"agt_target", "conv_target", trustedID+1, "msg_other",
		now.Add(2*time.Second), time.Minute),
		"an active internal turn cannot be overwritten")
	require.NoError(t, CancelPendingStandingOrderTurnOrigin(
		"agt_target", "conv_target", trustedID, origin.OpenCodeMessageID),
		"pending cancellation never clears an active marker")
	require.NotNil(t, mustGetTurnOrigin(
		t, "agt_target", "conv_target", now.Add(3*time.Second)))

	require.NoError(t, CompleteStandingOrderTurnOrigin("agt_target", "conv_old"))
	require.NotNil(t, mustGetTurnOrigin(
		t, "agt_target", "conv_target", now.Add(3*time.Second)),
		"an old generation Stop cannot clear the marker")
	require.NoError(t, CompleteStandingOrderTurnOrigin("agt_target", "conv_target"))
	assert.Nil(t, mustGetTurnOrigin(
		t, "agt_target", "conv_target", now.Add(3*time.Second)))
}

func TestStandingOrderTurnOriginPendingExpiryRefreshAndExactCancel(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	require.NoError(t, ArmStandingOrderTurnOrigin(
		"agt_target", "conv_target", 51, "msg_first", now, time.Second))
	activated, err := ActivateStandingOrderTurnOrigin(
		"agt_target", "conv_target", "msg_first",
		now.Add(2*time.Second), time.Hour)
	require.NoError(t, err)
	assert.False(t, activated, "an expired handshake cannot suppress a later human turn")

	require.NoError(t, ArmStandingOrderTurnOrigin(
		"agt_target", "conv_target", 52, "msg_second",
		now.Add(2*time.Second), time.Minute),
		"an expired marker is replaceable")
	require.NoError(t, CancelPendingStandingOrderTurnOrigin(
		"agt_target", "conv_target", 51, "msg_first"))
	require.NoError(t, RefreshPendingStandingOrderTurnOrigin(
		"agt_target", "conv_target", 52, "msg_second",
		now.Add(30*time.Second), time.Minute))
	activated, err = ActivateStandingOrderTurnOrigin(
		"agt_target", "conv_target", "msg_second",
		now.Add(89*time.Second), time.Hour)
	require.NoError(t, err)
	assert.True(t, activated,
		"cancelling an old message does not remove a newer refreshed handshake")
}

func mustGetTurnOrigin(
	t *testing.T,
	targetAgent, targetConv string,
	now time.Time,
) *StandingOrderTurnOrigin {
	t.Helper()
	origin, err := GetStandingOrderTurnOrigin(targetAgent, targetConv, now)
	require.NoError(t, err)
	return origin
}
