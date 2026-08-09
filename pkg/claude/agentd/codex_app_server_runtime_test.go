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

	initialize := nextCodexAppServerMessage(t, sim)
	assert.Equal(t, codexappserver.MethodInitialize, initialize.Method)
	initialized := nextCodexAppServerMessage(t, sim)
	assert.Equal(t, codexappserver.MethodInitialized, initialized.Method)
	list := nextCodexAppServerMessage(t, sim)
	require.Equal(t, codexappserver.MethodThreadLoadedList, list.Method)
	require.NoError(t, sim.Reply(list.ID, codexappserver.ThreadLoadedListResult{Data: []string{"thread-from-tui"}}))
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
