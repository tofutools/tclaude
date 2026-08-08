package agentd

import (
	"encoding/json"
	"os"
	"os/exec"
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

	assert.Equal(t, slashTransportCopilotAPI,
		dispatchSlashCommand(fixture.convID, "/compact", "carry on", "compact"))

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
func TestCopilotAPIUnmappedLifecycleCommandFailsClosed(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)

	assert.Equal(t, slashTransportNone,
		dispatchSlashCommand(fixture.convID, "/exit", "", "soft-exit"))
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
	assert.Equal(t, slashTransportNone,
		dispatchSlashCommand(fixture.convID, "/compact", "", "compact"))
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

	assert.Equal(t, slashTransportSendKeys,
		dispatchSlashCommand(convID, "/compact", "", "compact"))
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
