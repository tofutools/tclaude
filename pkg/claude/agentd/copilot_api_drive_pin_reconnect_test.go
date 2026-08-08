package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TCL-1082. The reconnect sweep re-establishes channels for API-driven panes
// that survived an agentd restart. Its candidate test is "a recorded port plus a
// live Copilot pane"; the recorded DRIVE is deliberately not a filter, because a
// conversation with no recorded posture is the one whose mail routes to
// keystrokes and reconnecting it closes that sink.
//
// A durable "off" introduces a state that rule was written before: a posture
// recorded as an explicit FALSE, by an operator, for a pane that is still
// running the channel it was launched with. This measures what the sweep does
// with it, because the answer decides how long the rollback actually lasts.

// pinConversationDriveOff turns the drive off in the conversation fallback —
// the record a clone-shaped or legacy conversation keeps its drive in, and the
// one the reconnect fixture writes.
func pinConversationDriveOff(t *testing.T, convID string) {
	t.Helper()
	target, err := db.CopilotDriveTargetForConv(convID)
	require.NoError(t, err)
	require.Equal(t, db.CopilotDriveRecordConversationFallback, target.Record)
	require.True(t, target.Value, "the scenario needs a drive recorded ON")
	ok, err := db.CompareAndSetConversationCopilotAPI(convID, false, target.Raw)
	require.NoError(t, err)
	require.True(t, ok, "the pin's compare-and-set must hold")
	require.False(t, copilotAPIDriven(convID),
		"precondition: with no handle, the pinned record must already route to send-keys")
}

// TestAPinnedDriveIsNotReacquiredByTheReconnectSweep is the lifetime of a
// rollback performed against a still-running pane.
//
// The state is the ordinary one after a durable "off": the operator pinned the
// record, the pane kept running (a pin does not stop anything), and agentd was
// later restarted. The sweep then finds a recorded port and a live Copilot pane
// — its two candidate conditions.
//
// This assertion is INVERTED from the one first committed here, which measured
// the sweep adopting and was labelled current-behaviour rather than desired. The
// inversion belongs in the same change as the condition that causes it, so the
// diff reads as a rule changing rather than as a test being relaxed. Without
// that condition a durable off holds for launches and evaporates for the running
// pane at the next daemon restart, which is a rollback the operator believes
// worked.
func TestAPinnedDriveIsNotReacquiredByTheReconnectSweep(t *testing.T) {
	fixture := newCopilotAPIReconnectFixture(t)
	pinConversationDriveOff(t, fixture.convID)

	fixture.reconcile(t)

	assert.False(t, copilotAPISessions.Connected(fixture.convID),
		"an operator's explicit off must stand the sweep down; re-adopting it puts "+
			"the conversation back on the drive it was taken off, with no disclosure "+
			"anywhere")
	assert.False(t, copilotAPIDriven(fixture.convID),
		"and so the pinned conversation stays on the keystroke path its record asks for")

	target, err := db.CopilotDriveTargetForConv(fixture.convID)
	require.NoError(t, err)
	assert.False(t, target.Value, "the pin itself is untouched by the sweep")
}

// TestAnUnrecordedDriveIsStillAdoptedAfterThePinCondition is the other half of
// the same condition, and the one that keeps it honest.
//
// The skip is scoped to an explicit false. A conversation whose posture was
// never recorded means UNKNOWN, its mail routes to keystrokes, and adopting it
// closes that sink — the reason the sweep declines to filter on posture in the
// first place. If this arm ever goes red, the change stopped being "respect an
// operator" and became "stop closing the sink".
//
// TestCopilotAPIReconnectStillAdoptsAConversationWithNoRecordedPosture asserts
// the same property from the reconnect side; this one exists beside the pin so
// the boundary is visible to whoever edits the condition.
func TestAnUnrecordedDriveIsStillAdoptedAfterThePinCondition(t *testing.T) {
	fixture := newCopilotAPIReconnectFixtureWithPosture(t, false)
	require.False(t, copilotAPIPostureRecorded(fixture.convID),
		"precondition: nothing may answer which drive this launch took")
	target, err := db.CopilotDriveTargetForConv(fixture.convID)
	require.NoError(t, err)
	require.Equal(t, db.CopilotDriveRecordNone, target.Record,
		"precondition: unknown, not an operator's false")

	fixture.reconcile(t)

	assert.True(t, copilotAPISessions.Connected(fixture.convID),
		"an UNRECORDED posture is not an operator's off and must still be adopted")
}
