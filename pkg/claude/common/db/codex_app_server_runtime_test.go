package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestCodexAppServerRecoveryClaimIsLeasedAndCASProtected(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	runtime := CodexAppServerRuntime{
		Generation: "recovery-generation", LaunchID: "launch", AgentID: "agent",
		ConvID: "thread", ThreadID: "thread", SocketPath: "/tmp/app.sock",
		CodexVersion: "0.147.0", State: CodexAppServerReady,
	}
	require.NoError(t, UpsertCodexAppServerRuntime(runtime))
	claimed, err := ClaimCodexAppServerRuntimeRecovery(runtime.Generation, "daemon-one", now, time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = ClaimCodexAppServerRuntimeRecovery(runtime.Generation, "daemon-two", now, time.Minute)
	require.NoError(t, err)
	assert.False(t, claimed, "a second daemon must not adopt the live lease")
	claimed, err = ClaimCodexAppServerRuntimeRecovery(runtime.Generation, "daemon-two", now.Add(2*time.Minute), time.Minute)
	require.NoError(t, err)
	assert.True(t, claimed, "an abandoned claim becomes recoverable after its lease")
}

func TestCodexAppServerBootstrapCannotOverwriteRecoveryDecision(t *testing.T) {
	setupTestDB(t)
	runtime := CodexAppServerRuntime{
		Generation: "bootstrap-generation", LaunchID: "launch", AgentID: "agent",
		ConvID: "thread", SocketPath: "/tmp/bootstrap.sock", CodexVersion: "0.147.0",
		State: CodexAppServerWarming,
	}
	require.NoError(t, UpsertCodexAppServerRuntime(runtime))
	claimed, err := ClaimCodexAppServerRuntimeRecovery(
		runtime.Generation, "recovery-owner", time.Now().UTC(), time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)

	runtime.ThreadID = runtime.ConvID
	runtime.ServerPID = 42
	completed, err := CompleteCodexAppServerRuntimeBootstrap(runtime)
	require.NoError(t, err)
	assert.False(t, completed, "bootstrap must not resurrect a recovery-owned generation")
	failed, err := FailCodexAppServerRuntimeBootstrap(runtime.Generation, "late bootstrap failure")
	require.NoError(t, err)
	assert.False(t, failed, "bootstrap failure must not overwrite the recovery owner token")

	stored, err := GetCodexAppServerRuntime(runtime.Generation)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, CodexAppServerRecovering, stored.State)
	assert.Equal(t, "recovery-owner", stored.Detail)
}

func TestRecoverableCodexAppServerRuntimesSelectNewestGeneration(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	for _, runtime := range []CodexAppServerRuntime{
		{Generation: "old", LaunchID: "old-launch", AgentID: "agent", ConvID: "thread", ThreadID: "thread",
			SocketPath: "/tmp/old.sock", CodexVersion: "0.147.0", State: CodexAppServerReady, CreatedAt: now.Add(-time.Minute)},
		{Generation: "new", LaunchID: "new-launch", AgentID: "agent", ConvID: "thread", ThreadID: "thread",
			SocketPath: "/tmp/new.sock", CodexVersion: "0.147.0", State: CodexAppServerReady, CreatedAt: now},
	} {
		require.NoError(t, UpsertCodexAppServerRuntime(runtime))
	}
	runtimes, err := RecoverableCodexAppServerRuntimes()
	require.NoError(t, err)
	require.Len(t, runtimes, 1)
	assert.Equal(t, "new", runtimes[0].Generation)
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
