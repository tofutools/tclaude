package agentd

import (
	"encoding/json"
	"os"
	"path/filepath"
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

	select {
	case message := <-sim.Messages():
		t.Fatalf("bootstrap must bind without replay; unexpected RPC %q", message.Method)
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
