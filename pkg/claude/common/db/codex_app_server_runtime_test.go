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

func TestBindWarmingCodexAppServerRuntimeFromTUIIsGenerationAndResumeSafe(t *testing.T) {
	setupTestDB(t)
	old := CodexAppServerRuntime{
		Generation: "old", LaunchID: "launch", AgentID: "agent", SocketPath: "/tmp/old.sock",
		State: CodexAppServerWarming, CreatedAt: time.Now().Add(-time.Minute),
	}
	current := CodexAppServerRuntime{
		Generation: "current", LaunchID: "launch", AgentID: "agent", ConvID: "thread-1",
		SocketPath: "/tmp/current.sock", State: CodexAppServerWarming, CreatedAt: time.Now(),
	}
	require.NoError(t, UpsertCodexAppServerRuntime(old))
	require.NoError(t, UpsertCodexAppServerRuntime(current))

	changed, err := BindWarmingCodexAppServerRuntimeFromTUI("launch", "foreign-thread")
	require.NoError(t, err)
	assert.False(t, changed, "a resume must reject a hook for another thread")
	changed, err = BindWarmingCodexAppServerRuntimeFromTUI("launch", "thread-1")
	require.NoError(t, err)
	assert.True(t, changed)

	got, err := GetCodexAppServerRuntime("current")
	require.NoError(t, err)
	assert.Equal(t, "thread-1", got.ThreadID)
	stale, err := GetCodexAppServerRuntime("old")
	require.NoError(t, err)
	assert.Empty(t, stale.ThreadID, "only the newest warming generation may bind")
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

	changed, err = MarkCodexAppServerRuntimeTerminalIfUnreplaced(
		replacement.Generation, CodexAppServerDead, "current watcher")
	require.NoError(t, err)
	assert.True(t, changed)
	current, err := GetCodexAppServerRuntime(replacement.Generation)
	require.NoError(t, err)
	require.NotNil(t, current)
	assert.Equal(t, CodexAppServerDead, current.State)
}

func TestCodexAppServerStatusWritesRequireCurrentRuntimeAndSessionGeneration(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	session := &SessionRow{
		ID: "session-1", ConvID: "thread-1", TmuxSession: "tmux-1", Cwd: t.TempDir(),
		Status: "idle", Harness: "codex", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, SaveSession(session))
	old := CodexAppServerRuntime{
		Generation: "old-generation", LaunchID: "old-launch", AgentID: "agent-1",
		ConvID: session.ConvID, ThreadID: session.ConvID, SocketPath: "/tmp/old.sock",
		State: CodexAppServerReady, CreatedAt: now.Add(-time.Minute),
	}
	current := CodexAppServerRuntime{
		Generation: "current-generation", LaunchID: "current-launch", AgentID: "agent-1",
		ConvID: session.ConvID, ThreadID: session.ConvID, SocketPath: "/tmp/current.sock",
		State: CodexAppServerReady, CreatedAt: now,
	}
	require.NoError(t, UpsertCodexAppServerRuntime(old))
	require.NoError(t, UpsertCodexAppServerRuntime(current))

	changed, err := SetSessionStatusForCodexAppServerGeneration(
		session.ID, session.ConvID, session.CreatedAt, old.Generation,
		session.Status, session.UpdatedAt, "working", "obsolete", now.Add(time.Second))
	require.NoError(t, err)
	assert.False(t, changed)
	changed, err = SetSessionStatusForCodexAppServerGeneration(
		session.ID, session.ConvID, session.CreatedAt, current.Generation,
		session.Status, session.UpdatedAt, "working", "app-server snapshot", now.Add(time.Second))
	require.NoError(t, err)
	assert.True(t, changed)
	got, err := FindSessionByConvID(session.ConvID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "working", got.Status)
	assert.Equal(t, "app-server snapshot", got.StatusDetail)
	changed, err = SetSessionStatusForCodexAppServerGeneration(
		session.ID, session.ConvID, session.CreatedAt.Add(time.Second), current.Generation,
		got.Status, got.UpdatedAt, "idle", "recreated", now.Add(2*time.Second))
	require.NoError(t, err)
	assert.False(t, changed, "a recreated session generation must reject the old status observer")
}
