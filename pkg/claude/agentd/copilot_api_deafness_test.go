package agentd

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync/atomic"
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
	copilotAPISessions.dropHandleForTest(fixture.convID)

	require.False(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"nothing has been observed yet, and 'nothing observed' must not read as 'failed' — "+
			"that is the whole bootstrap window and every agent awaiting a reconcile")

	called := failingBootstrap(t)
	generation := copilotAPISessions.NoteLaunch(fixture.convID, true)
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
	copilotAPISessions.dropHandleForTest(fixture.convID)

	called := failingBootstrap(t)
	generation := copilotAPISessions.NoteLaunch(fixture.convID, true)
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
	copilotAPISessions.dropHandleForTest(fixture.convID)

	failed := copilotAPISessions.NoteLaunch(fixture.convID, true)
	// The operator sees a deaf agent and relaunches it. The first launch's
	// bootstrap goroutine is still alive, still inside its budget.
	current := copilotAPISessions.NoteLaunch(fixture.convID, true)
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
//
// # What enforces this, which is NOT what it looks like
//
// The generation compare in ChannelFailed, not the delete in NoteLaunch. A
// mutation pass removed that delete and this test stayed green, along with the
// rest of the suite — so the delete is hygiene and the compare is the mechanism.
// Recorded here rather than quietly fixed because the test's NAME points at the
// delete, and the next person to read the two together would otherwise conclude
// the compare was redundant and delete the wrong one.
func TestARelaunchClearsAnEarlierLaunchsObservation(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	copilotAPISessions.dropHandleForTest(fixture.convID)

	generation := copilotAPISessions.NoteLaunch(fixture.convID, true)
	require.True(t, copilotAPISessions.NoteChannelFailed(fixture.convID, generation))
	require.True(t, copilotAPISessions.ChannelFailed(fixture.convID), "precondition: deaf")

	copilotAPISessions.NoteLaunch(fixture.convID, true)

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
	generation := copilotAPISessions.NoteLaunch(fixture.convID, true)
	require.True(t, copilotAPISessions.NoteChannelFailed(fixture.convID, generation))

	assert.False(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"a conversation with a live handle is not deaf, whatever was observed earlier")

	copilotAPISessions.dropHandleForTest(fixture.convID)
	assert.True(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"POSITIVE CONTROL: and the observation is still there once the handle is gone, "+
			"so the assertion above is about the handle outranking it rather than about "+
			"the observation having been lost")
}

// The deliverable: the fact reaches the surface an operator actually looks at.
//
// Exercised through the real projection rather than by asserting the predicate
// twice. The predicate having the right answer is worth nothing if the snapshot
// never asks it, and "the dashboard renders deafness" is the entire read-only
// half of this ticket — so it needs an assertion of its own rather than being
// implied by the ones above.
//
// Asserted as a transition across the three states the chip distinguishes,
// because the middle one is the reason the flag exists: an agent that is merely
// starting up must NOT be reported as deaf, or the surface flags the fleet as
// broken at exactly the moments it is working.
func TestTheDashboardDistinguishesAStartingAgentFromADeafOne(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(copilotAPISessions.ForgetLaunchesForTest)

	sess := copilotUsageSession(t, "s-copilot-deaf", "conv-deaf")
	agentID, _, err := db.EnsureAgentForConv(sess.ConvID, "spawn")
	require.NoError(t, err)
	haveCopilotAPILaunchIntent(t, agentID)

	snapshot := func() agentState {
		return stateForConvInSessionsBatched(
			[]*db.SessionRow{sess}, map[string]struct{}{sess.TmuxSession: {}}, nil, nil, nil)
	}

	// Still starting: on the drive, no connection, nothing observed. This is the
	// state the pair copilot_api && !connected cannot tell from deafness, and it
	// is the common one.
	starting := snapshot()
	require.True(t, starting.CopilotAPI, "precondition: the launch took the drive")
	require.False(t, starting.CopilotAPIConnected, "precondition: no handle yet")
	assert.False(t, starting.CopilotAPIChannelFailed,
		"an agent whose bootstrap is still running is not deaf, and reporting it as "+
			"deaf would fire on every healthy API spawn for the length of its bootstrap")

	generation := copilotAPISessions.NoteLaunch(sess.ConvID, true)
	require.True(t, copilotAPISessions.NoteChannelFailed(sess.ConvID, generation))

	deaf := snapshot()
	assert.True(t, deaf.CopilotAPIChannelFailed,
		"once the daemon has watched the channel fail, the surface has to say so — "+
			"this is the only thing that distinguishes a deaf agent from a busy one")
	assert.True(t, deaf.CopilotAPI,
		"and it still reports the API drive, because the agent's choice has not changed")
}


// ---------------------------------------------------------------------------
// The PATHS, not the helpers
// ---------------------------------------------------------------------------
//
// A cold review deleted the sweep's recording call and rewrote the bootstrap's
// generation to be read at failure time — reintroducing the exact race this
// change is named for — and the WHOLE agentd package stayed green. Every test
// above drives the registry or the recorder directly, so each asserts about a
// helper and none about a path.
//
// That is this series' own vacuously-green shape, and the fix is not more
// assertions on the helpers. It is to run the thing.

// The sweep records. Mutation-proven: deleting recordReconcileAttempt from the
// sweep's error exit turns this red.
func TestTheStartupSweepRecordsAFailedReconnect(t *testing.T) {
	fixture := newCopilotAPIReconnectFixture(t)
	t.Cleanup(copilotAPISessions.ForgetLaunchesForTest)

	original := reconnectCopilotAPISessionFn
	reconnectCopilotAPISessionFn = func(context.Context, string) (*copilotAPISession, error) {
		return nil, errors.New("the server has no drivable session under that id")
	}
	t.Cleanup(func() { reconnectCopilotAPISessionFn = original })

	require.False(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"precondition: nothing observed before the sweep")

	fixture.reconcile(t)

	assert.True(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"a candidate whose bounded attempt ran and failed is deaf for this daemon's "+
			"life, and the sweep is the only thing that will ever know it — nothing "+
			"re-runs it, and the launch that would have is long gone")
}

// The sweep stands down for a conversation whose own bootstrap is in flight.
//
// This is the state at daemon startup, not a corner: a conversation is a
// candidate precisely because it has no handle, which is what a healthy launch
// looks like for the whole length of its bootstrap. Recording here shows the
// operator a red "relaunch it" chip for an agent whose channel is seconds away,
// and following that advice kills the working bootstrap.
func TestTheStartupSweepDoesNotLibelALaunchThatIsStillBootstrapping(t *testing.T) {
	fixture := newCopilotAPIReconnectFixture(t)
	t.Cleanup(copilotAPISessions.ForgetLaunchesForTest)

	original := reconnectCopilotAPISessionFn
	reconnectCopilotAPISessionFn = func(context.Context, string) (*copilotAPISession, error) {
		// What a reconnect against a still-starting pane really answers: the
		// launch has not created its session yet, so there is nothing drivable.
		return nil, errors.New("the server has no drivable session under that id")
	}
	t.Cleanup(func() { reconnectCopilotAPISessionFn = original })

	// The launch arrived BEFORE the sweep latched, which is why a generation
	// comparison cannot catch this: the sweep and the launch hold the same one.
	copilotAPISessions.NoteLaunch(fixture.convID, true)

	fixture.reconcile(t)

	assert.False(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"the launch owns this conversation and its own bootstrap will answer, exactly, "+
			"on its own failure path; the sweep's fast failure against a half-started "+
			"pane means nothing")

	// POSITIVE CONTROL: the same sweep, the same stubbed failure, once the
	// bootstrap is no longer in flight. Without this the assertion above is
	// satisfied by a sweep that records nothing ever.
	copilotAPISessions.BootstrapFinished(fixture.convID,
		copilotAPISessions.CurrentLaunch(fixture.convID))
	fixture.reconcile(t)
	assert.True(t, copilotAPISessions.ChannelFailed(fixture.convID),
		"POSITIVE CONTROL: with no bootstrap in flight the same sweep does record")
}

// A sweep that was cut short did not examine anything, and must not say it did.
//
// Two shapes reach the error arm with a dead context: the sweep's deadline
// firing mid-attempt, and Go's select picking the slot arm when the slot and a
// cancelled context are both ready — a coin flip, not a rare interleaving. Both
// were reproduced by review, and before the ctx check they recorded identical
// agents as deaf or un-examined depending on queue position and a random pick.
//
// Asserted over repetitions on purpose: a single run passes ~50% of the time
// against the defect, which is worse than no test.
func TestACutShortSweepReportsNothingRatherThanGuessing(t *testing.T) {
	fixture := newCopilotAPIReconnectFixture(t)
	t.Cleanup(copilotAPISessions.ForgetLaunchesForTest)

	var attempts atomic.Int64
	original := reconnectCopilotAPISessionFn
	reconnectCopilotAPISessionFn = func(ctx context.Context, _ string) (*copilotAPISession, error) {
		attempts.Add(1)
		return nil, ctx.Err()
	}
	t.Cleanup(func() { reconnectCopilotAPISessionFn = original })

	for range 40 {
		copilotAPISessions.ForgetLaunchesForTest()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reconcileCopilotAPISessions(ctx)
		require.False(t, copilotAPISessions.ChannelFailed(fixture.convID),
			"a sweep that ran out of time did not examine this agent; reporting it as "+
				"deaf reports an agent's state on the evidence that we stopped looking")
	}
	// The runs where the select DID take the slot are the ones that matter — a
	// loop that never entered the attempt would pass against the defect too.
	assert.Positive(t, attempts.Load(),
		"POSITIVE CONTROL: at least one cancelled run must have reached the attempt, "+
			"or this loop proves nothing about the arm it is written for")
}

// The generation must TRAVEL with the launch, not be re-read when the bootstrap
// fails.
//
// Mutation-proven: replacing runCopilotAPIBootstrap's `generation` parameter
// with a CurrentLaunch(convID) read at failure time — the tidier-looking
// refactor the doc comments warn against — turns this red. Nothing else in the
// package notices, because every other test holds the generation itself.
func TestALateFailingBootstrapCannotStampTheRelaunchThatReplacedIt(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	copilotAPISessions.dropHandleForTest(fixture.convID)

	release := make(chan struct{})
	entered := make(chan struct{})
	previous := bootstrapCopilotAPISessionFn
	bootstrapCopilotAPISessionFn = func(
		context.Context, string, copilotAPILaunchKind, string,
	) (*copilotAPISession, error) {
		close(entered)
		<-release
		return nil, errors.New("the pane never bound its port")
	}
	t.Cleanup(func() { bootstrapCopilotAPISessionFn = previous })

	doomed := copilotAPISessions.NoteLaunch(fixture.convID, true)
	runCopilotAPIBootstrap(fixture.convID, true, copilotAPILaunchFresh, "", doomed)
	<-entered

	// The operator sees a deaf agent and relaunches it, while the first launch's
	// bootstrap is still inside its budget. This is the ordinary recovery action,
	// which is what makes the window worth defending.
	current := copilotAPISessions.NoteLaunch(fixture.convID, true)
	require.NotEqual(t, doomed, current)

	close(release)

	assert.False(t, awaitChannelFailed(t, fixture.convID),
		"the dying launch must not stamp its healthy successor; the operator's own "+
			"recovery would appear to work and then the agent would read as deaf again")
}

// The blind spot a cold review demonstrated: a NEW recording site at an exit
// where no attempt ran.
//
// The first version of this seam used a named outcome, on the argument that
// "the candidate did not end up connected" was then not sayable at the call
// site. A reviewer beat it in minutes with a change nobody would blink at — a
// liveness re-check after acquiring a slot, reporting the existing
// "channel unavailable" name — and every check stayed green. The guard bounded
// how many NAMES existed, not which exits were entitled to use them.
//
// Taking the attempt's own error as a parameter makes that misuse visible
// rather than impossible: a caller with no attempt has no error to pass, and
// fabricating one is something a reader sees. This guard is the other half, and
// it is deliberately the containment one — the demonstrated failure was an
// extra call site, so containment is the property that actually broke rather
// than the one that was easy to express.
//
// # What this does NOT cover, stated so it is not mistaken for a certificate
//
// It cannot tell whether the single permitted call site is still in the right
// place. Moving it above the attempt, or into the slot-starvation arm, keeps
// the count at one and this guard green. The ctx and bootstrap-in-flight checks
// inside recordReconcileAttempt are what defend that, and they have their own
// tests.
func TestOnlyTheSweepsAttemptExitMayRecordAnObservation(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	callers := map[string]int{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		require.NoError(t, err)
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, name, source, 0)
		require.NoError(t, err)

		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok &&
				ident.Name == "recordReconcileAttempt" {
				callers[name]++
			}
			return true
		})
	}

	// The positive control. Without it a rename of the function leaves every
	// assertion below trivially satisfied, which is how a guard becomes a
	// certificate.
	require.Equal(t, 1, callers["copilot_api_reconnect.go"],
		"recordReconcileAttempt must be called exactly once, from the sweep's attempt "+
			"exit in copilot_api_reconnect.go. If it moved, this guard is now watching "+
			"nothing")
	assert.Len(t, callers, 1,
		"a second call site means a new place that can declare an agent deaf. If the "+
			"new site genuinely ran a bounded attempt, widen this guard deliberately "+
			"and say why; if it did not, it is reporting an agent's state on the "+
			"evidence that we stopped looking. Callers found: %v", callers)

	// And the primitive itself, which is what the sentence above was claiming.
	//
	// Counting recordReconcileAttempt alone was never the invariant: it is one of
	// TWO ways to record, and the other is calling NoteChannelFailed directly.
	// Measured under review — adding it to the sweep's slot-starvation arm, the
	// exact "un-examined reported as deaf" failure this guard's message names,
	// left the guard green. Matched as REFERENCES rather than calls for the same
	// reason the revoke guard is: taking the method as a value is not something
	// anyone does innocently.
	type site struct{ file, function string }
	allowed := map[site]bool{
		// The launch's own bootstrap, on its failure path, where the answer is exact.
		{"copilot_api_bootstrap.go", "runCopilotAPIBootstrap"}: true,
		// The sweep, through the entitlement check.
		{"copilot_api_reconnect.go", "recordReconcileAttempt"}: true,
	}
	var recorders []site
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		require.NoError(t, err)
		parsed, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
		require.NoError(t, err)
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if selector, ok := node.(*ast.SelectorExpr); ok &&
					selector.Sel.Name == "NoteChannelFailed" {
					recorders = append(recorders, site{name, fn.Name.Name})
					return false
				}
				return true
			})
		}
	}
	for _, found := range recorders {
		assert.Truef(t, allowed[found],
			"%s references NoteChannelFailed in %s, which is a THIRD way to declare "+
				"an agent deaf. The two allowed sites each ran a bounded attempt and "+
				"are entitled to say what it found; a new one has to make that "+
				"argument rather than inherit it",
			found.function, found.file)
	}
	for want := range allowed {
		assert.Containsf(t, recorders, want,
			"%s no longer records in %s; this guard is watching nothing",
			want.function, want.file)
	}

	// A call-site allow-list cannot see what an allow-listed site DOES, and for
	// recordReconcileAttempt that is the whole invariant: its three disqualifying
	// checks each return early, so the record is legitimate only as the function's
	// final UNCONDITIONAL statement. A record placed inside one of those early-exit
	// branches — the slot-starvation arm, say — is the exact "un-examined reported
	// as deaf" failure this guard is named for, and it is allow-listed by file and
	// function. Measured: the allow-list alone stayed green on it.
	//
	// So the shape is pinned as well as the site. Exactly one reference, and it
	// must be unconditional.
	source, err := os.ReadFile("copilot_api_reconnect.go")
	require.NoError(t, err)
	parsed, err := parser.ParseFile(token.NewFileSet(), "copilot_api_reconnect.go", source, 0)
	require.NoError(t, err)
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Name.Name != "recordReconcileAttempt" {
			continue
		}
		var references int
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok &&
				selector.Sel.Name == "NoteChannelFailed" {
				references++
				return false
			}
			return true
		})
		assert.Equal(t, 1, references,
			"recordReconcileAttempt must record in exactly one place. A second "+
				"record inside one of its early-exit branches declares an agent deaf "+
				"on an attempt that was disqualified — cancelled, cut short, or racing "+
				"the launch's own bootstrap — which is precisely what those branches "+
				"exist to refuse")
		assert.True(t,
			callAlwaysEvaluated(fn.Body, "copilotAPISessions.NoteChannelFailed"),
			"recordReconcileAttempt's record must be UNCONDITIONAL — the last thing "+
				"it does once every disqualifier has returned. Inside a branch it is "+
				"reporting an agent's state on the evidence that we stopped looking")
	}
}

// copilotAPIObservationReaders names every place allowed to read the
// channel-failed observation, and the reader it is allowed to use.
//
// Two entries. The accessor itself, which only delegates to the registry, and
// the dashboard state builder, which renders it. Nothing that ROUTES is here,
// and that is the entire content of the list.
var copilotAPIObservationReaders = []struct{ file, function, reader string }{
	{"copilot_api_drive.go", "copilotAPIChannelFailed", "ChannelFailed"},
	{"dashboard.go", "stateForConvInSessionsBatched", "copilotAPIChannelFailed"},
}

// TestTheChannelFailedObservationNeverReachesARoutingDecision is the
// routing-side counterpart to TestNoAutomaticRevokeOfTheCopilotAPIPosture.
//
// # The hole it closes
//
// That guard makes "nothing WRITES a revoking record" structural, and it was
// shipped with an admission: a revoke that writes no record at all, and instead
// teaches [copilotAPIDriven] to consult this observation, is a revoke in every
// sense an operator cares about, and the write-side guard is green on it. Prose
// was all that refused it.
//
// That is not a hypothetical shape. It is the design that was explicitly
// considered and declined during the revoke decision — "route on the in-memory
// observation, no durable write" — and its most attractive property was exactly
// that it needs no record. So the alternative the group weighed and rejected is
// the one living in the write-side guard's blind spot. This closes it.
//
// # What it asserts
//
// UNIVERSALLY: every reference to the observation, by either of its two names,
// must be allow-listed above. Not "a legitimate reader exists" — that passes
// with an illegitimate one beside it.
//
// It matches REFERENCES rather than calls, so taking the accessor as a value
// does not slip past, for the same reason the write-side guard does.
//
// # What it cannot see
//
//   - A new predicate that reads the registry's failure state by some other
//     route, or a copy of the observation kept elsewhere. The list is by name.
//   - Whether an allow-listed reader ROUTES on what it read. The dashboard entry
//     is trusted to render rather than decide, and nothing here enforces that.
//   - Anything outside agentd's top-level directory.
func TestTheChannelFailedObservationNeverReachesARoutingDecision(t *testing.T) {
	watched := map[string]bool{}
	for _, site := range copilotAPIObservationReaders {
		watched[site.reader] = true
	}

	type reference struct{ file, function, reader string }
	var found []reference

	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		require.NoError(t, err)
		parsed, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
		require.NoError(t, err)

		record := func(function string, node ast.Node) {
			ast.Inspect(node, func(n ast.Node) bool {
				switch typed := n.(type) {
				case *ast.SelectorExpr:
					if watched[typed.Sel.Name] {
						found = append(found, reference{name, function, typed.Sel.Name})
						return false
					}
				case *ast.Ident:
					if watched[typed.Name] {
						found = append(found, reference{name, function, typed.Name})
					}
				}
				return true
			})
		}
		for _, decl := range parsed.Decls {
			switch typed := decl.(type) {
			case *ast.FuncDecl:
				if typed.Body != nil {
					record(typed.Name.Name, typed.Body)
				}
			case *ast.GenDecl:
				// Package-level vars and consts. Omitting these is how the
				// write-side revoke guard was beaten, and this guard — written as
				// its counterpart — re-imported the same hole. Measured under
				// review: a package-level `var copilotAPIDrivenNow = func(...)
				// { return copilotAPIDriven(c) && !copilotAPIChannelFailed(c) }`
				// with a caller routed onto it left this guard green, the
				// write-side guard green, and the whole behavioural set green —
				// which is the declined design shipping undetected.
				record(packageLevel, typed)
			}
		}
	}

	allowed := map[reference]bool{}
	for _, site := range copilotAPIObservationReaders {
		allowed[reference{site.file, site.function, site.reader}] = true
	}

	for _, site := range found {
		assert.Truef(t, allowed[site],
			"%s reads %s in %s. This observation must never reach a routing "+
				"decision: a false means \"nothing observed\", not \"healthy\", and "+
				"routing on a true is the injection sink re-opening for exactly the "+
				"agent whose channel is in trouble. That design was considered and "+
				"declined — see copilotAPIChannelFailed. Route on copilotAPIDriven; "+
				"ask copilotAPIConnected whether the channel is up",
			site.function, site.reader, site.file)
	}

	for _, want := range copilotAPIObservationReaders {
		site := reference{want.file, want.function, want.reader}
		assert.Containsf(t, found, site,
			"%s no longer reads %s in %s; this guard is now watching nothing",
			want.function, want.reader, want.file)
	}
}
