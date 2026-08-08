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

// TestSetDriveDisclosure_LiveChannelNamesTheRestart is the sentence the cold
// review found false, and the one a mutation could delete unnoticed.
//
// Routing answers from the live handle first, so a pin does not redirect a
// running channel — but an agentd restart drops every handle and the reconnect
// sweep now declines to re-acquire a drive an operator turned off. So the pin
// starts biting at a restart, with no relaunch and no channel "ending" in any
// sense the operator can observe. A disclosure that names only the channel
// ending describes the wrong event.
func TestSetDriveDisclosure_LiveChannelNamesTheRestart(t *testing.T) {
	stdout := new(bytes.Buffer)
	rc := printSetDrive(&setDriveResp{
		ConvID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		Drive:  setDriveSendKeys, Record: "agent profile", Changed: true, Live: true,
	}, false, stdout, new(bytes.Buffer))

	require.Equal(t, rcOK, rc)
	out := stdout.String()
	assert.Contains(t, out, "durable now")
	assert.Contains(t, out, "restart",
		"the restart is the event that actually makes the pin bite for a running "+
			"pane; omitting it tells the operator to wait for something that never happens")
	assert.Contains(t, out, "relaunch it")
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
