package agentd

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// These tests are all one assertion in two halves: the typed call was made,
// AND no keystroke was typed. Either half alone passes against a
// re-implementation of the bug this ticket exists to remove — a delivery that
// goes out over RPC and ALSO gets typed in, or one that reports the API and
// quietly falls back to the pane.

// copilotAPIDriveFixture stands up an API-connected Copilot conversation: a
// live session row whose pane the tmux stub reports as alive, a real client on
// a fake server, and a registry handle whose port this test process genuinely
// owns so the ownership re-proof passes for real rather than being stubbed out.
type copilotAPIDriveFixture struct {
	convID  string
	server  *fakeCopilotServer
	tmux    *commandRecordingTmux
	agentID string
}

func newCopilotAPIDriveFixture(t *testing.T) *copilotAPIDriveFixture {
	t.Helper()
	setupTestDB(t)
	t.Cleanup(SetInjectSettleDelayForTest(0))

	tmux := &commandRecordingTmux{}
	previous := clcommon.Default
	clcommon.Default = tmux
	t.Cleanup(func() { clcommon.Default = previous })

	const (
		convID    = "ses_copilot_api_drive"
		sessionID = "spwn-copilot-api-drive"
	)
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: sessionID, TmuxSession: sessionID, ConvID: convID,
		Harness: harness.CopilotName, Status: session.StatusIdle,
		Cwd: t.TempDir(),
	}))

	server := newFakeCopilotServer(t)
	client := dialFakeCopilot(t, server)
	copilotAPISessions.Adopt(&copilotAPISession{
		ConvID: convID, SessionID: "copilot-session-1",
		// The proof reads the kernel's tables, so the pane pid has to be a
		// process that really does own the listener: this test binary does.
		Port: server.port(), PanePID: os.Getpid(), Client: client,
	})
	t.Cleanup(func() { copilotAPISessions.Drop(convID) })
	// Launch generations and failure observations are process-wide too, and
	// unlike handles nothing else drops them — so one test's generation would be
	// the next one's starting point, and a failure observed here would stay
	// readable by a test that never launched anything.
	t.Cleanup(copilotAPISessions.ForgetLaunchesForTest)

	return &copilotAPIDriveFixture{
		convID: convID, server: server, tmux: tmux, agentID: agentID,
	}
}

// assertNoKeystrokes is the half that is easy to forget. A delivery that went
// out over RPC and ALSO typed into the pane would satisfy every "was the call
// made" assertion while leaving the injection sink exactly where it was.
func (f *copilotAPIDriveFixture) assertNoKeystrokes(t *testing.T) {
	t.Helper()
	for _, command := range f.tmux.snapshot() {
		require.NotEmpty(t, command)
		assert.NotContains(t, []string{"send-keys", "set-buffer", "paste-buffer"}, command[0],
			"an API-connected agent must not be typed into: %v", command)
	}
}

// The message half of the injection sink. An inbox nudge carries another
// agent's subject and body, which is the most caller-controlled text tclaude
// ever puts near a pane's input stream.
func TestCopilotAPINudgeGoesOverSessionSendAndNotThroughThePane(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)

	group, err := db.CreateAgentGroup("copilot-api", "")
	require.NoError(t, err)
	messageID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: group, FromConv: "peer", ToConv: fixture.convID, Body: "hello",
	})
	require.NoError(t, err)
	message, err := db.GetAgentMessage(messageID)
	require.NoError(t, err)

	const nudge = "[msg #1 from peer] hello"
	require.True(t, sendNudgeBracket(fixture.convID, message, nudge))

	assert.Contains(t, fixture.server.methodsCalled(), copilotapi.MethodSessionSend)
	var sent copilotapi.SendParams
	require.NoError(t, json.Unmarshal(fixture.server.paramsFor(copilotapi.MethodSessionSend), &sent))
	assert.Equal(t, nudge, sent.Prompt)
	assert.Equal(t, "copilot-session-1", sent.SessionID)
	assert.Empty(t, sent.Mode,
		"the default (enqueue) lane is the one that does not overtake the human's own "+
			"queued input; `immediate` does not interrupt a turn, it only jumps the queue")

	fixture.assertNoKeystrokes(t)
}

// A failing typed call must NOT be rescued by typing the message in. The
// durable inbox row is the retry, and returning it to the keystroke path would
// hand an agent that opted out of the injection sink exactly the delivery it
// opted out of.
func TestCopilotAPINudgeFailureDoesNotFallBackToKeystrokes(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	fixture.server.failMethod(copilotapi.MethodSessionSend, "server said no")

	group, err := db.CreateAgentGroup("copilot-api", "")
	require.NoError(t, err)
	messageID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: group, FromConv: "peer", ToConv: fixture.convID, Body: "hello",
	})
	require.NoError(t, err)
	message, err := db.GetAgentMessage(messageID)
	require.NoError(t, err)

	assert.False(t, sendNudgeBracket(fixture.convID, message, "[msg #1 from peer] hello"),
		"a failed typed delivery is a retryable failure, not a reason to type it in")
	fixture.assertNoKeystrokes(t)
}

// Rename is the other caller-controlled-text sink, and session.name.set is not
// merely the nearest RPC: it writes the same workspace.yaml `name` that
// copilotConvStore.Title reads.
func TestCopilotAPIRenameGoesOverSessionNameSet(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)

	require.True(t, deliverRename(fixture.convID, "new title"))

	assert.Contains(t, fixture.server.methodsCalled(), copilotapi.MethodSessionNameSet)
	var params copilotapi.SetNameParams
	require.NoError(t, json.Unmarshal(
		fixture.server.paramsFor(copilotapi.MethodSessionNameSet), &params))
	assert.Equal(t, "new title", params.Name)
	assert.Equal(t, "copilot-session-1", params.SessionID)

	fixture.assertNoKeystrokes(t)

	// The cached title is the read path every dashboard surface uses before the
	// harness has surfaced the rename itself; an API rename must populate it
	// exactly as the injected one does.
	row, err := db.GetConvIndex(fixture.convID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "new title", row.CustomTitle)
}

// The send-keys charset gate is not relaxed for API-mode agents. The send-keys
// path is still Copilot's default and still needs it, and a title that renders
// differently depending on which transport a conversation happens to hold
// would be its own bug.
func TestCopilotAPIRenameStillRunsTheCharsetGate(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)

	assert.False(t, deliverRename(fixture.convID, "line one\nline two"))
	assert.NotContains(t, fixture.server.methodsCalled(), copilotapi.MethodSessionNameSet,
		"a title the keystroke gate rejects is not smuggled out over RPC instead")
	fixture.assertNoKeystrokes(t)
}

// Compaction runs in the background because it is a model turn, so the
// assertion has to wait for the call rather than expect it synchronously.
func TestCopilotAPICompactGoesOverHistoryCompactThenSendsTheFollowUp(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)

	transport, failure := dispatchSlashCommand(fixture.convID, "/compact", "carry on", "compact")
	assert.Equal(t, slashTransportCopilotAPI, transport)
	assert.Equal(t, slashFailureNone, failure)

	assert.Eventually(t, func() bool {
		methods := fixture.server.methodsCalled()
		var compacted, sent bool
		for _, method := range methods {
			switch method {
			case copilotapi.MethodSessionCompact:
				compacted = true
			case copilotapi.MethodSessionSend:
				// Ordered by construction: session.history.compact resolves only
				// once the new history is in place, which is the guarantee
				// send-keys cannot make and OpenCode refuses to fake.
				sent = compacted
			}
		}
		return compacted && sent
	}, 5*time.Second, 10*time.Millisecond,
		"compaction then the follow-up, in that order")

	var params copilotapi.CompactParams
	require.NoError(t, json.Unmarshal(
		fixture.server.paramsFor(copilotapi.MethodSessionCompact), &params))
	assert.Equal(t, copilotapi.CompactTriggerManual, params.Trigger)

	fixture.assertNoKeystrokes(t)
}

// A lifecycle token with no typed mapping fails closed. The alternative — let
// it through to the keystroke path — is the quiet re-introduction of the sink
// for every command nobody has mapped yet.
//
// The token here is deliberately one that could plausibly arrive at this sink
// later (a model switch) rather than /exit: soft exit does not come through
// dispatchSlashCommand at all, so using it would suggest this test pins the
// soft-exit routing decision, which lives in lifecycle.go and is pinned by a
// live test instead.
func TestCopilotAPIUnmappedLifecycleCommandFailsClosed(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)

	transport, failure := dispatchSlashCommand(fixture.convID, "/model gpt-5", "", "model")
	assert.Equal(t, slashTransportNone, transport)
	assert.Equal(t, slashFailureControl, failure,
		"a managed channel refused; it is not a missing pane")
	fixture.assertNoKeystrokes(t)
	assert.NotContains(t, fixture.server.methodsCalled(), copilotapi.MethodSessionCompact)
}

// The ownership re-proof stands in for the authentication this endpoint does
// not have, and it runs per call because the bootstrap's proof is one-shot.
//
// This is the arm that says an unverifiable channel is a FAILURE and not a
// downgrade. The first version of this seam had one predicate for both "which
// channel" and "may I send now", so a handle that failed the re-proof answered
// exactly like a conversation that had never taken the drive — and every caller
// then typed into the pane instead. That is the injection sink re-opening for
// the one agent whose channel just became unverifiable.
func TestCopilotAPIUnprovableOwnershipFailsRatherThanFallingBackToThePane(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)

	// A live, non-ancestor pid whose subtree genuinely excludes the listener.
	// pid 1 would be rejected before the comparison this test is named after.
	stranger := exec.Command("sleep", "60")
	require.NoError(t, stranger.Start())
	t.Cleanup(func() {
		_ = stranger.Process.Kill()
		_ = stranger.Wait()
	})
	handle := copilotAPISessions.Handle(fixture.convID)
	require.NotNil(t, handle)
	handle.PanePID = stranger.Process.Pid

	assert.True(t, copilotAPIDriven(fixture.convID),
		"the conversation still BELONGS to the API channel; that is not what "+
			"became unverifiable")
	_, err := copilotAPIDrive(fixture.convID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no longer be shown to belong")

	assert.False(t, deliverRename(fixture.convID, "new title"))
	transport, _ := dispatchSlashCommand(fixture.convID, "/compact", "", "compact")
	assert.Equal(t, slashTransportNone, transport)
	assert.NotContains(t, fixture.server.methodsCalled(), copilotapi.MethodSessionNameSet)
	fixture.assertNoKeystrokes(t)

	// Refused, not dropped: a transient failure of two kernel table reads must
	// not turn into a durable "this agent is disconnected".
	assert.True(t, copilotAPISessions.Connected(fixture.convID),
		"the connection is still there; only these calls were refused")
}

// A Copilot agent that did NOT take the API drive keeps the send-keys path
// byte-for-byte. The flag is opt-in, and this is the arm that says so.
func TestCopilotWithoutTheAPIDriveStillTakesSendKeys(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(SetInjectSettleDelayForTest(0))
	tmux := &commandRecordingTmux{}
	previous := clcommon.Default
	clcommon.Default = tmux
	t.Cleanup(func() { clcommon.Default = previous })

	const convID = "ses_copilot_send_keys"
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: "spwn-copilot-send-keys", TmuxSession: "spwn-copilot-send-keys", ConvID: convID,
		Harness: harness.CopilotName, Status: session.StatusIdle,
	}))

	transport, failure := dispatchSlashCommand(convID, "/compact", "", "compact")
	assert.Equal(t, slashTransportSendKeys, transport)
	assert.Equal(t, slashFailureNone, failure)
	assert.NotEmpty(t, tmux.snapshot(), "the pane is still the channel without the drive")
}

// The note a caller reads must describe the channel that carried the command.
// TCL-1053 shipped the other version — an echo derived from the intent that
// selected the transport rather than from the transport itself.
func TestSlashNoteNamesTheTransportThatActuallyCarriedIt(t *testing.T) {
	assert.Contains(t, slashNote(slashTransportCopilotAPI, "/compact", false), "Copilot API")
	assert.Contains(t, slashNote(slashTransportCopilotAPI, "/compact", true), "ordered")
	assert.Contains(t, slashNote(slashTransportOpenCodeAPI, "/compact", false), "OpenCode")
	assert.Contains(t, slashNote(slashTransportSendKeys, "/compact", false), "send-keys")
	assert.NotContains(t, slashNote(slashTransportCopilotAPI, "/compact", false), "send-keys",
		"the one sentence that must never appear for a pane nobody typed into")
}

// The spawn welcome was the last caller-derived text still typed into an
// API-driven pane — and typing it was not merely a sink, it was a LOSS: the
// bootstrap creates a fresh session under the conversation id and foregrounds
// it, so anything typed before that lands in the startup session it replaces.
// The welcome carries the agent's identity, its group and the pointer to the
// briefing waiting in its inbox; an agent that lost it looks exactly like one
// that read it and had nothing to say.
func TestCopilotAPISpawnWelcomeGoesOverSessionSend(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)

	// The post-init wait is stubbed out binary-wide by TestMain (no bootstrap
	// runs under test, so no handle could ever appear). Here a handle DOES
	// exist, which is the state the wait exists to reach.
	restore := SetCopilotAPIPostInitWaitForTest(func(string) bool { return true })
	t.Cleanup(restore)

	welcome := "[system: spawned by lead as \"worker\" (role: worker) in group \"crew\"]"
	require.NoError(t, sendCopilotAPIMessage(fixture.convID, welcome))

	var sent copilotapi.SendParams
	require.NoError(t, json.Unmarshal(
		fixture.server.paramsFor(copilotapi.MethodSessionSend), &sent))
	assert.Equal(t, welcome, sent.Prompt)
	fixture.assertNoKeystrokes(t)
}

// TCL-1080's defect, asserted as the TRANSITION that separates the two
// predicates rather than as a level that both satisfy.
//
// The wait must follow the CONNECTION. It used to loop on copilotAPIDriven,
// which is true from the durable launch posture that completeCopilotAPILaunch
// writes BEFORE it starts the bootstrap — so it returned on its first iteration
// of every API-drive launch and never waited for anything.
//
// The state below is the exact one it got wrong: the posture is recorded and no
// handle exists. Note what a single-instant assertion cannot do here. "The wait
// has not returned" is equally true of a wait that is working and of a wait
// that has hung, and "the wait returned true" is equally true of a wait that
// saw the handle and of the bug. Only the transition — blocked while
// disconnected, THEN returning true once the handle lands — is produced by the
// live behaviour and by nothing else. The second half is also this test's
// positive control: without it a permanently-stuck wait would pass.
func TestTheSpawnPostInitWaitFollowsTheConnectionAndNotTheLaunchPosture(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	haveCopilotAPILaunchIntent(t, fixture.agentID)

	// The bootstrap window: the launch has opted in durably and its channel has
	// not come up. The two predicates disagree here and nowhere else, which is
	// why this is the state the call site had to get right.
	//
	// Drop closes the connection it forgets, so the handle adopted further down
	// is built around a FRESH one. Re-adopting the closed original would be
	// adopting something Handle immediately evicts as dead — the wait would
	// still be right to keep waiting, and the test would fail for a reason that
	// has nothing to do with what it is about.
	copilotAPISessions.Drop(fixture.convID)
	require.True(t, copilotAPIDriven(fixture.convID),
		"the launch BELONGS to the API channel — that is what the posture records")
	require.False(t, copilotAPIConnected(fixture.convID),
		"and the channel is not up, which is the question the wait is asking")

	returned := make(chan bool, 1)
	go func() { returned <- waitForCopilotAPISession(fixture.convID) }()
	select {
	case result := <-returned:
		t.Fatalf("the wait returned %v while the channel was down: it is answering "+
			"the routing question again, and every API-drive spawn's rename and "+
			"welcome go out before the bootstrap has foregrounded its session", result)
	case <-time.After(750 * time.Millisecond):
	}

	// The channel comes up. Adopt is what runCopilotAPIBootstrap does, and it
	// does it only after the session was created, foregrounded and prompted —
	// so this is the moment the post-init delivery becomes safe.
	copilotAPISessions.Adopt(&copilotAPISession{
		ConvID: fixture.convID, SessionID: "copilot-session-1",
		Port: fixture.server.port(), PanePID: os.Getpid(),
		Client: dialFakeCopilot(t, fixture.server),
	})
	t.Cleanup(func() { copilotAPISessions.Drop(fixture.convID) })
	select {
	case result := <-returned:
		assert.True(t, result, "the wait must report the channel it just watched come up")
	case <-time.After(10 * time.Second):
		t.Fatal("the wait never noticed the handle it exists to wait for")
	}
}

// The other half of the contract: the wait gives up rather than blocking
// forever, and its budget is the bootstrap's own.
//
// The deadline arm needs a budget a test can outlast. copilotAPIBootstrapTimeout
// is copilotAPIStartupTimeout + 30s by construction, deliberately so it stays
// "the port wait's ceiling plus a margin" when a test shortens that ceiling —
// so reaching a sub-second budget means driving the ceiling negative. That is
// not a state production can produce; it is arithmetic on the same expression
// the real wait uses, and it exercises the same comparison.
func TestTheSpawnPostInitWaitGivesUpAtTheBootstrapsBudget(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	haveCopilotAPILaunchIntent(t, fixture.agentID)
	copilotAPISessions.Drop(fixture.convID)

	previous := copilotAPIStartupTimeout
	copilotAPIStartupTimeout = 300*time.Millisecond - 30*time.Second
	t.Cleanup(func() { copilotAPIStartupTimeout = previous })
	require.Equal(t, 300*time.Millisecond, copilotAPIBootstrapTimeout(),
		"the shortening below is only meaningful if it moved the real budget")

	started := time.Now()
	assert.False(t, waitForCopilotAPISession(fixture.convID),
		"a channel that never came up is a real answer, not a timeout to retry")
	elapsed := time.Since(started)

	// BRACKETED, not merely bounded above. A one-sided "it finished within 10s"
	// passes against a wait that uses any budget at all — including a hardcoded
	// one unrelated to the bootstrap's — which would leave the property in this
	// test's own name untested. That property is load-bearing rather than
	// cosmetic: the pane fallback is safe because this wait outlives the
	// bootstrap's context, and it only outlives it because the two budgets are
	// the same expression. A wait that gave up early would put back exactly
	// TCL-1080's failure, with the bootstrap foregrounding a fresh session over
	// the pane the fallback just typed into.
	//
	// The lower bound is the budget itself; the upper allows one poll interval
	// plus loaded-host slack, and is deliberately tight enough that a budget an
	// order of magnitude larger fails.
	assert.GreaterOrEqual(t, elapsed, copilotAPIBootstrapTimeout(),
		"the wait gave up BEFORE the bootstrap's budget, so it is no longer "+
			"guaranteed to outlive the bootstrap's context")
	assert.Less(t, elapsed, copilotAPIBootstrapTimeout()+copilotAPIPollInterval+2*time.Second,
		"the wait outlasted the bootstrap's budget by more than a poll, so it is "+
			"not using that budget")
}

// The override, at the seam, against a channel that is genuinely up.
//
// Two things are pinned here and they are easy to conflate. The first is that
// the routed default is untouched: a live API channel still takes
// session.name.set and still types nothing. The second is that the pane
// override REACHES THE SINK.
//
// The second one is not hypothetical fussiness. The first version of this
// change set the channel at deliverRename and stopped there, on the reasoning
// that the routing decision lives in one place. It does not: deliverRename's
// pane arm calls injectSlashCommand, which calls dispatchSlashCommand, which
// asks copilotAPIDriven again on its own account and has no typed RPC for a
// rename token — so the forced-to-the-pane rename was refused with "lifecycle
// command has no typed RPC mapping" and the agent stayed nameless. An override
// applied at the top of a call chain is not an override; it has to reach every
// predicate between the decision and the sink.
//
// So the assertion below is deliberately on the KEYSTROKE and not on
// deliverRenameOn's return value. The broken version returned false, which a
// require.True would have caught — but a variant that returned true while
// delivering nothing is the same class of bug and is what the keystroke pins.
func TestTheCopilotPaneOverrideChangesTheChannelAndNothingElseDoes(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)

	require.True(t, deliverRenameOn(fixture.convID, "routed title", deliveryChannelRouted))
	assert.Contains(t, fixture.server.methodsCalled(), copilotapi.MethodSessionNameSet,
		"a live API channel still carries the rename; the override changes one caller, "+
			"not the rule")
	fixture.assertNoKeystrokes(t)

	require.True(t, deliverRenameOn(fixture.convID, "pane title", deliveryChannelPane))
	var typed bool
	for _, command := range fixture.tmux.snapshot() {
		for _, argument := range command {
			if strings.Contains(argument, "/rename pane title") {
				typed = true
			}
		}
	}
	assert.True(t, typed,
		"the pane override must reach the pane. It failed here once already, one "+
			"call deeper than the predicate it was written against, and the only "+
			"symptom was a WARN line")
}

// haveCopilotAPILaunchIntent records the durable opt-in without a connection —
// the state the routing predicate has to recognise on its own.
func haveCopilotAPILaunchIntent(t *testing.T, agentID string) {
	t.Helper()
	api := true
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, CopilotAPI: &api,
	}))
}

// The finding this predicate was rewritten for: "no handle" is not "never took
// the drive". A launch that opted out of keystrokes has no connection during
// the bootstrap's up-to-a-minute port wait, after a bootstrap that failed, and
// after every agentd restart — handles live in memory only. Each of those must
// HOLD the delivery, not type it in.
func TestCopilotAPILaunchWithNoHandleHoldsRatherThanTyping(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	haveCopilotAPILaunchIntent(t, fixture.agentID)
	// The connection goes away — an agentd restart, or a bootstrap that never
	// completed. The launch's opt-in does not go away with it.
	copilotAPISessions.Drop(fixture.convID)

	assert.True(t, copilotAPIDriven(fixture.convID),
		"the launch opted out of keystrokes; losing the connection does not opt it back in")
	_, err := copilotAPIDrive(fixture.convID)
	assert.Error(t, err, "and there is nothing to send on right now")

	group, err := db.CreateAgentGroup("copilot-api", "")
	require.NoError(t, err)
	messageID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: group, FromConv: "peer", ToConv: fixture.convID, Body: "hello",
	})
	require.NoError(t, err)
	message, err := db.GetAgentMessage(messageID)
	require.NoError(t, err)

	assert.False(t, sendNudgeBracket(fixture.convID, message, "[msg #1 from peer] hello"),
		"held for retry, which is a visible recoverable state; typed in is an "+
			"invisible unrecoverable one")
	fixture.assertNoKeystrokes(t)

	transport, failure := dispatchSlashCommand(fixture.convID, "/compact", "", "compact")
	assert.Equal(t, slashTransportNone, transport)
	assert.Equal(t, slashFailureControl, failure)
	fixture.assertNoKeystrokes(t)
}

// haveCopilotAPIConversationFallbackIntent records the durable opt-in in the
// CONVERSATION fallback and nowhere else — the shape a clone is actually in.
func haveCopilotAPIConversationFallbackIntent(t *testing.T, convID, cwd string) {
	t.Helper()
	require.NoError(t, db.SetConversationCopilotAPI(convID, harness.CopilotName, cwd, true))
}

// The same rule as the test above, for the record a CLONE actually has.
//
// TCL-1058's version records the opt-in on the stable agent's relaunch profile,
// which is where a spawned agent's posture is frozen. A clone has no such
// profile: it is a new agent, and nothing on the clone path ever writes one, so
// its drive lives only in the conversation fallback. That is the arm where a
// dropped record turns an API-driven agent into a typed-into one, and it needs
// its own assertion because the two records are read by different halves of
// copilotLaunchIntentForConv.
func TestACopilotCloneShapedRecordAlsoHoldsRatherThanTyping(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	haveCopilotAPIConversationFallbackIntent(t, fixture.convID, t.TempDir())
	// The agent profile exists — the launch projection writes one — but it
	// records NO drive, which is exactly a clone's shape: nothing on the clone
	// path ever freezes that field. So the conversation fallback is what has to
	// answer, and this pins that the test is really exercising it.
	agentProfile, err := db.AgentRelaunchProfileForConv(fixture.convID)
	require.NoError(t, err)
	require.NotNil(t, agentProfile)
	require.Nil(t, agentProfile.CopilotAPI,
		"with a drive recorded here the agent profile would answer first and the "+
			"conversation fallback this test is about would never be consulted")

	copilotAPISessions.Drop(fixture.convID)

	assert.True(t, copilotAPIDriven(fixture.convID),
		"a clone that took the drive opted out of keystrokes just as much as a spawn did")
	_, err = copilotAPIDrive(fixture.convID)
	assert.Error(t, err, "and there is nothing to send on right now")

	group, err := db.CreateAgentGroup("copilot-api-clone", "")
	require.NoError(t, err)
	messageID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: group, FromConv: "peer", ToConv: fixture.convID, Body: "hello",
	})
	require.NoError(t, err)
	message, err := db.GetAgentMessage(messageID)
	require.NoError(t, err)

	assert.False(t, sendNudgeBracket(fixture.convID, message, "[msg #1 from peer] hello"),
		"held for retry rather than typed into the pane")
	fixture.assertNoKeystrokes(t)
}

// The unread reminder shares pickNudgeSession with the inbox nudge and carries
// the same peer-derived sender labels, so it is the same delivery family and
// takes the same channel. It was taught about OpenCode and not about the API
// drive, which made it a keystroke path to a connected agent on every reminder
// tick for the life of that agent — not a window, and not a race.
func TestCopilotAPIUnreadReminderGoesOverSessionSend(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)

	group, err := db.CreateAgentGroup("copilot-api", "")
	require.NoError(t, err)
	messageID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: group, FromConv: "peer", ToConv: fixture.convID,
		Subject: "a subject", Body: "hello",
	})
	require.NoError(t, err)
	message, err := db.GetAgentMessage(messageID)
	require.NoError(t, err)

	// Delivered but unread is the state the reminder sweep exists for.
	require.NoError(t, db.MarkAgentMessageDelivered(message.ID))

	// A fresh state with a zero epoch, driven at a clock well past the
	// interval, so the sweep's own cadence does not decide this test.
	runUnreadReminderTickWith(time.Now().Add(2*unreadReminderInterval),
		&unreadReminderState{remindedAt: map[string]time.Time{}})

	assert.Contains(t, fixture.server.methodsCalled(), copilotapi.MethodSessionSend,
		"the reminder must take the same channel as the nudge it reminds about")
	fixture.assertNoKeystrokes(t)
}

// ---------------------------------------------------------------------------
// The delivery family, asserted structurally
// ---------------------------------------------------------------------------

// copilotAPIChannelChain is the call chain a spawn's post-init pane override
// travels, from the function that decides it to the function that types.
//
// Each entry names a function that MUST take the caller's deliveryChannel and
// hand it to the next one. The list is the artefact; the guard below is
// mechanical over it.
var copilotAPIChannelChain = []struct{ from, to string }{
	{"deliverRenameOn", "injectSlashCommandOn"},
	{"injectSlashCommandOn", "dispatchSlashCommandOn"},
}

// copilotAPIChannelLessVariants are the routed-default wrappers. Calling one
// from inside the chain is precisely how the override was silently dropped the
// first time, so the chain may not contain them at all.
var copilotAPIChannelLessVariants = []string{
	"deliverRename", "injectSlashCommand", "dispatchSlashCommand",
}

// The override has to reach the sink, and this is the mechanical statement of
// that rather than a comment asking for it.
//
// # The defect it exists for
//
// The spawn's post-init pane fallback was first written by setting the channel
// at deliverRenameOn and stopping there. deliverRenameOn's pane arm called
// injectSlashCommand — the routed-default wrapper — which dispatched, asked
// copilotAPIDriven on its own account, took the Copilot branch, found no typed
// RPC for a rename token and refused. The override existed, read correctly,
// compiled, and did nothing.
//
// A guard on the OVERRIDE CONSTANT's call sites would have been green through
// all of that: the constant had exactly one call site the whole time. What
// broke was the threading, so the threading is what is asserted.
//
// # What this covers, and what it does not
//
// Covered: every hop listed in copilotAPIChannelChain takes a deliveryChannel
// parameter named `channel`, passes that identifier onward, and calls no
// routed-default wrapper; and dispatchSlashCommandOn's copilotAPIDriven test is
// guarded by `channel` rather than standing alone.
//
// NOT covered: a NEW predicate inserted between the decision and the sink
// tomorrow. Nothing here notices a fourth hop appearing, because the chain is a
// written list and a new function is not on it. That gap is stated rather than
// left to be discovered — the behavioural arm of
// TestTheCopilotPaneOverrideChangesTheChannelAndNothingElseDoes is what would
// catch it, by asserting the keystroke rather than the shape.
func TestTheCopilotPaneOverrideIsThreadedAllTheWayToTheSink(t *testing.T) {
	functions := map[string]*ast.FuncDecl{}
	paneConstantHolders := map[string]*ast.FuncDecl{}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
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
			if fn.Recv == nil {
				// The chain's own hops, which are all plain functions. Keyed by
				// bare name; a same-named function in a build-tagged file would
				// collide, which is a known limit rather than a covered case.
				functions[fn.Name.Name] = fn
			}
			// Everything that can HOLD the override constant, methods included.
			// The containment count below used to skip methods entirely, so a
			// method returning deliveryChannelPane was a second call site the
			// count could not see — measured under review.
			key := fn.Name.Name
			if fn.Recv != nil {
				key = "(method) " + key
			}
			paneConstantHolders[key] = fn
		}
	}

	// The positive control. Every assertion below is over functions looked up
	// by name, so a rename would silently empty the whole guard.
	for _, hop := range copilotAPIChannelChain {
		require.Containsf(t, functions, hop.from,
			"%s is the chain's own subject and no longer exists; the guard is "+
				"watching nothing", hop.from)
		require.Containsf(t, functions, hop.to, "%s no longer exists", hop.to)
	}

	for _, hop := range copilotAPIChannelChain {
		fn := functions[hop.from]
		assert.Truef(t, declaresChannelParam(fn), "%s must take the caller's "+
			"deliveryChannel as a parameter named `channel`", hop.from)

		// EVERY call to the next hop, not merely some call. The existential
		// version of this check — set a flag when a good call is seen — passed
		// with a bad call sitting beside it, which is how this guard was beaten
		// twice under review: once by putting the good call on a branch nothing
		// takes, and once by dropping the override only for deliveries carrying
		// a follow-up, which the behavioural tests do not walk.
		var callsToNextHop int
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			assert.NotContainsf(t, copilotAPIChannelLessVariants, callee.Name,
				"%s calls %s, the routed-default wrapper. That silently drops the "+
					"caller's channel, and the only symptom is a delivery refused in a "+
					"log line", hop.from, callee.Name)
			if callee.Name != hop.to {
				return true
			}
			callsToNextHop++
			var passesChannel bool
			for _, argument := range call.Args {
				if ident, ok := argument.(*ast.Ident); ok && ident.Name == "channel" {
					passesChannel = true
				}
			}
			assert.Truef(t, passesChannel,
				"%s calls %s WITHOUT passing its own `channel`. Every call on this "+
					"hop must carry it, not merely one of them: a second call that "+
					"defaults or re-derives the channel ends the caller's override "+
					"silently, and does so while a correctly-threaded call sits beside "+
					"it keeping this guard green",
				hop.from, hop.to)
			return true
		})
		assert.Positivef(t, callsToNextHop,
			"%s no longer calls %s at all; the chain is broken, or this guard is "+
				"watching the wrong pair", hop.from, hop.to)
	}

	// A hop may not REASSIGN the channel it was handed. Threading it perfectly
	// and then overwriting it mid-chain —
	//
	//	if copilotAPIDriven(convID) { channel = deliveryChannelRouted }
	//
	// — destroys the override in transit while every assertion above still
	// passes, because the identifier is still declared and still passed on.
	// That spelling is a plausible "normalize the channel" edit, which is
	// exactly the kind this package has had a guard sail past before.
	for _, hop := range copilotAPIChannelChain {
		ast.Inspect(functions[hop.from].Body, func(node ast.Node) bool {
			// AssignStmt is not the only way to replace the value. A range
			// variable named `channel`, or a var declaration shadowing it, both
			// substitute a different value while every other check here still
			// passes — measured under review with a RangeStmt.
			var targets []ast.Expr
			switch typed := node.(type) {
			case *ast.AssignStmt:
				targets = typed.Lhs
			case *ast.RangeStmt:
				targets = []ast.Expr{typed.Key, typed.Value}
			case *ast.ValueSpec:
				for _, ident := range typed.Names {
					targets = append(targets, ident)
				}
			default:
				return true
			}
			for _, target := range targets {
				if ident, ok := target.(*ast.Ident); ok && ident.Name == "channel" {
					assert.Failf(t, "the channel is reassigned or shadowed mid-chain",
						"%s assigns to its own `channel` parameter. The caller's "+
							"override then ends here, and it ends silently: the "+
							"identifier is still threaded onward, so every other check "+
							"in this guard still passes", hop.from)
				}
			}
			return true
		})
	}

	// The override constant's containment. Weaker than the threading above —
	// the constant had exactly one call site the entire time the rename was
	// being refused one hop deeper — but it is the property deliveryChannelPane's
	// own comment claims, so it is asserted rather than claimed.
	var paneCallSites int
	for name, fn := range paneConstantHolders {
		if name == "deliverRenameOn" || name == "injectSlashCommandOn" ||
			name == "dispatchSlashCommandOn" {
			// The chain itself compares against the routed constant; only uses
			// that SELECT the pane are call sites in the sense meant here.
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok && ident.Name == "deliveryChannelPane" {
				paneCallSites++
			}
			return true
		})
	}
	assert.Equal(t, 1, paneCallSites,
		"deliveryChannelPane must have exactly one non-test call site. Its doc "+
			"argues the entitlement for ONE caller — a one-shot delivery with no "+
			"retry — and a second caller has to argue its own case rather than "+
			"inheriting that one")

	// The last hop is the one that actually chooses, so its choice must READ
	// the channel. Without this the chain could thread it perfectly and then
	// ignore it.
	// EVERY copilotAPIDriven test in the sink, not merely one of them. This
	// check had the same existential shape as the threading check above, and
	// the same beat: a second, unguarded copilotAPIDriven(convID) elsewhere in
	// the sink would decide routing on its own account with the guard green,
	// because the first one still satisfied it.
	sink := functions["dispatchSlashCommandOn"]
	guarded := map[*ast.CallExpr]bool{}
	var driven []*ast.CallExpr
	ast.Inspect(sink.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if callee, ok := call.Fun.(*ast.Ident); ok &&
				callee.Name == "copilotAPIDriven" {
				driven = append(driven, call)
			}
			return true
		}
		binary, ok := node.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		// Proximity is not guarding. An enclosing BinaryExpr that merely MENTIONS
		// channel marks nothing: only an && whose LEFT operand tests the channel
		// actually gates what is on its right. Measured under review, all three
		// counted as guarded by the proximity version:
		//
		//	channel == deliveryChannelRouted && copilotAPIDriven(c)  // guarded
		//	channel == deliveryChannelRouted || copilotAPIDriven(c)  // decides alone
		//	copilotAPIDriven(c) && channel != deliveryChannelRouted  // runs first
		//
		// The second widens the condition while reading like a narrowing, and the
		// third is an INVERTED guard that the behavioural tests also miss.
		if binary.Op != token.LAND {
			return true
		}
		var gatesOnChannel bool
		ast.Inspect(binary.X, func(inner ast.Node) bool {
			if ident, ok := inner.(*ast.Ident); ok && ident.Name == "channel" {
				gatesOnChannel = true
			}
			return true
		})
		if !gatesOnChannel {
			return true
		}
		ast.Inspect(binary.Y, func(inner ast.Node) bool {
			if call, ok := inner.(*ast.CallExpr); ok {
				if callee, ok := call.Fun.(*ast.Ident); ok &&
					callee.Name == "copilotAPIDriven" {
					guarded[call] = true
				}
			}
			return true
		})
		return true
	})

	assert.NotEmpty(t, driven,
		"dispatchSlashCommandOn no longer tests copilotAPIDriven at all; this "+
			"guard is watching nothing")
	for _, call := range driven {
		assert.Truef(t, guarded[call],
			"dispatchSlashCommandOn tests copilotAPIDriven at offset %d without the "+
				"caller's channel in the same condition. Unguarded, it re-decides "+
				"the routing the caller already decided — which is the bug this "+
				"whole chain exists to close. EVERY such test must be guarded, not "+
				"just the first one a walk happens to find",
			call.Pos())
	}
}

func declaresChannelParam(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, field := range fn.Type.Params.List {
		ident, ok := field.Type.(*ast.Ident)
		if !ok || ident.Name != "deliveryChannel" {
			continue
		}
		for _, name := range field.Names {
			if name.Name == "channel" {
				return true
			}
		}
	}
	return false
}

// copilotAPIKeystrokeSinkFiles lists every file allowed to put text into a
// pane, and why each one is either already API-aware or unreachable for an
// API-driven Copilot agent.
//
// This exists because of how this ticket's worst bug got in. The RPC conversion
// was done by finding the delivery path it went looking for — the inbox nudge —
// and not the FAMILY that path belongs to. unread_reminder.go shares
// pickNudgeSession with it, carries the same peer-derived sender labels, and
// was simply never visited: a keystroke path to a connected agent, reachable on
// every reminder tick for the life of that agent. A cold reviewer found it; a
// grep would have found it too, and memory did not.
//
// So the completeness check is written down rather than performed once. It is
// deliberately structural, like TCL-1056's port guard: a behavioural test can
// only cover the sites someone thought of, and the failure being guarded
// against is a site nobody thought of. A NEW keystroke sink trips this and has
// to argue for itself in review — either by becoming API-aware, or by saying
// here why an API-driven Copilot agent cannot reach it.
//
// It is not a security boundary; anyone editing the code can edit the list. It
// is a tripwire on an invariant whose violation is otherwise silent and looks
// exactly like working code.
var copilotAPIKeystrokeSinkFiles = map[string]string{
	// Converted: the lifecycle-command dispatch takes the Copilot branch first.
	"handlers.go": "dispatchSlashCommand routes an API-driven conv to RPC before reaching send-keys",
	// Converted: inbox nudges.
	"flush.go": "sendNudgeBracket routes an API-driven conv to session.send",
	// Converted: the unread reminder, same family as the nudge.
	"unread_reminder.go": "the reminder sweep routes an API-driven conv to session.send",
	// Converted: the spawn welcome, which additionally waits for the bootstrap.
	"lifecycle.go": "runSpawnPostInit routes an API-driven conv to session.send, and soft " +
		"exit stays on keystrokes deliberately — no RPC ends the copilot process",
	// Unreachable: remote control is gated on CanRemoteControl(), and Copilot's
	// Lifecycle returns "" for RemoteControlCommand, so no Copilot agent — API
	// or not — ever reaches this sink.
	"remote_control.go": "gated on CanRemoteControl(); Copilot has no remote-control command",
}

// copilotAPIKeystrokeSinks are the helpers that type into a pane. Bare
// identifiers rather than qualified selectors, because they are this package's
// own functions.
var copilotAPIKeystrokeSinks = []string{
	"injectTextAndSubmit",
	"injectBracketedTextAndSubmit",
	"injectTextAndSubmitWithOptions",
	"injectMenuToggle",
	// Found by cold review: it delegates to injectTextAndSubmitWithOptions and
	// lives in handlers.go, so it was a real sink in an already-watched file that
	// this list simply did not name. A guard whose header promises that a NEW
	// sink trips it has to actually enumerate the existing ones.
	"injectSoftExitTextSerializedBy",
}

func TestEveryKeystrokeSinkIsAccountedForAgainstTheCopilotAPIDrive(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	found := map[string]bool{}
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
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || !slices.Contains(copilotAPIKeystrokeSinks, ident.Name) {
				return true
			}
			// The declarations themselves and their internal delegation live in
			// handlers.go, which is on the list anyway.
			found[name] = true
			return true
		})
	}

	for name := range found {
		assert.Contains(t, copilotAPIKeystrokeSinkFiles, name,
			"%s types into a pane and is not accounted for against the Copilot API "+
				"drive. An agent launched with --copilot-api opted OUT of the keystroke "+
				"path; a new sink that does not know about it is a silent way back into "+
				"the injection sink. Make it API-aware, or add it to "+
				"copilotAPIKeystrokeSinkFiles with the reason a Copilot agent cannot "+
				"reach it", name)
	}
	// The positive control. Without it every assertion above passes vacuously
	// against a rename of the helpers, which is precisely how this guard would
	// stop watching anything without anyone noticing.
	for name := range copilotAPIKeystrokeSinkFiles {
		assert.Contains(t, found, name,
			"%s is listed as a keystroke sink but no longer calls one; if the sink moved, "+
				"this guard is now watching the wrong file", name)
	}
}
