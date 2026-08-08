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
// running the channel it was launched with. The sweep treats it like any other
// candidate, deliberately — see the decision on reconcileCopilotAPISessions —
// and these arms hold that decision in place.

// pinConversationDriveOff turns the drive off in the conversation fallback —
// the record a clone-shaped or legacy conversation keeps its drive in, and the
// one the reconnect fixture writes.
func pinConversationDriveOff(t *testing.T, convID string) {
	t.Helper()
	target, err := db.CopilotDriveTargetForConv(convID)
	require.NoError(t, err)
	require.Equal(t, db.CopilotDriveRecordConversationFallback, target.Record)
	require.True(t, target.Value, "the scenario needs a drive recorded ON")
	ok, err := db.CompareAndSetConversationCopilotAPI(convID, false, "explicit", target.Raw)
	require.NoError(t, err)
	require.True(t, ok, "the pin's compare-and-set must hold")
	require.False(t, copilotAPIDriven(convID),
		"precondition: with no handle, the pinned record must already route to send-keys")
}

// TestAPinnedDriveIsReacquiredByTheReconnectSweep records a DECISION, not an
// observation, and that is why it survived being written twice.
//
// It first went in measuring the sweep adopting a pinned conversation, labelled
// current-behaviour-not-desired; it was then inverted to assert a skip; and it
// is inverted back here because the skip was wrong in principle. What it now
// pins is the reasoning that settled it:
//
// AN AGENTD RESTART IS NOT THE CHANNEL ENDING. The pane persists, the copilot
// process persists, the RPC listener persists; only agentd's in-memory handle is
// lost, and the sweep is the mechanism that remembers. A pin governs the next
// LAUNCH — it does not evict a live channel, and a restart is not an eviction.
//
// The flip is asserted on BOTH sides on purpose, because the pair IS the
// decision: with no handle the pinned record answers and the conversation routes
// to send-keys; once the sweep re-adopts, the handle answers first and it is back
// on its channel. Asserting only the adopt would record that something happened.
// Asserting the flip records why it is allowed to.
func TestAPinnedDriveIsReacquiredByTheReconnectSweep(t *testing.T) {
	fixture := newCopilotAPIReconnectFixture(t)
	// pinConversationDriveOff asserts the first half: no handle, so the pinned
	// record decides and the conversation is on keystrokes.
	pinConversationDriveOff(t, fixture.convID)

	fixture.reconcile(t)

	assert.True(t, copilotAPISessions.Connected(fixture.convID),
		"a pin governs the next launch; it must not stand the sweep down for a pane "+
			"whose channel never ended")
	assert.True(t, copilotAPIDriven(fixture.convID),
		"and with the handle back, the live channel answers before the record — the "+
			"other half of the flip, and the reason the pin is not lost: it still "+
			"decides the next launch")

	target, err := db.CopilotDriveTargetForConv(fixture.convID)
	require.NoError(t, err)
	assert.False(t, target.Value,
		"the pin itself is untouched by the sweep, so the next LAUNCH still honours it")
}

// TestAnUnrecordedDriveIsStillAdoptedByTheSweep guards the sink-closing property
// that made the sweep decline to filter on posture in the first place.
//
// A conversation whose posture was never recorded means UNKNOWN, its mail routes
// to keystrokes, and adopting it closes that sink rather than merely observing
// the conversation. Kept beside the pin arm so the two states — "nobody said" and
// "the operator said no" — stay visibly distinct to whoever edits this sweep,
// even though neither now changes what it does.
func TestAnUnrecordedDriveIsStillAdoptedByTheSweep(t *testing.T) {
	fixture := newCopilotAPIReconnectFixtureWithPosture(t, false)
	require.False(t, copilotAPIPostureRecorded(fixture.convID),
		"precondition: nothing may answer which drive this launch took")
	target, err := db.CopilotDriveTargetForConv(fixture.convID)
	require.NoError(t, err)
	require.Equal(t, db.CopilotDriveRecordNone, target.Record,
		"precondition: unknown, not an operator's false")

	fixture.reconcile(t)

	assert.True(t, copilotAPISessions.Connected(fixture.convID),
		"an UNRECORDED posture is the case this sweep exists for")
}
