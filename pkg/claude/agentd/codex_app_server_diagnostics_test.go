package agentd

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func seedCodexDiagnosticAgent(t *testing.T, convID string, selected bool) {
	t.Helper()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "session-" + convID, TmuxSession: "tmux-" + convID, Cwd: t.TempDir(),
		ConvID: convID, Harness: harness.CodexName, CreatedAt: time.Now().UTC(),
	}))
	_, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentCodexAppServerSelectionForConv(convID, selected, "explicit"))
}

func TestCodexAppServerDiagnosticDefaultOffAndCapabilityGated(t *testing.T) {
	resetTestDB(t)
	seedCodexDiagnosticAgent(t, "codex-sendkeys", false)

	got, err := codexAppServerDiagnosticForConv("codex-sendkeys", time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, "send-keys", got.Drive)
	assert.Equal(t, "explicit", got.DriveSource)
	assert.Equal(t, "ready", got.Health)
	assert.Contains(t, got.MessageDelivery, "send-keys")

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "session-claude", ConvID: "claude-agent", Cwd: t.TempDir(), Harness: harness.DefaultName,
	}))
	got, err = codexAppServerDiagnosticForConv("claude-agent", time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, "unsupported", got.Drive)
	assert.Equal(t, "not-applicable", got.Health)
}

func TestCodexAppServerDiagnosticFailureIsActionableAndRedacted(t *testing.T) {
	resetTestDB(t)
	const convID = "codex-failed"
	seedCodexDiagnosticAgent(t, convID, true)
	runtime := db.CodexAppServerRuntime{
		Generation: "generation-secret", LaunchID: "launch-secret", AgentID: "agent-secret",
		ConvID: convID, ThreadID: convID, SocketPath: "/home/operator/.tclaude/api/codex/private/app.sock",
		ServerPID: 4242, CodexVersion: "0.147.3", State: db.CodexAppServerUnavailable,
		Detail: "dial unix:/home/operator/.tclaude/api/codex/private/app.sock: connection refused",
	}
	require.NoError(t, db.UpsertCodexAppServerRuntime(runtime))

	got, err := codexAppServerDiagnosticForConv(convID, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, "app-server", got.Drive)
	assert.Equal(t, "disconnected", got.Health)
	assert.Equal(t, "0.147.3", got.CodexVersion)
	assert.Equal(t, 4242, got.ServerPID)
	assert.Equal(t, "bound and verified against thread/read", got.ThreadBinding)
	assert.Contains(t, got.MessageDelivery, "held")
	assert.Contains(t, got.Rollback, "--send-keys")
	assert.NotContains(t, got.Detail, "/home/operator")
	assert.Contains(t, got.Detail, "<private path>")
	assert.False(t, strings.Contains(got.SocketIdentity, runtime.SocketPath))
}

func TestCodexAppServerDiagnosticReadyAndStaleHealth(t *testing.T) {
	resetTestDB(t)
	const convID = "codex-ready"
	seedCodexDiagnosticAgent(t, convID, true)
	runtime := db.CodexAppServerRuntime{
		Generation: "generation-ready", LaunchID: "launch-ready", AgentID: "agent-ready",
		ConvID: convID, ThreadID: convID, SocketPath: "/tmp/private/app.sock",
		ServerPID: 4243, CodexVersion: "0.147.0", State: db.CodexAppServerReady,
	}
	require.NoError(t, db.UpsertCodexAppServerRuntime(runtime))
	handle := &codexAppServerHandle{runtime: runtime}
	handle.observation.statusAt = time.Now().UTC()
	handle.observation.usageAt = time.Now().Add(-time.Minute).UTC()
	codexAppServerHandles.Lock()
	codexAppServerHandles.byConv[convID] = handle
	codexAppServerHandles.byGeneration[runtime.Generation] = handle
	codexAppServerHandles.Unlock()
	t.Cleanup(func() {
		codexAppServerHandles.Lock()
		delete(codexAppServerHandles.byConv, convID)
		delete(codexAppServerHandles.byGeneration, runtime.Generation)
		codexAppServerHandles.Unlock()
	})

	got, err := codexAppServerDiagnosticForConv(convID, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, "ready", got.Health)
	assert.Equal(t, "connected to the verified generation", got.ClientConnection)
	assert.NotEmpty(t, got.StatusObservedAt)
	assert.Contains(t, got.MessageDelivery, "typed RPC")

	got, err = codexAppServerDiagnosticForConv(convID,
		handle.observation.statusAt.Add(6*codexAppServerStatusPollInterval))
	require.NoError(t, err)
	assert.Equal(t, "degraded", got.Health)
	assert.Contains(t, got.Detail, "stale")
}

func TestDashboardCodexDriveUsesCurrentPostureNotHistoricalRuntime(t *testing.T) {
	resetTestDB(t)
	const convID = "codex-rolled-back"
	seedCodexDiagnosticAgent(t, convID, false)
	runtime := db.CodexAppServerRuntime{
		Generation: "historical-generation", LaunchID: "historical-launch", AgentID: "agent-old",
		ConvID: convID, ThreadID: convID, SocketPath: "/tmp/private/app.sock",
		CodexVersion: "0.147.0", State: db.CodexAppServerDead,
	}
	require.NoError(t, db.UpsertCodexAppServerRuntime(runtime))
	row, err := db.FindSessionByConvID(convID)
	require.NoError(t, err)
	require.NotNil(t, row)

	state := stateForConvInSessionsBatched([]*db.SessionRow{row}, map[string]struct{}{}, nil, nil, nil)
	assert.False(t, state.CodexAppServer,
		"a historical app-server generation must not override the current explicit send-keys posture")
	assert.Empty(t, state.CodexAppServerState)

	require.NoError(t, db.SetAgentCodexAppServerSelectionForConv(convID, true, "named profile rollout"))
	runtime.State = db.CodexAppServerUnavailable
	runtime.Detail = "dial /home/operator/private/app.sock: connection refused"
	require.NoError(t, db.UpsertCodexAppServerRuntime(runtime))
	state = stateForConvInSessionsBatched([]*db.SessionRow{row}, map[string]struct{}{}, nil, nil, nil)
	assert.True(t, state.CodexAppServer)
	assert.Equal(t, "named profile rollout", state.CodexAppServerSource)
	assert.Equal(t, "disconnected", state.CodexAppServerHealth)
	assert.NotContains(t, state.CodexAppServerDetail, "/home/operator")
}
