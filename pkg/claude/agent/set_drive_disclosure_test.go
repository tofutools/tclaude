package agent

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The drive switch's report is the deliverable, not decoration: TCL-1082 exists
// because an operator could not tell what a rollback had actually done. Each
// sentence below is here because leaving it out produces an operator who
// believes something false, so each gets an assertion.
//
// These live at the renderer rather than in a flow test for a reason a mutation
// pass made concrete: the live-channel sentence prints only for a conversation
// on a connected API channel, which no flow test stands up — so deleting that
// sentence outright left the whole suite green. A renderer test is the cheapest
// thing that can fail for it.

// TestSetDriveDisclosure_NamesTheRecordAndThatItEdited is the ordinary rollback.
func TestSetDriveDisclosure_NamesTheRecordAndThatItEdited(t *testing.T) {
	stdout := new(bytes.Buffer)
	rc := printSetDrive(&setDriveResp{
		ConvID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		Drive:  setDriveSendKeys, Record: "agent profile", Changed: true,
	}, false, stdout, new(bytes.Buffer))

	require.Equal(t, rcOK, rc)
	out := stdout.String()
	assert.Contains(t, out, setDriveSendKeys)
	assert.Contains(t, out, "agent profile",
		"two shapes of durably-off look identical unless the record is named")
	assert.Contains(t, out, "edited")
	assert.NotContains(t, out, "CREATED")
	assert.Contains(t, out, "not future members",
		"a pin is per-agent, and an operator expecting the group's next spawn to "+
			"follow will be wrong quietly")
}

// TestSetDriveDisclosure_SaysCreatedWhenNothingWasRecorded: "created" is the
// diagnostic that a lower tier had been answering for this agent.
func TestSetDriveDisclosure_SaysCreatedWhenNothingWasRecorded(t *testing.T) {
	stdout := new(bytes.Buffer)
	rc := printSetDrive(&setDriveResp{
		ConvID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		Drive:  setDriveSendKeys, Record: "agent profile", Created: true, Changed: true,
	}, false, stdout, new(bytes.Buffer))

	require.Equal(t, rcOK, rc)
	out := stdout.String()
	assert.Contains(t, out, "CREATED")
	assert.Contains(t, out, "free to answer for it",
		"the operator must learn that a default profile had been speaking for this agent")
}

// TestSetDriveDisclosure_LiveChannelSurvivesARestart pins the sentence the whole
// skip argument turned on.
//
// "Until that channel ends" is what made a daemon restart look like a channel
// ending — it is not. The pane, the copilot process and the RPC listener all
// survive a restart and the reconnect sweep re-adopts; only a relaunch ends the
// channel. An operator told the wrong event waits for something that never
// happens.
func TestSetDriveDisclosure_LiveChannelSurvivesARestart(t *testing.T) {
	stdout := new(bytes.Buffer)
	rc := printSetDrive(&setDriveResp{
		ConvID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		Drive:  setDriveSendKeys, Record: "agent profile", Changed: true, Live: true,
	}, false, stdout, new(bytes.Buffer))

	require.Equal(t, rcOK, rc)
	out := stdout.String()
	assert.Contains(t, out, "durable now")
	assert.Contains(t, out, "restart does not end it",
		"a restart is agentd's amnesia, not the channel ending, and the operator has "+
			"to be told which")
	assert.Contains(t, out, "relaunch",
		"the relaunch is the thing that actually makes the pin bite for a running pane")
}

// TestSetDriveDisclosure_RestartSentenceSurvivesAnUnknownChannel is the arm that
// matters most and is easiest to miss.
//
// The daemon-down path cannot populate Live — there is no daemon to ask about
// handles — and that is exactly the operator most likely to be mid-incident with
// a pane still serving --ui-server. A sentence gated on Live would be silently
// absent for them, and absent disclosure looks exactly like nothing to disclose.
func TestSetDriveDisclosure_RestartSentenceSurvivesAnUnknownChannel(t *testing.T) {
	stdout := new(bytes.Buffer)
	rc := printSetDrive(&setDriveResp{
		ConvID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		Drive:  setDriveSendKeys, Record: "agent profile", Changed: true,
	}, false, stdout, new(bytes.Buffer))

	require.Equal(t, rcOK, rc)
	out := stdout.String()
	assert.Contains(t, out, "survives an agentd restart",
		"the path that cannot KNOW about a channel must say what it does not know, "+
			"rather than say nothing")
	assert.Contains(t, out, "only a relaunch ends it")
}

// TestSetDriveDisclosure_NoOpSaysUnchanged: a no-op reported as a change is a
// small lie that costs a debugging session later.
func TestSetDriveDisclosure_NoOpSaysUnchanged(t *testing.T) {
	stdout := new(bytes.Buffer)
	rc := printSetDrive(&setDriveResp{
		ConvID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		Drive:  setDriveSendKeys, Record: "agent profile",
	}, false, stdout, new(bytes.Buffer))

	require.Equal(t, rcOK, rc)
	out := stdout.String()
	assert.Contains(t, out, "unchanged")
	assert.NotContains(t, out, "→",
		"nothing moved, so nothing may be rendered as a transition")
	assert.NotContains(t, out, "not future members",
		"the pin advice belongs to a pin that happened")
}

// TestSetDriveDisclosure_NothingRecordedIsSaidPlainly: the read-back shape where
// no record answers at all. Reporting a drive here would assert a posture nobody
// chose.
func TestSetDriveDisclosure_NothingRecordedIsSaidPlainly(t *testing.T) {
	stdout := new(bytes.Buffer)
	rc := printSetDrive(&setDriveResp{
		ConvID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", Record: "none",
	}, false, stdout, new(bytes.Buffer))

	require.Equal(t, rcOK, rc)
	assert.Contains(t, stdout.String(), "no record says")
}
