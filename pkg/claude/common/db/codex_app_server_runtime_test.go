package db

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInvalidateCodexAppServerRuntimesAfterRestartClearsLiveClaims(t *testing.T) {
	setupTestDB(t)
	for _, tc := range []struct {
		generation, state string
	}{
		{"warming-generation", CodexAppServerWarming},
		{"ready-generation", CodexAppServerReady},
		{"dead-generation", CodexAppServerDead},
	} {
		require.NoError(t, UpsertCodexAppServerRuntime(CodexAppServerRuntime{
			Generation: tc.generation, LaunchID: "launch-" + tc.generation,
			AgentID: "agent", SocketPath: filepath.Join(t.TempDir(), "app.sock"), State: tc.state,
		}))
	}

	require.NoError(t, InvalidateCodexAppServerRuntimesAfterRestart())
	for _, generation := range []string{"warming-generation", "ready-generation"} {
		runtime, err := GetCodexAppServerRuntime(generation)
		require.NoError(t, err)
		require.NotNil(t, runtime)
		assert.Equal(t, CodexAppServerUnavailable, runtime.State)
		assert.Contains(t, runtime.Detail, "agentd restarted")
	}
	dead, err := GetCodexAppServerRuntime("dead-generation")
	require.NoError(t, err)
	require.NotNil(t, dead)
	assert.Equal(t, CodexAppServerDead, dead.State)
}

func TestObsoleteCodexAppServerWatcherCannotSupersedeReadyReplacement(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	old := CodexAppServerRuntime{
		Generation: "old-generation", LaunchID: "old-launch", AgentID: "agent-1",
		ConvID: "thread-1", ThreadID: "thread-1", SocketPath: "/tmp/old.sock",
		State: CodexAppServerReady, CreatedAt: now.Add(-time.Minute),
	}
	replacement := CodexAppServerRuntime{
		Generation: "new-generation", LaunchID: "new-launch", AgentID: "agent-1",
		ConvID: "thread-1", ThreadID: "thread-1", SocketPath: "/tmp/new.sock",
		State: CodexAppServerReady, CreatedAt: now,
	}
	require.NoError(t, UpsertCodexAppServerRuntime(old))
	require.NoError(t, UpsertCodexAppServerRuntime(replacement))

	changed, err := MarkCodexAppServerRuntimeTerminalIfUnreplaced(
		old.Generation, CodexAppServerDead, "late watcher")
	require.NoError(t, err)
	assert.False(t, changed)
	latest, err := GetCodexAppServerRuntimeByConvID("thread-1")
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, replacement.Generation, latest.Generation)
}
