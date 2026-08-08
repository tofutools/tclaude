package agentd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The observation a failed bootstrap leaves behind (TCL-1089).
//
// The state under test is the one an operator meets as "the agent is spawned,
// its pane is alive and perfectly typeable, and it receives no mail ever". The
// hold that produces it is correct — TCL-1058's rule, and its retry is real —
// but for a launch whose bootstrap never completed the retry can never fire,
// because no handle will ever appear for it. What was missing was not the hold
// but any statement that it had become permanent.

// failingBootstrap makes the bootstrap seam fail and returns a channel that
// closes once it has been called, so a test can wait for the goroutine rather
// than sleep past it.
func failingBootstrap(t *testing.T) <-chan struct{} {
	t.Helper()
	called := make(chan struct{})
	previous := bootstrapCopilotAPISessionFn
	bootstrapCopilotAPISessionFn = func(
		context.Context, string, copilotAPILaunchKind, string,
	) (*copilotAPISession, error) {
		close(called)
		return nil, errors.New("the pane never bound its port")
	}
	t.Cleanup(func() { bootstrapCopilotAPISessionFn = previous })
	return called
}

func awaitChannelFailed(t *testing.T, convID string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if copilotAPISessions.ChannelFailed(convID) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return copilotAPISessions.ChannelFailed(convID)
}

// The headline, asserted as a TRANSITION rather than a level.
//
// "ChannelFailed is true" on its own is satisfied by a registry that answers
// true for everything, and the before-state is what separates the fix from
// that. It is also the fact the surface renders, so a level-only assertion here
// would leave the dashboard's third state resting on nothing.
func TestCopilotAPIBootstrapFailureIsObservedAgainstTheLaunchThatFailed(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	copilotAPISessions.Drop(fixture.convID)

	require.False(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"nothing has been observed yet, and 'nothing observed' must not read as 'failed' — "+
			"that is the whole bootstrap window and every agent awaiting a reconcile")

	called := failingBootstrap(t)
	generation := copilotAPISessions.NoteLaunch(fixture.convID)
	runCopilotAPIBootstrap(fixture.convID, true, copilotAPILaunchFresh, "", generation)
	<-called

	assert.True(t, awaitChannelFailed(t, fixture.convID),
		"a bootstrap that returned an error has finished its whole bounded attempt and "+
			"nothing re-runs it, so this launch is deaf for good and something has to say so")
}

// The property a future author is most likely to break while "finishing" this
// ticket: the observation must not become the agent's drive.
//
// This is the ticket's own leading candidate, and it was killed by measurement
// rather than by taste — a revoke written through the existing posture write is
// INERT for a spawned agent, because copilotLaunchIntentForConv reads the stable
// agent relaunch profile first and only consults the conversation fallback for
// fields that profile left nil. Every Copilot spawn freezes CopilotAPI there.
//
// So the assertion is not squeamishness about writes. A revoke that DID land
// would overwrite an agent's birth intent with an observation about one launch,
// permanently, for every later relaunch — turning a per-agent operator toggle
// into one decided by whether a host was loaded that minute.
func TestAFailedBootstrapLeavesTheAgentsRecordedDriveAlone(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	haveCopilotAPILaunchIntent(t, fixture.agentID)
	copilotAPISessions.Drop(fixture.convID)

	called := failingBootstrap(t)
	generation := copilotAPISessions.NoteLaunch(fixture.convID)
	runCopilotAPIBootstrap(fixture.convID, true, copilotAPILaunchFresh, "", generation)
	<-called
	require.True(t, awaitChannelFailed(t, fixture.convID), "precondition: the failure was observed")

	assert.True(t, copilotAPIDriven(fixture.convID),
		"the agent chose the API drive and a bootstrap that lost a race with a loaded "+
			"host has not un-chosen it; routing still BELONGS to the API channel, which "+
			"is what keeps the delivery held rather than typed into the pane")

	profile, err := db.AgentRelaunchProfileForConv(fixture.convID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.CopilotAPI,
		"the spawn froze this, and an observation must not have cleared it")
	assert.True(t, *profile.CopilotAPI,
		"the agent's frozen intent is what the next relaunch replays, and it must still "+
			"say API — this is the record TCL-1082's manual un-choose owns, and an "+
			"observation about one launch may not write it")
}

// The race, and the claim under test is ORDERING rather than unlikelihood.
//
// Relaunching is what an operator does about an agent that has gone deaf, so the
// window in which a dying bootstrap could libel its successor is exactly the
// window in which a successor is most likely to exist. The compare and the write
// live in one critical section inside the registry, so a stale generation cannot
// land rather than usually not landing.
//
// The positive control is the second half: without it this passes just as well
// against a registry that records nothing at all.
func TestAFailedLaunchCannotSpeakForTheRelaunchThatReplacedIt(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	copilotAPISessions.Drop(fixture.convID)

	failed := copilotAPISessions.NoteLaunch(fixture.convID)
	// The operator sees a deaf agent and relaunches it. The first launch's
	// bootstrap goroutine is still alive, still inside its budget.
	current := copilotAPISessions.NoteLaunch(fixture.convID)
	require.NotEqual(t, failed, current, "a relaunch is a new launch generation")

	assert.False(t, copilotAPISessions.NoteChannelFailed(fixture.convID, failed),
		"the older launch's observation is about a launch nobody is asking after")
	assert.False(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"and it must not be readable as a fact about the healthy relaunch, which would "+
			"make the operator's own recovery action appear to work and then fail again")

	assert.True(t, copilotAPISessions.NoteChannelFailed(fixture.convID, current),
		"POSITIVE CONTROL: the current launch's own observation still lands, so the "+
			"assertions above are about generation-keying rather than about a recorder "+
			"that never records")
	assert.True(t, copilotAPISessions.ChannelFailed(fixture.convID))
}

// A relaunch supersedes an observation even when it arrives after one has been
// taken, which is the other order the same hazard can happen in.
func TestARelaunchClearsAnEarlierLaunchsObservation(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	copilotAPISessions.Drop(fixture.convID)

	generation := copilotAPISessions.NoteLaunch(fixture.convID)
	require.True(t, copilotAPISessions.NoteChannelFailed(fixture.convID, generation))
	require.True(t, copilotAPISessions.ChannelFailed(fixture.convID), "precondition: deaf")

	copilotAPISessions.NoteLaunch(fixture.convID)

	assert.False(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"the relaunch has its own bootstrap and will answer the question again; leaving "+
			"the old answer standing would render a fresh agent as deaf before its "+
			"channel has had any chance to come up")
}

// A live handle outranks an observation, with no rule anywhere to clear it.
//
// This is what covers the case an error return does not distinguish: a bootstrap
// that died at setForeground or at the launch prompt had already created the
// session, so a later daemon's reconcile can legitimately adopt it.
func TestALiveHandleOutranksAFailureObservation(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	// The fixture adopts a live handle; take the observation anyway.
	generation := copilotAPISessions.NoteLaunch(fixture.convID)
	require.True(t, copilotAPISessions.NoteChannelFailed(fixture.convID, generation))

	assert.False(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"a conversation with a live handle is not deaf, whatever was observed earlier")

	copilotAPISessions.Drop(fixture.convID)
	assert.True(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"POSITIVE CONTROL: and the observation is still there once the handle is gone, "+
			"so the assertion above is about the handle outranking it rather than about "+
			"the observation having been lost")
}

// The reconcile's two non-adopting exits mean opposite things, and only one of
// them is entitled to record.
//
// A candidate whose bounded attempt RAN and failed is known to have no channel
// coming. A candidate that never got a slot was never looked at — reporting it
// as deaf would be reporting an agent's state on the evidence that the sweep ran
// out of time examining it. The natural phrasing at the call site, "this
// candidate did not end up connected", is true of both, which is why the outcome
// is a named value rather than a boolean.
func TestOnlyAnExaminedReconcileCandidateIsObservedAsDeaf(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	copilotAPISessions.Drop(fixture.convID)
	generation := copilotAPISessions.NoteLaunch(fixture.convID)

	reconcileOutcome(fixture.convID, generation, copilotAPIReconcileUnexamined)
	assert.False(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"un-examined is an absence of evidence, not evidence of absence")

	reconcileOutcome(fixture.convID, generation, copilotAPIReconcileReconnected)
	assert.False(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"and a conversation that got its channel back has nothing to observe")

	reconcileOutcome(fixture.convID, generation, copilotAPIReconcileChannelUnavailable)
	assert.True(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"POSITIVE CONTROL: the examined-and-failed outcome does record, so the two "+
			"assertions above are about which outcome is entitled rather than about a "+
			"recorder that is wired to nothing")
}

// The reconcile latches its generation BEFORE it queues for a slot, so a launch
// arriving mid-sweep takes the conversation off it — the same rule AdoptIfAbsent
// already applies to the handle, applied to the observation.
func TestALaunchArrivingMidSweepTakesTheConversationOffTheReconcile(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	copilotAPISessions.Drop(fixture.convID)

	// Post-restart shape: the registry knows of no launch for this conversation,
	// so the sweep latches zero. That is a usable identity rather than a missing
	// one, and this is the test that says so.
	latched := copilotAPISessions.CurrentLaunch(fixture.convID)
	require.Equal(t, uint64(0), latched, "an agentd restart empties the registry")

	copilotAPISessions.NoteLaunch(fixture.convID)
	reconcileOutcome(fixture.convID, latched, copilotAPIReconcileChannelUnavailable)

	assert.False(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"the sweep's conclusion is about the state it found at the top; a launch that "+
			"has since started owns the conversation and its own bootstrap answers")
}
