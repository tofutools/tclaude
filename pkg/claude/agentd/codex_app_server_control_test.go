package agentd

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/codexappserver"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type codexControlFixture struct {
	convID string
	sim    *testharness.CodexAppServerSim
	tmux   *commandRecordingTmux

	mu        sync.Mutex
	thread    codexappserver.Thread
	methods   []string
	drop      map[string]bool
	fail      map[string]bool
	clientIDs []string
}

func newCodexControlFixture(t *testing.T) *codexControlFixture {
	t.Helper()
	resetTestDB(t)
	t.Cleanup(SetInjectSettleDelayForTest(0))
	tmux := &commandRecordingTmux{}
	previousTmux := clcommon.Default
	clcommon.Default = tmux
	t.Cleanup(func() { clcommon.Default = previousTmux })

	const convID = "019ec113-1000-7000-8000-000000000001"
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: "codex-control-session", TmuxSession: "codex-control-pane", ConvID: convID,
		Harness: harness.CodexName, Status: session.StatusIdle, Cwd: t.TempDir(),
	}))
	require.NoError(t, db.SetConversationCodexAppServer(
		convID, harness.CodexName, t.TempDir(), true))
	dir, err := os.MkdirTemp("/tmp", "tcl1131-control-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	sim, err := testharness.StartCodexAppServerSim(filepath.Join(dir, "app.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sim.Close() })
	client, err := codexappserver.Dial(context.Background(), sim.SocketPath(),
		&codexappserver.Options{CodexVersion: "0.147.0"})
	require.NoError(t, err)
	initialize := nextCodexAppServerMessage(t, sim)
	require.Equal(t, codexappserver.MethodInitialize, initialize.Method)
	initialized := nextCodexAppServerMessage(t, sim)
	require.Equal(t, codexappserver.MethodInitialized, initialized.Method)

	runtime := db.CodexAppServerRuntime{
		Generation: "control-generation", LaunchID: "control-launch", AgentID: "control-agent",
		ConvID: convID, ThreadID: convID, SocketPath: sim.SocketPath(), ServerPID: os.Getpid(),
		CodexVersion: "0.147.0", State: db.CodexAppServerReady,
	}
	require.NoError(t, db.UpsertCodexAppServerRuntime(runtime))
	handle := &codexAppServerHandle{runtime: runtime, client: client}
	codexAppServerHandles.Lock()
	codexAppServerHandles.byConv[convID] = handle
	codexAppServerHandles.byGeneration[runtime.Generation] = handle
	codexAppServerHandles.Unlock()
	t.Cleanup(func() {
		codexAppServerHandles.Lock()
		delete(codexAppServerHandles.byConv, convID)
		delete(codexAppServerHandles.byGeneration, runtime.Generation)
		codexAppServerHandles.Unlock()
		_ = client.Close()
	})

	f := &codexControlFixture{
		convID: convID, sim: sim, tmux: tmux, drop: map[string]bool{}, fail: map[string]bool{},
		thread: codexappserver.Thread{
			ID: convID, Status: json.RawMessage(`{"type":"idle"}`), Turns: []codexappserver.Turn{},
		},
	}
	go f.serve()
	return f
}

func (f *codexControlFixture) serve() {
	for message := range f.sim.Messages() {
		f.mu.Lock()
		f.methods = append(f.methods, message.Method)
		drop := f.drop[message.Method]
		fail := f.fail[message.Method]
		f.mu.Unlock()
		switch message.Method {
		case codexappserver.MethodThreadRead:
			_ = f.sim.Reply(message.ID, codexappserver.ThreadReadResult{Thread: f.threadSnapshot()})
		case codexappserver.MethodTurnStart, codexappserver.MethodTurnSteer:
			var params struct {
				Input               []codexappserver.UserInput `json:"input"`
				ClientUserMessageID *string                    `json:"clientUserMessageId"`
			}
			_ = json.Unmarshal(message.Params, &params)
			clientID := ""
			if params.ClientUserMessageID != nil {
				clientID = *params.ClientUserMessageID
			}
			f.mu.Lock()
			f.clientIDs = append(f.clientIDs, clientID)
			f.mu.Unlock()
			if fail {
				_ = f.sim.ReplyError(message.ID, codexappserver.RPCError{Code: -32000, Message: "test refusal"})
				continue
			}
			item, _ := json.Marshal(map[string]any{
				"id": "item-" + clientID, "type": "userMessage", "clientId": clientID,
				"content": params.Input,
			})
			f.mu.Lock()
			if message.Method == codexappserver.MethodTurnStart {
				f.thread.Status = json.RawMessage(`{"type":"active","activeFlags":[]}`)
				f.thread.Turns = append(f.thread.Turns, codexappserver.Turn{
					ID: "turn-started", Status: "inProgress", Items: []json.RawMessage{item},
				})
			} else {
				last := len(f.thread.Turns) - 1
				f.thread.Turns[last].Items = append(f.thread.Turns[last].Items, item)
			}
			f.mu.Unlock()
			if !drop {
				if message.Method == codexappserver.MethodTurnStart {
					_ = f.sim.Reply(message.ID, codexappserver.TurnStartResult{Turn: codexappserver.Turn{
						ID: "turn-started", Status: "inProgress", Items: []json.RawMessage{},
					}})
				} else {
					_ = f.sim.Reply(message.ID, codexappserver.TurnSteerResult{TurnID: "turn-started"})
				}
			}
		case codexappserver.MethodThreadNameSet:
			var params codexappserver.ThreadNameSetParams
			_ = json.Unmarshal(message.Params, &params)
			f.mu.Lock()
			f.thread.Name = &params.Name
			f.mu.Unlock()
			if !drop {
				_ = f.sim.Reply(message.ID, map[string]any{})
			}
		case codexappserver.MethodThreadCompactStart:
			compaction := json.RawMessage(`{"id":"compact-1","type":"contextCompaction"}`)
			f.mu.Lock()
			f.thread.Turns = append(f.thread.Turns, codexappserver.Turn{
				ID: "compact-turn", Status: "completed", Items: []json.RawMessage{compaction},
			})
			f.thread.Status = json.RawMessage(`{"type":"idle"}`)
			f.mu.Unlock()
			if !drop {
				_ = f.sim.Reply(message.ID, map[string]any{})
			}
		case codexappserver.MethodTurnInterrupt:
			f.mu.Lock()
			for i := range f.thread.Turns {
				if f.thread.Turns[i].Status == "inProgress" {
					f.thread.Turns[i].Status = "interrupted"
				}
			}
			f.thread.Status = json.RawMessage(`{"type":"idle"}`)
			f.mu.Unlock()
			if !drop {
				_ = f.sim.Reply(message.ID, map[string]any{})
			}
		}
	}
}

func (f *codexControlFixture) threadSnapshot() codexappserver.Thread {
	f.mu.Lock()
	defer f.mu.Unlock()
	thread := f.thread
	thread.Status = append(json.RawMessage(nil), f.thread.Status...)
	thread.Turns = make([]codexappserver.Turn, len(f.thread.Turns))
	copy(thread.Turns, f.thread.Turns)
	for i := range thread.Turns {
		thread.Turns[i].Items = make([]json.RawMessage, len(f.thread.Turns[i].Items))
		copy(thread.Turns[i].Items, f.thread.Turns[i].Items)
		for j := range thread.Turns[i].Items {
			thread.Turns[i].Items[j] = append(json.RawMessage(nil), f.thread.Turns[i].Items[j]...)
		}
	}
	if f.thread.Name != nil {
		name := *f.thread.Name
		thread.Name = &name
	}
	return thread
}

func (f *codexControlFixture) methodCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, got := range f.methods {
		if got == method {
			count++
		}
	}
	return count
}

func (f *codexControlFixture) submittedClientIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.clientIDs...)
}

func (f *codexControlFixture) assertNoPaneInput(t *testing.T) {
	t.Helper()
	for _, command := range f.tmux.snapshot() {
		if len(command) == 0 {
			continue
		}
		assert.NotContains(t, []string{"send-keys", "set-buffer", "paste-buffer"}, command[0], command)
	}
}

func TestCodexAppServerProductionMessageRouteIsTypedAndIdempotent(t *testing.T) {
	f := newCodexControlFixture(t)
	message := &db.AgentMessage{ID: 42, ToConv: f.convID}
	const nudge = "[system: new agent message #42; delivery: inline] hello"
	require.True(t, sendNudgeBracket(f.convID, message, nudge))
	require.True(t, sendNudgeBracket(f.convID, message, nudge), "durable completion-stamp retry")
	assert.Equal(t, 1, f.methodCount(codexappserver.MethodTurnStart), "stable message id prevents a duplicate turn")
	f.assertNoPaneInput(t)
}

func TestCodexAppServerAmbiguousMessageIsReconciledWithoutDuplicate(t *testing.T) {
	f := newCodexControlFixture(t)
	f.mu.Lock()
	f.drop[codexappserver.MethodTurnStart] = true
	f.mu.Unlock()
	previousCall := codexAppServerCallTimeout
	previousMutation := codexAppServerMutationTimeout
	codexAppServerCallTimeout = 100 * time.Millisecond
	codexAppServerMutationTimeout = time.Second
	t.Cleanup(func() {
		codexAppServerCallTimeout = previousCall
		codexAppServerMutationTimeout = previousMutation
	})

	require.NoError(t, sendCodexAppServerMessage(f.convID, 43, "[msg #43 from peer] hello"))
	assert.Equal(t, 1, f.methodCount(codexappserver.MethodTurnStart),
		"thread/read found the committed input, so the ambiguous call was not replayed")
	f.assertNoPaneInput(t)
}

func TestCodexAppServerBusyMessageHoldsWithoutMutationOrFallback(t *testing.T) {
	f := newCodexControlFixture(t)
	f.mu.Lock()
	f.thread.Status = json.RawMessage(`{"type":"active","activeFlags":["waitingOnApproval"]}`)
	f.thread.Turns = []codexappserver.Turn{{ID: "turn-approval", Status: "inProgress", Items: []json.RawMessage{}}}
	f.mu.Unlock()

	err := sendCodexAppServerMessage(f.convID, 44, "[msg #44 from peer] wait")
	assert.ErrorIs(t, err, errCodexControlBusy)
	assert.Zero(t, f.methodCount(codexappserver.MethodTurnSteer))
	assert.Zero(t, f.methodCount(codexappserver.MethodTurnStart))
	f.assertNoPaneInput(t)
}

func TestCodexAppServerRenameCompactAndInterruptUseTypedMutations(t *testing.T) {
	f := newCodexControlFixture(t)
	require.True(t, deliverRename(f.convID, "typed-title"))
	require.NoError(t, compactCodexAppServerThread(f.convID, ""))
	f.mu.Lock()
	f.thread.Status = json.RawMessage(`{"type":"active","activeFlags":[]}`)
	f.thread.Turns = append(f.thread.Turns, codexappserver.Turn{
		ID: "turn-to-interrupt", Status: "inProgress", Items: []json.RawMessage{},
	})
	f.mu.Unlock()
	require.NoError(t, interruptCodexAppServerThread(f.convID))

	assert.Equal(t, 1, f.methodCount(codexappserver.MethodThreadNameSet))
	assert.Equal(t, 1, f.methodCount(codexappserver.MethodThreadCompactStart))
	assert.Equal(t, 1, f.methodCount(codexappserver.MethodTurnInterrupt))
	f.assertNoPaneInput(t)
}

func TestCodexAppServerPerThreadArbitrationStartsOnceThenSteers(t *testing.T) {
	f := newCodexControlFixture(t)
	errs := make(chan error, 2)
	go func() { errs <- sendCodexAppServerMessage(f.convID, 50, "[msg #50] first") }()
	go func() { errs <- sendCodexAppServerMessage(f.convID, 51, "[msg #51] second") }()
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	assert.Equal(t, 1, f.methodCount(codexappserver.MethodTurnStart))
	assert.Equal(t, 1, f.methodCount(codexappserver.MethodTurnSteer))
	f.assertNoPaneInput(t)
}

func TestCodexAppServerAmbiguousCompactAndInterruptReconcileBySnapshot(t *testing.T) {
	f := newCodexControlFixture(t)
	previousCall := codexAppServerCallTimeout
	codexAppServerCallTimeout = 100 * time.Millisecond
	t.Cleanup(func() { codexAppServerCallTimeout = previousCall })
	f.mu.Lock()
	f.drop[codexappserver.MethodThreadCompactStart] = true
	f.mu.Unlock()
	require.NoError(t, compactCodexAppServerThread(f.convID, ""),
		"new contextCompaction item proves the timed-out request landed")

	f.mu.Lock()
	f.drop[codexappserver.MethodTurnInterrupt] = true
	f.thread.Status = json.RawMessage(`{"type":"active","activeFlags":[]}`)
	f.thread.Turns = append(f.thread.Turns, codexappserver.Turn{
		ID: "ambiguous-interrupt", Status: "inProgress", Items: []json.RawMessage{},
	})
	f.mu.Unlock()
	require.NoError(t, interruptCodexAppServerThread(f.convID),
		"the active turn disappearing proves the timed-out interrupt landed")
	assert.Equal(t, 1, f.methodCount(codexappserver.MethodThreadCompactStart))
	assert.Equal(t, 1, f.methodCount(codexappserver.MethodTurnInterrupt))
}

func TestCodexAppServerInterruptProductionHandler(t *testing.T) {
	f := newCodexControlFixture(t)
	f.mu.Lock()
	f.thread.Status = json.RawMessage(`{"type":"active","activeFlags":[]}`)
	f.thread.Turns = []codexappserver.Turn{{
		ID: "handler-turn", Status: "inProgress", Items: []json.RawMessage{},
	}}
	f.mu.Unlock()
	recorder := httptest.NewRecorder()
	runCodexInterrupt(recorder, f.convID, "")
	assert.Equal(t, 200, recorder.Code, recorder.Body.String())
	assert.Equal(t, 1, f.methodCount(codexappserver.MethodTurnInterrupt))
	f.assertNoPaneInput(t)
}

func TestCodexAppServerUnavailableMessageNeverFallsBackToPane(t *testing.T) {
	f := newCodexControlFixture(t)
	codexAppServerHandles.Lock()
	delete(codexAppServerHandles.byConv, f.convID)
	delete(codexAppServerHandles.byGeneration, "control-generation")
	codexAppServerHandles.Unlock()
	runtime, err := db.GetCodexAppServerRuntime("control-generation")
	require.NoError(t, err)
	runtime.State = db.CodexAppServerUnavailable
	runtime.Detail = "test disconnect"
	require.NoError(t, db.UpsertCodexAppServerRuntime(*runtime))

	assert.False(t, sendNudgeBracket(f.convID, &db.AgentMessage{ID: 60, ToConv: f.convID}, "[msg #60] hold"))
	f.assertNoPaneInput(t)
}

func TestCodexAppServerExplicitFalseRestoresOrdinaryPaneRouting(t *testing.T) {
	f := newCodexControlFixture(t)
	require.NoError(t, db.SetConversationCodexAppServer(
		f.convID, harness.CodexName, t.TempDir(), false))
	selected, err := codexAppServerSelected(f.convID)
	require.NoError(t, err)
	assert.False(t, selected,
		"current explicit false must outrank the historical ready runtime")
	require.True(t, sendNudgeBracket(f.convID,
		&db.AgentMessage{ID: 61, ToConv: f.convID}, "[msg #61] ordinary"))
	assert.Zero(t, f.methodCount(codexappserver.MethodTurnStart))
	foundPaneInput := false
	for _, command := range f.tmux.snapshot() {
		if len(command) > 0 && (command[0] == "set-buffer" || command[0] == "send-keys") {
			foundPaneInput = true
		}
	}
	assert.True(t, foundPaneInput, "ordinary Codex routing should be restored")
}

func TestCodexAppServerCompactionRetryResumesStableFollowUpStage(t *testing.T) {
	f := newCodexControlFixture(t)
	f.mu.Lock()
	f.fail[codexappserver.MethodTurnStart] = true
	f.mu.Unlock()

	err := compactCodexAppServerThread(f.convID, "continue after compact")
	require.ErrorContains(t, err, "compaction committed; follow-up pending")
	assert.Equal(t, 1, f.methodCount(codexappserver.MethodThreadCompactStart))
	firstIDs := f.submittedClientIDs()
	require.Len(t, firstIDs, 1)

	f.mu.Lock()
	f.fail[codexappserver.MethodTurnStart] = false
	f.mu.Unlock()
	require.NoError(t, compactCodexAppServerThread(f.convID, "continue after compact"))
	assert.Equal(t, 1, f.methodCount(codexappserver.MethodThreadCompactStart),
		"retry after a follow-up failure must not compact again")
	ids := f.submittedClientIDs()
	require.Len(t, ids, 2)
	assert.Equal(t, ids[0], ids[1], "the retried follow-up keeps one stable identity")
}

func TestCodexThreadContainsMessageDoesNotReuseOtherClientIdentity(t *testing.T) {
	item := json.RawMessage(`{"type":"userMessage","clientId":"older-operation","text":"same follow-up"}`)
	thread := codexappserver.Thread{Turns: []codexappserver.Turn{{Items: []json.RawMessage{item}}}}
	assert.False(t, codexThreadContainsMessage(thread, "new-operation", "same follow-up"))
	assert.True(t, codexThreadContainsMessage(thread, "older-operation", "same follow-up"))

	legacy := json.RawMessage(`{"type":"userMessage","text":"same follow-up"}`)
	thread.Turns[0].Items = []json.RawMessage{legacy}
	assert.True(t, codexThreadContainsMessage(thread, "new-operation", "same follow-up"),
		"unscoped legacy items retain framed-text reconciliation")
}

func TestCodexAppServerRenameKeepsRoutedCharsetGate(t *testing.T) {
	f := newCodexControlFixture(t)
	assert.False(t, deliverRenameOn(f.convID, "unsafe\ntitle", deliveryChannelRouted))
	assert.Zero(t, f.methodCount(codexappserver.MethodThreadNameSet))
}
