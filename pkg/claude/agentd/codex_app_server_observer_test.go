package agentd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/codexappserver"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestCodexAppServerObserverProjectsSnapshotStatusAndRejectsStaleOrdering(t *testing.T) {
	resetTestDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	row := &db.SessionRow{
		ID: "observer-session", ConvID: "thread-1", TmuxSession: "observer-tmux",
		Cwd: t.TempDir(), Status: session.StatusIdle, Harness: harness.CodexName,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.SaveSession(row))
	runtime := db.CodexAppServerRuntime{
		Generation: "generation-1", LaunchID: "launch-1", AgentID: "agent-1",
		ConvID: row.ConvID, ThreadID: row.ConvID, SocketPath: "/tmp/observer.sock",
		State: db.CodexAppServerReady, CreatedAt: now,
	}
	require.NoError(t, db.UpsertCodexAppServerRuntime(runtime))
	handle := &codexAppServerHandle{runtime: runtime}

	projectCodexAppServerStatus(handle, codexappserver.ThreadStatus{
		Type: "active", ActiveFlags: []string{"waitingOnApproval"},
	}, now.Add(time.Second), "app-server snapshot")
	got, err := db.FindSessionByConvID(row.ConvID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, session.StatusAwaitingPermission, got.Status)

	// Thread-scoped notifications are unreachable after the post-bind dial and
	// therefore ignored even if a future server happens to broadcast one.
	statusParams, err := json.Marshal(codexappserver.ThreadStatusChangedNotification{
		ThreadID: row.ConvID, Status: codexappserver.ThreadStatus{Type: "idle"},
	})
	require.NoError(t, err)
	handleCodexAppServerNotification(handle, codexappserver.Notification{
		Method: codexappserver.NotificationThreadStatusChanged, Params: statusParams,
	})
	got, err = db.FindSessionByConvID(row.ConvID)
	require.NoError(t, err)
	assert.Equal(t, session.StatusAwaitingPermission, got.Status,
		"a thread event must not override snapshot-owned status")

	projectCodexAppServerStatus(handle, codexappserver.ThreadStatus{Type: "idle"},
		now, "late event")
	got, err = db.FindSessionByConvID(row.ConvID)
	require.NoError(t, err)
	assert.Equal(t, session.StatusAwaitingPermission, got.Status,
		"an older event must not roll status backwards")
	assert.True(t, handle.observation.snapshot().StatusAt.After(now))
}

func TestCodexAppServerObserverReplacementSnapshotIsGenerationIsolated(t *testing.T) {
	resetTestDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	row := &db.SessionRow{
		ID: "observer-reconnect", ConvID: "thread-reconnect", TmuxSession: "observer-tmux",
		Cwd: t.TempDir(), Status: session.StatusWorking, Harness: harness.CodexName,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.SaveSession(row))
	oldRuntime := db.CodexAppServerRuntime{
		Generation: "old-generation", LaunchID: "old-launch", AgentID: "agent-1",
		ConvID: row.ConvID, ThreadID: row.ConvID, SocketPath: "/tmp/old.sock",
		State: db.CodexAppServerReady, CreatedAt: now.Add(-time.Minute),
	}
	newRuntime := db.CodexAppServerRuntime{
		Generation: "new-generation", LaunchID: "new-launch", AgentID: "agent-1",
		ConvID: row.ConvID, ThreadID: row.ConvID, SocketPath: "/tmp/new.sock",
		State: db.CodexAppServerReady, CreatedAt: now,
	}
	require.NoError(t, db.UpsertCodexAppServerRuntime(oldRuntime))
	require.NoError(t, db.UpsertCodexAppServerRuntime(newRuntime))

	oldHandle := &codexAppServerHandle{runtime: oldRuntime}
	projectCodexAppServerRawStatus(oldHandle, json.RawMessage(`{"type":"idle"}`),
		now.Add(time.Second), "obsolete snapshot")
	got, err := db.FindSessionByConvID(row.ConvID)
	require.NoError(t, err)
	assert.Equal(t, session.StatusWorking, got.Status)

	newHandle := &codexAppServerHandle{runtime: newRuntime}
	projectCodexAppServerRawStatus(newHandle, json.RawMessage(`{"type":"idle"}`),
		now.Add(2*time.Second), "reconnect snapshot")
	got, err = db.FindSessionByConvID(row.ConvID)
	require.NoError(t, err)
	assert.Equal(t, session.StatusIdle, got.Status)
	assert.Equal(t, "reconnect snapshot", got.StatusDetail)
}

func TestCodexUsageFromAppServerSelectsWindowDurations(t *testing.T) {
	limitID, limitName, plan := "codex", "Codex", "plus"
	fiveMinutes, weeklyMinutes := int64(300), int64(10080)
	fiveReset, weeklyReset := int64(2_000_000_000), int64(2_000_600_000)
	observed := time.Unix(1_999_000_000, 0)
	usage := codexUsageFromAppServer(codexappserver.RateLimitSnapshot{
		LimitID: &limitID, LimitName: &limitName, PlanType: &plan,
		Primary: &codexappserver.RateLimitWindow{
			UsedPercent: 12, WindowDurationMins: &fiveMinutes, ResetsAt: &fiveReset,
		},
		Secondary: &codexappserver.RateLimitWindow{
			UsedPercent: 34, WindowDurationMins: &weeklyMinutes, ResetsAt: &weeklyReset,
		},
	}, observed)
	require.NotNil(t, usage.FiveHour)
	require.NotNil(t, usage.Weekly)
	assert.Equal(t, 12.0, usage.FiveHour.UsedPercent)
	assert.Equal(t, 34.0, usage.Weekly.UsedPercent)
	assert.Equal(t, observed, usage.Observed)
	assert.Equal(t, "codex", usage.LimitID)
}
