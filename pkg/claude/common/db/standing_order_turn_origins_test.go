package db

import (
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
	internal, err := AgentMessageIsStandingOrder(spoofedID)
	require.NoError(t, err)
	assert.False(t, internal, "a sender-controlled subject cannot suppress automations")

	trustedID, err := InsertStandingOrderAgentMessage(&AgentMessage{
		ToConv: "conv_target", Subject: "durable reminder", Body: "trusted",
		OperatorAuthored: true,
	}, 10, 4)
	require.NoError(t, err)
	internal, err = AgentMessageIsStandingOrder(trustedID)
	require.NoError(t, err)
	assert.True(t, internal)
	assert.True(t, IsOperatorAgentMessage(trustedID),
		"the atomic helper must preserve real operator authorship")

	require.NoError(t, ArmStandingOrderTurnOrigin("agt_target", 41, now, time.Minute))
	active, err := StandingOrderTurnOriginActive("agt_target", now)
	require.NoError(t, err)
	assert.False(t, active, "arming alone does not classify a turn as internal")

	activated, err := ActivateStandingOrderTurnOrigin(
		"agt_target", now.Add(time.Second), time.Hour)
	require.NoError(t, err)
	assert.True(t, activated)
	active, err = StandingOrderTurnOriginActive("agt_target", now.Add(30*time.Minute))
	require.NoError(t, err)
	assert.True(t, active, "active origin survives projector restart")

	assert.Error(t, ArmStandingOrderTurnOrigin(
		"agt_target", 42, now.Add(2*time.Second), time.Minute),
		"an active internal turn cannot be overwritten")
	require.NoError(t, CancelPendingStandingOrderTurnOrigin("agt_target", 41),
		"pending cancellation never clears an active marker")
	active, err = StandingOrderTurnOriginActive("agt_target", now.Add(3*time.Second))
	require.NoError(t, err)
	assert.True(t, active)

	require.NoError(t, ClearStandingOrderTurnOrigin("agt_target"))
	active, err = StandingOrderTurnOriginActive("agt_target", now.Add(3*time.Second))
	require.NoError(t, err)
	assert.False(t, active)
}

func TestStandingOrderTurnOriginPendingExpiryAndExactCancel(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	require.NoError(t, ArmStandingOrderTurnOrigin("agt_target", 51, now, time.Second))
	activated, err := ActivateStandingOrderTurnOrigin(
		"agt_target", now.Add(2*time.Second), time.Hour)
	require.NoError(t, err)
	assert.False(t, activated, "an expired handshake cannot suppress a later human turn")

	require.NoError(t, ArmStandingOrderTurnOrigin(
		"agt_target", 52, now.Add(2*time.Second), time.Minute),
		"an expired marker is replaceable")
	require.NoError(t, CancelPendingStandingOrderTurnOrigin("agt_target", 51))
	activated, err = ActivateStandingOrderTurnOrigin(
		"agt_target", now.Add(3*time.Second), time.Hour)
	require.NoError(t, err)
	assert.True(t, activated, "cancelling an old message does not remove a newer handshake")
}
