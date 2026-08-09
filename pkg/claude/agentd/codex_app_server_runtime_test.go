package agentd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/codexappserver"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestPrepareCodexAppServerRuntimeIsolatesAgents(t *testing.T) {
	resetTestDB(t)

	first := clcommon.SpawnArgs{CodexAppServer: true, AgentID: "agent-one", Label: "one"}
	second := clcommon.SpawnArgs{CodexAppServer: true, AgentID: "agent-two", Label: "two"}
	require.NoError(t, prepareCodexAppServerRuntime(&first))
	require.NoError(t, prepareCodexAppServerRuntime(&second))
	t.Cleanup(func() {
		removeCodexAppServerGeneration(first.CodexAppServerSocket)
		removeCodexAppServerGeneration(second.CodexAppServerSocket)
	})

	assert.NotEqual(t, first.CodexAppServerGeneration, second.CodexAppServerGeneration)
	assert.NotEqual(t, filepath.Dir(filepath.Dir(first.CodexAppServerSocket)),
		filepath.Dir(filepath.Dir(second.CodexAppServerSocket)), "agents need distinct owner directories")
	for _, socket := range []string{first.CodexAppServerSocket, second.CodexAppServerSocket} {
		info, err := os.Stat(filepath.Dir(socket))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
}

func TestSessionArgsCarryPrivateCodexAppServerGeneration(t *testing.T) {
	for name, args := range map[string][]string{
		"new": sessionNewArgs(clcommon.SpawnArgs{
			Label: "worker", Cwd: "/tmp/work", CodexAppServer: true,
			CodexAppServerGeneration: "generation-1",
			CodexAppServerSocket:     "/tmp/app.sock", CodexAppServerPIDFile: "/tmp/app.pid",
			CodexAppServerLogFile: "/tmp/app.log",
		}),
		"resume": sessionResumeArgs(clcommon.SpawnArgs{
			ConvID: "thread-1", Cwd: "/tmp/work", CodexAppServer: true,
			CodexAppServerGeneration: "generation-1",
			CodexAppServerSocket:     "/tmp/app.sock", CodexAppServerPIDFile: "/tmp/app.pid",
			CodexAppServerLogFile: "/tmp/app.log",
		}),
	} {
		t.Run(name, func(t *testing.T) {
			assert.True(t, slices.Contains(args, "--codex-app-server"))
			assert.True(t, slices.Contains(args, "generation-1"))
			assert.True(t, slices.Contains(args, "/tmp/app.sock"))
			assert.True(t, slices.Contains(args, "/tmp/app.pid"))
			assert.True(t, slices.Contains(args, "/tmp/app.log"))
		})
	}
	plain := sessionNewArgs(clcommon.SpawnArgs{Label: "worker", Cwd: "/tmp/work"})
	assert.False(t, slices.Contains(plain, "--codex-app-server"))
}

func TestAwaitCodexAppServerLaunchFailsFastBeforeConversationBind(t *testing.T) {
	resetTestDB(t)
	const (
		generation = "0123456789abcdef"
		launchID   = "spwn-successor"
	)
	require.NoError(t, db.UpsertCodexAppServerRuntime(db.CodexAppServerRuntime{
		Generation: generation, LaunchID: launchID, AgentID: "agent",
		SocketPath: filepath.Join(t.TempDir(), "app.sock"),
		State:      db.CodexAppServerUnavailable, Detail: "exact launch version incompatible",
	}))

	started := time.Now()
	assert.False(t, awaitCodexAppServerLaunch("not-bound-yet", launchID))
	assert.Less(t, time.Since(started), time.Second,
		"a failed successor must not wait out the readiness timeout when only its launch id is known")
}

func TestCodexAppServerBootstrapBindsTUIThreadWithoutReplay(t *testing.T) {
	resetTestDB(t)
	dir, err := os.MkdirTemp("/tmp", "tcl-codex-runtime-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	socket := filepath.Join(dir, "app.sock")
	sim, err := testharness.StartCodexAppServerSim(socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sim.Close() })
	pidFile := filepath.Join(dir, "server.pid")
	require.NoError(t, os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600))

	const generation = "generation-1"
	require.NoError(t, db.UpsertCodexAppServerRuntime(db.CodexAppServerRuntime{
		Generation:   generation,
		LaunchID:     "launch-1",
		AgentID:      "agent-1",
		SocketPath:   socket,
		CodexVersion: "0.147.0",
		State:        db.CodexAppServerWarming,
	}))

	done := make(chan struct{})
	go func() {
		runCodexAppServerBootstrap(clcommon.SpawnArgs{
			CodexAppServer:           true,
			CodexAppServerGeneration: generation,
			CodexAppServerPIDFile:    pidFile,
		})
		close(done)
	}()

	select {
	case message := <-sim.Messages():
		t.Fatalf("agentd initialized before TUI thread binding: %s", message.Method)
	case <-time.After(100 * time.Millisecond):
	}
	bound, err := db.BindWarmingCodexAppServerRuntimeFromTUI("launch-1", "thread-from-tui")
	require.NoError(t, err)
	require.True(t, bound)

	initialize := nextCodexAppServerMessage(t, sim)
	assert.Equal(t, codexappserver.MethodInitialize, initialize.Method)
	initialized := nextCodexAppServerMessage(t, sim)
	assert.Equal(t, codexappserver.MethodInitialized, initialized.Method)
	read := nextCodexAppServerMessage(t, sim)
	require.Equal(t, codexappserver.MethodThreadRead, read.Method)
	require.NoError(t, sim.Reply(read.ID, codexappserver.ThreadReadResult{Thread: codexappserver.Thread{
		ID: "thread-from-tui", Status: json.RawMessage(`"idle"`), Turns: []codexappserver.Turn{},
	}}))

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runtime bootstrap did not finish")
	}
	runtime, err := db.GetCodexAppServerRuntime(generation)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	assert.Equal(t, db.CodexAppServerReady, runtime.State)
	assert.Equal(t, "thread-from-tui", runtime.ConvID)
	assert.Equal(t, "thread-from-tui", runtime.ThreadID)

	// The observer may immediately issue the stable account snapshot read, but
	// it must never subscribe or replay the TUI-owned birth prompt.
	select {
	case message := <-sim.Messages():
		assert.Equal(t, codexappserver.MethodAccountRateLimitsRead, message.Method)
		require.NoError(t, sim.Reply(message.ID, codexappserver.AccountRateLimitsReadResult{}))
	case <-time.After(100 * time.Millisecond):
	}

	codexAppServerHandles.Lock()
	handle := codexAppServerHandles.byGeneration[generation]
	delete(codexAppServerHandles.byGeneration, generation)
	delete(codexAppServerHandles.byConv, "thread-from-tui")
	codexAppServerHandles.Unlock()
	if handle != nil {
		_ = handle.client.Close()
	}
}

func TestCodexAppServerDaemonRestartReadoptsExactLiveThread(t *testing.T) {
	resetTestDB(t)
	dir, err := os.MkdirTemp("/tmp", "tcl-codex-readopt-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	sim, err := testharness.StartCodexAppServerSim(filepath.Join(dir, "app.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sim.Close() })
	runtime := db.CodexAppServerRuntime{
		Generation: "restart-generation", LaunchID: "restart-launch", AgentID: "restart-agent",
		ConvID: "restart-thread", ThreadID: "restart-thread", SocketPath: sim.SocketPath(),
		ServerPID: os.Getpid(), CodexVersion: "0.147.0", State: db.CodexAppServerReady,
	}
	require.NoError(t, db.UpsertCodexAppServerRuntime(runtime))
	claimed, err := db.ClaimCodexAppServerRuntimeRecovery(
		runtime.Generation, "test-daemon", time.Now().UTC(), time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	previousAlive := codexAppServerConvAlive
	codexAppServerConvAlive = func(string) bool { return true }
	t.Cleanup(func() { codexAppServerConvAlive = previousAlive })

	done := make(chan struct{})
	go func() {
		recoverCodexAppServerRuntime(runtime, "test-daemon")
		close(done)
	}()
	require.Equal(t, codexappserver.MethodInitialize, nextCodexAppServerMessage(t, sim).Method)
	require.Equal(t, codexappserver.MethodInitialized, nextCodexAppServerMessage(t, sim).Method)
	read := nextCodexAppServerMessage(t, sim)
	require.Equal(t, codexappserver.MethodThreadRead, read.Method)
	require.NoError(t, sim.Reply(read.ID, codexappserver.ThreadReadResult{Thread: codexappserver.Thread{
		ID: runtime.ThreadID, Status: json.RawMessage(`{"type":"idle"}`), Turns: []codexappserver.Turn{},
	}}))
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon restart recovery did not finish")
	}
	stored, err := db.GetCodexAppServerRuntime(runtime.Generation)
	require.NoError(t, err)
	require.Equal(t, db.CodexAppServerReady, stored.State)
	handle := codexAppServerHandleForConv(runtime.ConvID)
	require.NotNil(t, handle)
	assert.Equal(t, runtime.Generation, handle.runtime.Generation)

	// The first observer call is an account snapshot only. Re-adoption must not
	// resume/subscribe the thread or submit any birth/durable prompt.
	rateRead := nextCodexAppServerMessage(t, sim)
	require.Equal(t, codexappserver.MethodAccountRateLimitsRead, rateRead.Method)
	require.NoError(t, sim.Reply(rateRead.ID, codexappserver.AccountRateLimitsReadResult{}))

	// A durable delivery whose write completed before agentd died is settled by
	// its stable client id on the re-adopted exact thread. Recovery must not
	// replay it as a second turn.
	deliveryDone := make(chan error, 1)
	go func() {
		deliveryDone <- sendCodexAppServerMessage(runtime.ConvID, 77, "[system: message #77] once")
	}()
	reconcileRead := nextCodexAppServerMessage(t, sim)
	require.Equal(t, codexappserver.MethodThreadRead, reconcileRead.Method)
	item := json.RawMessage(`{"type":"userMessage","clientId":"tclaude-agent-message-77","text":"[system: message #77] once"}`)
	require.NoError(t, sim.Reply(reconcileRead.ID, codexappserver.ThreadReadResult{Thread: codexappserver.Thread{
		ID: runtime.ThreadID, Status: json.RawMessage(`{"type":"idle"}`),
		Turns: []codexappserver.Turn{{ID: "completed-before-restart", Status: "completed", Items: []json.RawMessage{item}}},
	}}))
	require.NoError(t, <-deliveryDone)
	select {
	case message := <-sim.Messages():
		assert.NotEqual(t, codexappserver.MethodTurnStart, message.Method,
			"recovery must not duplicate a previously committed durable message")
	case <-time.After(100 * time.Millisecond):
	}
	handle.mutations.Lock()
	handle.closing = true
	_ = handle.client.Close()
	handle.mutations.Unlock()
}

func TestCodexAppServerDaemonRestartRejectsWrongThreadIdentity(t *testing.T) {
	resetTestDB(t)
	dir, err := os.MkdirTemp("/tmp", "tcl-codex-wrong-thread-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	sim, err := testharness.StartCodexAppServerSim(filepath.Join(dir, "app.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sim.Close() })
	runtime := db.CodexAppServerRuntime{
		Generation: "wrong-generation", LaunchID: "wrong-launch", AgentID: "wrong-agent",
		ConvID: "expected-thread", ThreadID: "expected-thread", SocketPath: sim.SocketPath(),
		ServerPID: os.Getpid(), CodexVersion: "0.147.0", State: db.CodexAppServerReady,
	}
	require.NoError(t, db.UpsertCodexAppServerRuntime(runtime))
	claimed, err := db.ClaimCodexAppServerRuntimeRecovery(runtime.Generation, "test-daemon", time.Now().UTC(), time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	previousAlive := codexAppServerConvAlive
	codexAppServerConvAlive = func(string) bool { return true }
	t.Cleanup(func() { codexAppServerConvAlive = previousAlive })
	previousTmux := clcommon.Default
	clcommon.Default = &commandRecordingTmux{}
	t.Cleanup(func() { clcommon.Default = previousTmux })

	done := make(chan struct{})
	go func() {
		recoverCodexAppServerRuntime(runtime, "test-daemon")
		close(done)
	}()
	require.Equal(t, codexappserver.MethodInitialize, nextCodexAppServerMessage(t, sim).Method)
	require.Equal(t, codexappserver.MethodInitialized, nextCodexAppServerMessage(t, sim).Method)
	read := nextCodexAppServerMessage(t, sim)
	require.NoError(t, sim.Reply(read.ID, codexappserver.ThreadReadResult{Thread: codexappserver.Thread{
		ID: "different-thread", Status: json.RawMessage(`{"type":"idle"}`), Turns: []codexappserver.Turn{},
	}}))
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("wrong-thread recovery did not finish")
	}
	stored, err := db.GetCodexAppServerRuntime(runtime.Generation)
	require.NoError(t, err)
	assert.Equal(t, db.CodexAppServerUnavailable, stored.State)
	assert.Contains(t, stored.Detail, "different-thread")
	assert.Nil(t, codexAppServerHandleForConv(runtime.ConvID))
}

func TestCodexAppServerDaemonRestartExpiresUnboundGeneration(t *testing.T) {
	resetTestDB(t)
	runtime := db.CodexAppServerRuntime{
		Generation: "unbound-generation", LaunchID: "unbound-launch", AgentID: "unbound-agent",
		ConvID: "unbound-thread", SocketPath: filepath.Join(t.TempDir(), "app.sock"),
		State: db.CodexAppServerWarming, CreatedAt: time.Now().Add(-time.Minute),
	}
	require.NoError(t, db.UpsertCodexAppServerRuntime(runtime))
	previousAlive := codexAppServerConvAlive
	codexAppServerConvAlive = func(string) bool { return false }
	t.Cleanup(func() { codexAppServerConvAlive = previousAlive })

	runCodexAppServerRecoverySweep()
	stored, err := db.GetCodexAppServerRuntime(runtime.Generation)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, db.CodexAppServerUnavailable, stored.State)
	assert.Contains(t, stored.Detail, "validated Codex TUI binding")
}

func TestCodexAppServerClientFailureReconnectsSameGeneration(t *testing.T) {
	resetTestDB(t)
	dir, err := os.MkdirTemp("/tmp", "tcl-codex-reconnect-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	sim, err := testharness.StartCodexAppServerSim(filepath.Join(dir, "app.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sim.Close() })
	client, err := codexappserver.Dial(context.Background(), sim.SocketPath(),
		&codexappserver.Options{CodexVersion: "0.147.0"})
	require.NoError(t, err)
	require.Equal(t, codexappserver.MethodInitialize, nextCodexAppServerMessage(t, sim).Method)
	require.Equal(t, codexappserver.MethodInitialized, nextCodexAppServerMessage(t, sim).Method)
	runtime := db.CodexAppServerRuntime{
		Generation: "reconnect-generation", LaunchID: "reconnect-launch", AgentID: "reconnect-agent",
		ConvID: "reconnect-thread", ThreadID: "reconnect-thread", SocketPath: sim.SocketPath(),
		ServerPID: os.Getpid(), CodexVersion: "0.147.0", State: db.CodexAppServerReady,
	}
	require.NoError(t, db.UpsertCodexAppServerRuntime(runtime))
	handle := registerCodexAppServerHandle(runtime, client)
	go watchCodexAppServerHandle(handle)
	firstRate := nextCodexAppServerMessage(t, sim)
	require.Equal(t, codexappserver.MethodAccountRateLimitsRead, firstRate.Method)
	require.NoError(t, sim.Reply(firstRate.ID, codexappserver.AccountRateLimitsReadResult{}))

	require.NoError(t, sim.CloseClient())
	require.Equal(t, codexappserver.MethodInitialize, nextCodexAppServerMessage(t, sim).Method)
	require.Equal(t, codexappserver.MethodInitialized, nextCodexAppServerMessage(t, sim).Method)
	read := nextCodexAppServerMessage(t, sim)
	require.Equal(t, codexappserver.MethodThreadRead, read.Method)
	require.NoError(t, sim.Reply(read.ID, codexappserver.ThreadReadResult{Thread: codexappserver.Thread{
		ID: runtime.ThreadID, Status: json.RawMessage(`{"type":"idle"}`), Turns: []codexappserver.Turn{},
	}}))
	secondRate := nextCodexAppServerMessage(t, sim)
	require.Equal(t, codexappserver.MethodAccountRateLimitsRead, secondRate.Method)
	require.NoError(t, sim.Reply(secondRate.ID, codexappserver.AccountRateLimitsReadResult{}))
	assert.Same(t, handle, codexAppServerHandleForConv(runtime.ConvID))
	stored, err := db.GetCodexAppServerRuntime(runtime.Generation)
	require.NoError(t, err)
	assert.Equal(t, db.CodexAppServerReady, stored.State)
	handle.mutations.Lock()
	handle.closing = true
	_ = handle.client.Close()
	handle.mutations.Unlock()
}

func TestCodexAppServerBootstrapNeverJoinsFreshThreadSubscriberSet(t *testing.T) {
	for _, ordering := range []struct {
		name           string
		bootstrapFirst bool
	}{
		{name: "bootstrap goroutine starts first", bootstrapFirst: true},
		{name: "TUI creates thread first", bootstrapFirst: false},
	} {
		for _, requestMethod := range []string{
			codexappserver.MethodCommandApproval,
			codexappserver.MethodRequestUserInput,
		} {
			t.Run(ordering.name+"/"+requestMethod, func(t *testing.T) {
				resetTestDB(t)
				dir, err := os.MkdirTemp("/tmp", "tcl-codex-order-")
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
				sim, err := testharness.StartCodexAppServerSim(filepath.Join(dir, "app.sock"))
				require.NoError(t, err)
				t.Cleanup(func() { _ = sim.Close() })
				pidFile := filepath.Join(dir, "server.pid")
				require.NoError(t, os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600))

				generation := "generation-" + strconv.FormatBool(ordering.bootstrapFirst) + "-" +
					strconv.Itoa(len(requestMethod))
				require.NoError(t, db.UpsertCodexAppServerRuntime(db.CodexAppServerRuntime{
					Generation: generation, LaunchID: "launch", AgentID: "agent",
					SocketPath: sim.SocketPath(), CodexVersion: "0.147.0", State: db.CodexAppServerWarming,
				}))
				args := clcommon.SpawnArgs{CodexAppServer: true, CodexAppServerGeneration: generation,
					CodexAppServerPIDFile: pidFile}
				done := make(chan struct{})
				startBootstrap := func() {
					go func() {
						runCodexAppServerBootstrap(args)
						close(done)
					}()
				}
				if ordering.bootstrapFirst {
					startBootstrap()
					select {
					case message := <-sim.Messages():
						t.Fatalf("bootstrap initialized before the TUI binding: %s", message.Method)
					case <-time.After(50 * time.Millisecond):
					}
				}

				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				tui, err := codexappserver.Dial(ctx, sim.SocketPath(), nil)
				require.NoError(t, err)
				t.Cleanup(func() { _ = tui.Close() })
				tuiInitialize := nextCodexAppServerMessage(t, sim)
				require.Equal(t, codexappserver.MethodInitialize, tuiInitialize.Method)
				tuiInitialized := nextCodexAppServerMessage(t, sim)
				require.Equal(t, codexappserver.MethodInitialized, tuiInitialized.Method)
				require.Equal(t, tuiInitialize.ConnectionID, tuiInitialized.ConnectionID)

				subscribed := sim.CreateFreshThread()
				require.Equal(t, []int64{tuiInitialize.ConnectionID}, subscribed,
					"the fresh thread must auto-subscribe only the already-initialized TUI")
				bound, err := db.BindWarmingCodexAppServerRuntimeFromTUI("launch", "thread")
				require.NoError(t, err)
				require.True(t, bound)
				if !ordering.bootstrapFirst {
					startBootstrap()
				}

				agentInitialize := nextCodexAppServerMessage(t, sim)
				require.Equal(t, codexappserver.MethodInitialize, agentInitialize.Method)
				require.NotEqual(t, tuiInitialize.ConnectionID, agentInitialize.ConnectionID)
				require.Equal(t, codexappserver.MethodInitialized, nextCodexAppServerMessage(t, sim).Method)
				read := nextCodexAppServerMessage(t, sim)
				require.Equal(t, codexappserver.MethodThreadRead, read.Method)
				require.NoError(t, sim.Reply(read.ID, codexappserver.ThreadReadResult{Thread: codexappserver.Thread{
					ID: "thread", Status: json.RawMessage(`"idle"`), Turns: []codexappserver.Turn{},
				}}))
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("bootstrap did not finish")
				}
				if rateRead := nextCodexAppServerMessage(t, sim); rateRead.Method == codexappserver.MethodAccountRateLimitsRead {
					require.NoError(t, sim.Reply(rateRead.ID, codexappserver.AccountRateLimitsReadResult{}))
				}

				recipients, err := sim.SendRequestToSubscribers(requestMethod, map[string]string{"threadId": "thread"})
				require.NoError(t, err)
				require.Equal(t, []int64{tuiInitialize.ConnectionID}, recipients)
				select {
				case request := <-tui.ServerRequests():
					require.Equal(t, requestMethod, request.Method)
				case <-time.After(time.Second):
					t.Fatal("TUI subscriber did not receive the request")
				}
				time.Sleep(50 * time.Millisecond)
				runtime, err := db.GetCodexAppServerRuntime(generation)
				require.NoError(t, err)
				require.Equal(t, db.CodexAppServerReady, runtime.State,
					"the post-bind agentd connection must not receive or quarantine TUI requests")

				codexAppServerHandles.Lock()
				handle := codexAppServerHandles.byGeneration[generation]
				delete(codexAppServerHandles.byGeneration, generation)
				delete(codexAppServerHandles.byConv, "thread")
				codexAppServerHandles.Unlock()
				if handle != nil {
					_ = handle.client.Close()
				}
			})
		}
	}
}

func TestCodexAppServerObserverQuarantinesUnexpectedRequestWithVisibleMethod(t *testing.T) {
	resetTestDB(t)
	dir, err := os.MkdirTemp("/tmp", "tcl-codex-observer-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	sim, err := testharness.StartCodexAppServerSim(filepath.Join(dir, "app.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sim.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := codexappserver.Dial(ctx, sim.SocketPath(), nil)
	require.NoError(t, err)
	initialize := nextCodexAppServerMessage(t, sim)
	require.Equal(t, codexappserver.MethodInitialize, initialize.Method)
	initialized := nextCodexAppServerMessage(t, sim)
	require.Equal(t, codexappserver.MethodInitialized, initialized.Method)

	runtime := db.CodexAppServerRuntime{
		Generation: "request-generation", LaunchID: "request-launch", AgentID: "request-agent",
		ConvID: "request-thread", ThreadID: "request-thread", SocketPath: sim.SocketPath(),
		State: db.CodexAppServerReady,
	}
	require.NoError(t, db.UpsertCodexAppServerRuntime(runtime))
	handle := &codexAppServerHandle{runtime: runtime, client: client}
	go watchCodexAppServerHandle(handle)
	rateRead := nextCodexAppServerMessage(t, sim)
	require.Equal(t, codexappserver.MethodAccountRateLimitsRead, rateRead.Method,
		"the observer must use a non-subscribing snapshot")
	require.NoError(t, sim.Reply(rateRead.ID, codexappserver.AccountRateLimitsReadResult{}))

	_, err = sim.SendRequest(codexappserver.MethodPermissionsApproval,
		map[string]string{"threadId": runtime.ThreadID})
	require.NoError(t, err)
	deadline := time.Now().Add(3 * time.Second)
	for {
		stored, getErr := db.GetCodexAppServerRuntime(runtime.Generation)
		require.NoError(t, getErr)
		if stored != nil && stored.State == db.CodexAppServerUnavailable {
			assert.Contains(t, stored.Detail, codexappserver.MethodPermissionsApproval)
			assert.Contains(t, stored.Detail, "non-subscribing observer")
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("observer did not quarantine the unexpected request")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func nextCodexAppServerMessage(t *testing.T, sim *testharness.CodexAppServerSim) testharness.CodexAppServerMessage {
	t.Helper()
	select {
	case message := <-sim.Messages():
		return message
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for fake Codex app-server message")
		return testharness.CodexAppServerMessage{}
	}
}
