package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"sync"
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

// The reconnect is the answer to a durable, user-visible failure: an agentd
// restart leaves every already-running API-driven Copilot agent with no handle,
// and since TCL-1058 those agents HOLD their mail rather than having it typed
// into the pane. Holding is right; staying mute until a relaunch is not.
//
// Every test here is written against the pane as well as the registry, because
// the two failures this path could introduce are opposite: re-establishing
// nothing (the agent stays mute) and re-establishing by opening a session (the
// agent's conversation is destroyed while the daemon reports success).

// paneReportingTmux answers tmux's pane-pid query with a pid of the test's
// choosing, and records everything else the way commandRecordingTmux does.
//
// livePanePID shells out to `tmux display-message`, and the ownership proof is
// taken against whatever it returns — so a stub that answered nothing would
// make every reconnect fail at the port wait for a reason unrelated to what
// these tests are about.
type paneReportingTmux struct {
	panePID  int
	mu       sync.Mutex
	commands [][]string
}

func (r *paneReportingTmux) Command(args ...string) *exec.Cmd {
	r.mu.Lock()
	r.commands = append(r.commands, append([]string(nil), args...))
	r.mu.Unlock()
	if len(args) > 0 && args[0] == "display-message" {
		return exec.Command("printf", fmt.Sprintf("0|%d", r.panePID))
	}
	return exec.Command("true")
}

func (r *paneReportingTmux) ListSessions() (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (r *paneReportingTmux) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.commands))
	for i := range r.commands {
		out[i] = append([]string(nil), r.commands[i]...)
	}
	return out
}

func (r *paneReportingTmux) assertNoKeystrokes(t *testing.T) {
	t.Helper()
	for _, command := range r.snapshot() {
		require.NotEmpty(t, command)
		assert.NotContains(t, []string{"send-keys", "set-buffer", "paste-buffer"}, command[0],
			"an API-driven agent must not be typed into: %v", command)
	}
}

// copilotAPIReconnectFixture is a daemon that has just restarted: a Copilot
// conversation whose launch took the API drive, whose port record survived,
// whose pane is still alive with its server still listening — and an EMPTY
// handle registry, which is the whole of what a restart destroys.
type copilotAPIReconnectFixture struct {
	convID string
	server *fakeCopilotServer
	tmux   *paneReportingTmux
}

func newCopilotAPIReconnectFixture(t *testing.T) *copilotAPIReconnectFixture {
	t.Helper()
	return newCopilotAPIReconnectFixtureWithPosture(t, true)
}

// newCopilotAPIReconnectFixtureWithPosture is the fixture with the launch's
// recorded drive posture made optional, so a test can stand up the LEGACY shape:
// a conversation that is genuinely running the API drive but whose posture was
// never written. That is not an exotic state — it is what every conversation
// launched before TCL-1059 closed the mint paths looks like, and it is the one
// whose mail routes to keystrokes today.
func newCopilotAPIReconnectFixtureWithPosture(
	t *testing.T, recordPosture bool,
) *copilotAPIReconnectFixture {
	t.Helper()
	setupTestDB(t)
	t.Cleanup(SetInjectSettleDelayForTest(0))

	server := newFakeCopilotServer(t)
	// The pane pid has to be a process that genuinely owns the listener, so the
	// ownership proof passes for real rather than being stubbed: this test
	// binary does.
	tmux := &paneReportingTmux{panePID: os.Getpid()}
	previous := clcommon.Default
	clcommon.Default = tmux
	t.Cleanup(func() { clcommon.Default = previous })

	const (
		convID    = "ses_copilot_api_reconnect"
		sessionID = "spwn-copilot-api-reconnect"
	)
	cwd := t.TempDir()
	_, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: sessionID, TmuxSession: sessionID, ConvID: convID,
		Harness: harness.CopilotName, Status: session.StatusIdle,
		Cwd: cwd,
	}))
	// The durable posture the launch recorded. This is what makes the
	// conversation's mail HOLD rather than route to keystrokes while it has no
	// handle, so it is the state a restarted daemon actually finds.
	if recordPosture {
		require.NoError(t, db.SetConversationCopilotAPI(convID, harness.CopilotName, cwd, true, nil))
	}
	require.NoError(t, db.UpsertCopilotAPIRuntime(db.CopilotAPIRuntime{
		ConvID: convID, Port: server.port(),
	}))
	t.Cleanup(func() { copilotAPISessions.Drop(convID) })

	return &copilotAPIReconnectFixture{convID: convID, server: server, tmux: tmux}
}

func (f *copilotAPIReconnectFixture) reconcile(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reconcileCopilotAPISessions(ctx)
}

// The headline property, asserted end to end: a conversation that was mute
// after the restart can be spoken to again, over RPC, without a keystroke.
//
// Asserting Connected alone would not be enough. "The registry has an entry"
// is satisfied by a handle to anything at all; what the agent needs is a
// channel that carries a message, so the delivery is the assertion and the
// registry is the mechanism.
func TestCopilotAPIReconnectReEstablishesTheChannelAfterARestart(t *testing.T) {
	fixture := newCopilotAPIReconnectFixture(t)

	require.False(t, copilotAPISessions.Connected(fixture.convID),
		"precondition: a restarted daemon holds no handles, which is the state this fixes")
	require.True(t, copilotAPIDriven(fixture.convID),
		"precondition: the conversation still BELONGS to the API channel — that is why "+
			"its mail is held rather than typed in")
	_, err := copilotAPIDrive(fixture.convID)
	require.Error(t, err, "precondition: and the channel is unavailable, so it holds")

	fixture.reconcile(t)

	assert.True(t, copilotAPISessions.Connected(fixture.convID),
		"the whole point: an already-running agent is reachable again without relaunching")

	group, err := db.CreateAgentGroup("copilot-api-reconnect", "")
	require.NoError(t, err)
	messageID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: group, FromConv: "peer", ToConv: fixture.convID, Body: "after the restart",
	})
	require.NoError(t, err)
	message, err := db.GetAgentMessage(messageID)
	require.NoError(t, err)

	const nudge = "[msg #1 from peer] after the restart"
	require.True(t, sendNudgeBracket(fixture.convID, message, nudge))

	assert.Contains(t, fixture.server.methodsCalled(), copilotapi.MethodSessionSend)
	var sent copilotapi.SendParams
	require.NoError(t, json.Unmarshal(
		fixture.server.paramsFor(copilotapi.MethodSessionSend), &sent))
	assert.Equal(t, nudge, sent.Prompt)
	assert.Equal(t, fixture.convID, sent.SessionID,
		"the session a reconnect drives is the CONVERSATION's own id — the id the "+
			"bootstrap opened its session under and the id everything else resolves")
	fixture.tmux.assertNoKeystrokes(t)
}

// The reconnect's defining property. It rejoins a conversation it must not
// change, and each of the calls that would open one is unsafe in at least one
// case it cannot tell apart in advance:
//
//   - session.create at a COLD id starts it FRESH, discarding the history.
//   - session.resume appends an event and re-applies options (measured: it
//     reloads MCP servers and re-emits the system prompt).
//   - session.setForeground moves what the human is looking at.
//
// Asserted against the WIRE rather than against the source, so it holds however
// the call is spelled; the structural guard beside it in
// copilot_api_bootstrap_test.go covers the calls a fake server would never be
// asked to answer.
// Driven through reconnectCopilotAPISession rather than the reconcile, and that
// is not a shortcut. The reconcile ALSO starts TCL-1057's state consumer, whose
// first act is three more reads on the same connection — so an exact-wire
// assertion taken after a reconcile is racing a goroutine and would be asserting
// something the reconcile does not promise. What is being pinned here is the
// reconnect's own traffic, which is where the property lives.
func TestCopilotAPIReconnectSendsNothingThatCouldChangeTheConversation(t *testing.T) {
	fixture := newCopilotAPIReconnectFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	handle, err := reconnectCopilotAPISession(ctx, fixture.convID)
	require.NoError(t, err,
		"precondition: the reconnect must have succeeded, or 'it sent nothing' is "+
			"trivially true of a path that did nothing")
	t.Cleanup(func() { _ = handle.Client.Close() })

	called := fixture.server.methodsCalled()
	assert.Equal(t,
		[]string{copilotapi.MethodConnect, copilotapi.MethodSessionIsProcessing},
		called,
		"a reconnect is a handshake and ONE read. Anything else on this list is a call "+
			"whose safety depends on which case the reconnect is in, which is exactly what "+
			"it cannot know")
}

// The two failure arms of a refusal want OPPOSITE responses, and one assertion
// cannot separate them:
//
//   - refuse and leave the conversation held — correct, and
//   - refuse and then open a session anyway — the failure that destroys a
//     conversation while reporting a healthy reconnect.
//
// "No handle was adopted" is true of both. So the wire is asserted too.
func TestCopilotAPIReconnectRefusesRatherThanOpeningASession(t *testing.T) {
	fixture := newCopilotAPIReconnectFixture(t)
	fixture.server.failMethod(copilotapi.MethodSessionIsProcessing,
		"Session not found: "+fixture.convID)

	fixture.reconcile(t)

	// The positive control, and without it this whole test is vacuous: every
	// assertion below — nothing connected, no forbidden method on the wire, the
	// conversation still held, no keystrokes — is satisfied just as well by a
	// reconcile that never ran at all. Proven, not assumed: with
	// reconcileCopilotAPISessions stubbed to return immediately, this test passed
	// and the other three in this file failed.
	called := fixture.server.methodsCalled()
	require.Contains(t, called, copilotapi.MethodConnect,
		"the reconnect must have got as far as connecting, or the refusal being "+
			"asserted below never happened")
	require.Contains(t, called, copilotapi.MethodSessionIsProcessing,
		"and as far as the drivability probe, which is the call whose failure IS the "+
			"refusal under test")

	assert.False(t, copilotAPISessions.Connected(fixture.convID),
		"a session the server does not hold is not a channel")
	for _, method := range called {
		assert.NotContains(t,
			[]string{
				copilotapi.MethodSessionCreate,
				copilotapi.MethodSessionResume,
				copilotapi.MethodSessionSetFg,
			},
			method,
			"the reconnect answered a missing session by calling %s. `session.create` at an "+
				"id whose history is only on disk starts it FRESH, so recovering this way "+
				"would turn 'I could not rejoin the conversation' into 'I replaced it' — and "+
				"the launch would still look healthy", method)
	}

	// And the conversation is held rather than typed into, which is the
	// behaviour the refusal has to preserve.
	require.True(t, copilotAPIDriven(fixture.convID))
	_, err := copilotAPIDrive(fixture.convID)
	assert.Error(t, err)
	fixture.tmux.assertNoKeystrokes(t)
}

// The guard clauses, with a positive control. Without the control both
// negatives would pass just as well against a reconcile that never reaches the
// seam at all — the exact shape of vacuously-green test this series keeps
// finding.
func TestCopilotAPIReconcileSkipsWhatItMustNotTouch(t *testing.T) {
	setupTestDB(t)
	server := newFakeCopilotServer(t)
	tmux := &paneReportingTmux{panePID: os.Getpid()}
	previous := clcommon.Default
	clcommon.Default = tmux
	t.Cleanup(func() { clcommon.Default = previous })

	// held: a conversation this daemon already has a live handle for. A second
	// connection would be waste, and Adopt would close the working one.
	// gone: a port record whose pane is no longer running.
	// other-harness: a conversation whose record outlived the Copilot launch
	//   that made it, and which is now running something else. The record cannot
	//   be released while the conversation has a live launch, so without the
	//   harness gate this one is a candidate on EVERY restart, for a full port
	//   wait each time.
	// fresh: the one that must be reconnected.
	for _, convID := range []string{"conv-held", "conv-gone", "conv-other-harness", "conv-fresh"} {
		require.NoError(t, db.UpsertCopilotAPIRuntime(db.CopilotAPIRuntime{
			ConvID: convID, Port: server.port(),
		}))
	}
	for _, convID := range []string{"conv-held", "conv-fresh"} {
		require.NoError(t, session.SaveSessionState(&session.SessionState{
			ID: "spwn-" + convID, TmuxSession: "spwn-" + convID, ConvID: convID,
			Harness: harness.CopilotName, Status: session.StatusIdle, Cwd: t.TempDir(),
		}))
	}
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: "spwn-conv-other-harness", TmuxSession: "spwn-conv-other-harness",
		ConvID: "conv-other-harness", Harness: harness.CodexName,
		Status: session.StatusIdle, Cwd: t.TempDir(),
	}))
	existing := dialFakeCopilot(t, server)
	copilotAPISessions.Adopt(&copilotAPISession{
		ConvID: "conv-held", SessionID: "conv-held",
		Port: server.port(), PanePID: os.Getpid(), Client: existing,
	})
	t.Cleanup(func() {
		for _, convID := range []string{
			"conv-held", "conv-gone", "conv-other-harness", "conv-fresh",
		} {
			copilotAPISessions.Drop(convID)
		}
	})

	attempted := make(chan string, 8)
	original := reconnectCopilotAPISessionFn
	reconnectCopilotAPISessionFn = func(
		_ context.Context, convID string,
	) (*copilotAPISession, error) {
		attempted <- convID
		return nil, errors.New("not reached in this test")
	}
	t.Cleanup(func() { reconnectCopilotAPISessionFn = original })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reconcileCopilotAPISessions(ctx)

	close(attempted)
	var tried []string
	for convID := range attempted {
		tried = append(tried, convID)
	}
	assert.Equal(t, []string{"conv-fresh"}, tried,
		"a conversation with a live handle needs no second connection, one with no live "+
			"session row has nothing to reconnect to, and one now running a DIFFERENT "+
			"harness has no Copilot endpoint however good its record looks — but the one "+
			"that is running Copilot and unconnected is the whole reason this sweep exists")

	assert.NotNil(t, copilotAPISessions.Handle("conv-held"),
		"the handle this daemon already had must survive the sweep, not be replaced by it")
}

// The conversation with NO recorded posture is the one this sweep most needs to
// find, and it must not be filtered out for looking unusual.
//
// It is the case where reconnecting does more than restore observation. With no
// posture recorded, TCL-1058's durable routing arm answers "not the API
// channel", so this conversation's peer-derived mail is being TYPED INTO ITS
// PANE — the injection sink the drive exists to close. Adopting a handle flips
// routing to the live-handle fast path and closes it. Skipping it to save a
// bounded port wait would be trading the highest-value case for the cheapest
// cost.
func TestCopilotAPIReconnectStillAdoptsAConversationWithNoRecordedPosture(t *testing.T) {
	// Everything a restarted daemon finds — port record, live Copilot pane —
	// except the posture, exactly as a legacy conversation looks.
	fixture := newCopilotAPIReconnectFixtureWithPosture(t, false)

	require.False(t, copilotAPIPostureRecorded(fixture.convID),
		"precondition: nothing may answer 'which drive did this launch take'")
	require.False(t, copilotAPIDriven(fixture.convID),
		"precondition: and so its mail routes to KEYSTROKES today, which is the sink "+
			"this reconnect closes rather than merely observes")

	fixture.reconcile(t)

	assert.True(t, copilotAPISessions.Connected(fixture.convID),
		"a missing posture record is a reason to LOG, never a reason to skip")
	assert.True(t, copilotAPIDriven(fixture.convID),
		"and with a live handle the routing question is answered by the connection, "+
			"so the conversation stops being typed into")
}

// A launch that establishes the channel while the sweep is still working must
// WIN, and the reconnect must stand down rather than replace it.
//
// The window is real rather than theoretical: the candidate check runs once at
// the top of the sweep and the adopt can land a port wait's whole budget later,
// with the daemon serving spawns throughout. A replace in that window closes the
// bootstrap's connection while it is still running its remaining hard steps —
// foregrounding, and delivering the launch prompt — so those fail, and the
// registry still looks healthy because the reconnect's own handle is in it. The
// visible result is an agent that was never given its briefing, which looks
// exactly like an agent that finished its work.
func TestCopilotAPIReconnectStandsDownForALaunchThatWonTheRace(t *testing.T) {
	setupTestDB(t)
	server := newFakeCopilotServer(t)
	launched := dialFakeCopilot(t, server)
	reconnected := dialFakeCopilot(t, server)

	const convID = "conv-race"
	t.Cleanup(func() { copilotAPISessions.Drop(convID) })

	// The bootstrap got there first.
	copilotAPISessions.Adopt(&copilotAPISession{
		ConvID: convID, SessionID: convID,
		Port: server.port(), PanePID: os.Getpid(), Client: launched,
	})

	took := copilotAPISessions.AdoptIfAbsent(&copilotAPISession{
		ConvID: convID, SessionID: convID,
		Port: server.port(), PanePID: os.Getpid(), Client: reconnected,
	})
	assert.False(t, took, "the launch was already there; the reconnect must stand down")

	handle := copilotAPISessions.Handle(convID)
	require.NotNil(t, handle)
	assert.Same(t, launched, handle.Client,
		"the LAUNCH's connection must survive — it is the one with hard steps still "+
			"to run, and closing it strands the agent un-briefed")
	select {
	case <-launched.Done():
		t.Fatal("the launch's connection was closed by a reconnect that should have stood down")
	default:
	}

	// The positive control: with the slot free, the same call must take it.
	// Without this the assertion above passes against an AdoptIfAbsent that
	// always refuses, which would break the reconnect entirely.
	copilotAPISessions.Drop(convID)
	third := dialFakeCopilot(t, server)
	assert.True(t, copilotAPISessions.AdoptIfAbsent(&copilotAPISession{
		ConvID: convID, SessionID: convID,
		Port: server.port(), PanePID: os.Getpid(), Client: third,
	}), "an unheld conversation is exactly what the reconnect exists to adopt")

	// And a DEAD incumbent is not a claim worth deferring to: refusing in favour
	// of a closed socket would leave the agent mute for the life of the daemon.
	require.NoError(t, third.Close())
	fourth := dialFakeCopilot(t, server)
	assert.True(t, copilotAPISessions.AdoptIfAbsent(&copilotAPISession{
		ConvID: convID, SessionID: convID,
		Port: server.port(), PanePID: os.Getpid(), Client: fourth,
	}), "a dead incumbent must not block a live reconnect")
}

// The reconcile is only worth anything if it FIRES, and the one thing no test
// above can see is the daemon's own startup wiring — every flow test stubs the
// kick-off out binary-wide, precisely so it does not run.
//
// So the trigger is asserted structurally. A restart is the entire trigger for
// this feature: there is no drop to observe and no other path establishes a
// handle for an already-running agent, so a reconcile nobody calls is a
// perfectly healthy-looking package that fixes nothing.
//
// # Why it is not enough for the call to EXIST
//
// The first version of this guard walked the whole file for a call by that name
// and passed if it found one. That certified presence and claimed execution, and
// it was beaten in one line under review:
//
//	if os.Getenv("TCLAUDE_NEVER_SET_XYZ") == "1" {
//	    startCopilotAPIReconnect(cronStop)
//	}
//
// Guard green, every API-driven Copilot agent mute after every restart. A
// whole-file walk is worse still: the satisfying call need not be in the startup
// path at all, so a dead helper elsewhere in serve.go would have kept it happy.
//
// So this locates runServe and requires the call at the TOP LEVEL of its body —
// not nested in any if, loop, select or closure. Top level is what "runs" means
// for a statement list.
//
// # What it still cannot see
//
// Reachability. An early return above the call leaves it correctly positioned
// and unreachable, and no static check of call shape sees that. Stated so the
// next reader treats this as a bounded claim rather than a total one — it is the
// same residue every guard of this family carries.
func TestTheDaemonStartupCallsTheCopilotAPIReconcile(t *testing.T) {
	const (
		starter = "startCopilotAPIReconnect"
		caller  = "runServe"
	)

	source, err := os.ReadFile("serve.go")
	require.NoError(t, err)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "serve.go", source, 0)
	require.NoError(t, err)

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == caller {
			body = fn.Body
		}
	}
	require.NotNilf(t, body, "%s must exist in serve.go for this guard to mean "+
		"anything; if the daemon's startup moved, this guard has to move with it",
		caller)

	// The same corrected descent the dialler guard uses: it refuses to enter a
	// function literal or the skippable operand of && / ||, so a call parked in a
	// closure that nobody invokes does not count. That exact shape beat the first
	// version of this guard under review.
	unconditional := callAlwaysEvaluated(body, starter)

	assert.True(t, unconditional,
		"%s must call %s UNCONDITIONALLY, at the top level of its body. An agentd "+
			"restart is the ONLY trigger for re-establishing an already-running "+
			"agent's channel — nothing else in the daemon does it — so a reconcile "+
			"that does not run leaves every API-driven Copilot agent mute until it "+
			"is relaunched. A call that merely EXISTS somewhere in serve.go does not "+
			"establish this: wrapping it in a conditional no daemon start takes, or "+
			"parking it in a helper nobody invokes, satisfies presence while "+
			"destroying the behaviour",
		caller, starter)
}

// The reaper releases a port record on a grace timer, and this sweep reads the
// same records. A record for a pane that is already gone must cost a bounded,
// NAMED failure rather than a hang or a connection to whatever now holds the
// number.
func TestCopilotAPIReconnectFailsNamedForAConversationWhosePaneIsGone(t *testing.T) {
	setupTestDB(t)
	server := newFakeCopilotServer(t)
	// A pane pid tmux reports as dead: livePanePID answers 0, which is the same
	// thing the reconcile sees for a pane that exited while agentd was down.
	tmux := &paneReportingTmux{panePID: 0}
	previous := clcommon.Default
	clcommon.Default = tmux
	t.Cleanup(func() { clcommon.Default = previous })

	const convID = "conv-pane-gone"
	require.NoError(t, db.UpsertCopilotAPIRuntime(db.CopilotAPIRuntime{
		ConvID: convID, Port: server.port(),
	}))
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: "spwn-" + convID, TmuxSession: "spwn-" + convID, ConvID: convID,
		Harness: harness.CopilotName, Status: session.StatusIdle, Cwd: t.TempDir(),
	}))
	t.Cleanup(func() { copilotAPISessions.Drop(convID) })

	// The port wait's real ceiling is 60s, and the branch under test is the one
	// it takes AT THE DEADLINE. A short context would not shorten the test — it
	// would make every run die on ctx.Done() instead, producing the generic
	// "gave up verifying" and never reaching the named verdict this test exists
	// for. So the WAIT is shortened and the context is left long enough to lose
	// the race to it.
	previousWait := copilotAPIStartupTimeout
	copilotAPIStartupTimeout = 1500 * time.Millisecond
	t.Cleanup(func() { copilotAPIStartupTimeout = previousWait })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := reconnectCopilotAPISession(ctx, convID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no live pane process was found",
		"the failure must name the pane rather than reporting some generic connection "+
			"or timeout problem, because the three port-wait verdicts have completely "+
			"different remedies and an operator sent to the wrong one loses the time this "+
			"message exists to save: %v", err)
	assert.False(t, copilotAPISessions.Connected(convID))
	assert.NotContains(t, fmt.Sprint(server.methodsCalled()), copilotapi.MethodConnect,
		"nothing may be sent to a port whose listener was never shown to belong to the "+
			"agent's pane — this endpoint has no authentication, so an unverified listener "+
			"cannot be told apart from another agent's")
}
