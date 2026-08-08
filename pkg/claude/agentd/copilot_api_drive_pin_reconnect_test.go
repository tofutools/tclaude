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

// TestAPinnedDriveIsReacquiredByTheReconnectSweep measures the lifetime of a
// rollback performed against a still-running pane.
//
// The state is the ordinary one after a durable "off": the operator pinned the
// record, the pane kept running (a pin does not stop anything), and agentd was
// later restarted. The sweep then finds a recorded port and a live Copilot pane
// — its two candidate conditions — and the operator's explicit false is not
// among the things it looks at.
func TestAPinnedDriveIsReacquiredByTheReconnectSweep(t *testing.T) {
	fixture := newCopilotAPIReconnectFixture(t)
	pinConversationDriveOff(t, fixture.convID)

	fixture.reconcile(t)

	adopted := copilotAPISessions.Connected(fixture.convID)
	driven := copilotAPIDriven(fixture.convID)
	t.Logf("MEASURED: pinned copilot_api=false + recorded port + live pane → "+
		"reconnect adopted=%v, routing back on the API drive=%v", adopted, driven)

	assert.True(t, adopted,
		"MEASURED, and recorded as the current behaviour rather than as the desired "+
			"one: the sweep's candidate test is a recorded port and a live pane, so an "+
			"operator's explicit off is re-acquired at the next daemon restart while the "+
			"pane it was pinned for is still running")
	assert.True(t, driven,
		"and once a handle exists it answers before the record, so the pinned "+
			"conversation is back on the drive it was taken off")

	// The durable record is untouched by the adoption — which is why the next
	// LAUNCH still honours the pin, and why this is a bounded lifetime rather
	// than a lost write.
	target, err := db.CopilotDriveTargetForConv(fixture.convID)
	require.NoError(t, err)
	assert.False(t, target.Value,
		"the pin itself survives; what does not survive is its effect on the pane "+
			"that was already running")
}
