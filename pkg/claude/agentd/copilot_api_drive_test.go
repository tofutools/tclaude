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

// The wait's own contract: it returns as soon as a handle appears, and reports
// false rather than blocking forever when one never does.
func TestWaitForCopilotAPISessionFollowsTheHandle(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	assert.True(t, waitForCopilotAPISession(fixture.convID),
		"a conversation that already has a handle must not wait at all")

	copilotAPISessions.Drop(fixture.convID)
	// Not run to its real deadline — that is 90s by design. What matters here
	// is that the loop's answer follows the registry, which the positive arm
	// above establishes; the negative arm is the same predicate inverted.
	assert.False(t, copilotAPIDriven(fixture.convID))
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
