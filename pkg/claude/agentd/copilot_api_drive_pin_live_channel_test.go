package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
)

// TCL-1082. A durable "take this agent off the API drive" edits the record that
// LAUNCHES decide from. This file measures what that edit does — and does not do
// — to an agent that is running RIGHT NOW on a connected API channel, because
// the answer decides what the command may truthfully tell an operator.
//
// The reason it is not obvious from the record alone: copilotAPIDriven answers
// from the live handle FIRST and only reads the durable records when there is no
// handle. So a pin is a statement about the next launch, and a running channel
// is not bound by it.
//
// Measured rather than argued — the pin's whole value is that an operator can
// believe what it reports.

// recordAgentDriveOn puts the fixture's agent in the shape a Copilot spawn
// leaves it in: a durable relaunch profile whose copilot_api says true. The CAS
// the rollback uses needs a profile to edit INSIDE, so this is also what makes
// the pin below a real edit rather than a refusal.
func recordAgentDriveOn(t *testing.T, agentID string) {
	t.Helper()
	on := true
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, CopilotAPI: &on,
	}))
}

// pinDriveOffFor performs the durable un-choose against the agent profile.
func pinDriveOffFor(t *testing.T, convID string) {
	t.Helper()
	target, err := db.CopilotDriveTargetForConv(convID)
	require.NoError(t, err)
	require.Equal(t, db.CopilotDriveRecordAgentProfile, target.Record)
	require.True(t, target.Value, "the scenario needs a drive that was recorded ON")
	ok, err := db.CompareAndSetAgentCopilotAPI(target.AgentID, false, target.Raw)
	require.NoError(t, err)
	require.True(t, ok, "the pin's compare-and-set must hold")
}

// TestPinningTheDriveOffLeavesALiveChannelCarryingDeliveries is the measurement
// the command's disclosure has to be built on.
//
// A pin lands on the durable record; the running pane keeps the channel it was
// launched with, and deliveries keep taking it. Telling an operator "this agent
// is off the API drive" while its next message still travels over RPC would be
// the rollback-they-believe-worked failure in its purest form.
func TestPinningTheDriveOffLeavesALiveChannelCarryingDeliveries(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	recordAgentDriveOn(t, fixture.agentID)
	require.True(t, copilotAPIDriven(fixture.convID),
		"the scenario needs an agent on the API drive before the pin")

	pinDriveOffFor(t, fixture.convID)

	// The durable half took effect: every LAUNCH from here on reads send-keys.
	target, err := db.CopilotDriveTargetForConv(fixture.convID)
	require.NoError(t, err)
	assert.False(t, target.Value, "the pin must be durable immediately")

	// The live half did not, and this is the sentence the operator needs.
	assert.True(t, copilotAPIDriven(fixture.convID),
		"MEASURED: a connected handle answers before the record, so a pin does not "+
			"redirect the channel this pane is already running on")

	group, err := db.CreateAgentGroup("copilot-api-pin-live", "")
	require.NoError(t, err)
	messageID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: group, FromConv: "peer", ToConv: fixture.convID, Body: "hello",
	})
	require.NoError(t, err)
	message, err := db.GetAgentMessage(messageID)
	require.NoError(t, err)

	require.True(t, sendNudgeBracket(fixture.convID, message, "[msg #1 from peer] hello"))
	assert.Contains(t, fixture.server.methodsCalled(), copilotapi.MethodSessionSend,
		"the delivery still went over the live channel after the pin")
	fixture.assertNoKeystrokes(t)
}

// TestPinningTheDriveOffTakesEffectOnceTheChannelIsGone is the other half, and
// the one that proves the test above is measuring the HANDLE rather than a pin
// that simply does nothing.
//
// Dropping the handle is what a relaunch does to it. With no handle, the record
// is consulted, the pin is read, and the agent is back on the keystroke path
// every Copilot agent ran on before the drive existed.
func TestPinningTheDriveOffTakesEffectOnceTheChannelIsGone(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	recordAgentDriveOn(t, fixture.agentID)
	pinDriveOffFor(t, fixture.convID)

	copilotAPISessions.Drop(fixture.convID)

	assert.False(t, copilotAPIDriven(fixture.convID),
		"with the channel gone, the pinned record decides and the agent is on send-keys")
}
