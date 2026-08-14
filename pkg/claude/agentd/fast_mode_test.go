package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestSessionArgsCarryExplicitFastMode(t *testing.T) {
	bare := sessionNewArgs(clcommon.SpawnArgs{Label: "worker", Harness: harness.CodexName})
	assert.NotContains(t, bare, "--fast-mode")

	for _, mode := range []string{harness.FastModeOn, harness.FastModeOff} {
		fresh := sessionNewArgs(clcommon.SpawnArgs{
			Label: "worker", Harness: harness.CodexName, FastMode: mode,
		})
		assert.Equal(t, mode, valueAfter(fresh, "--fast-mode"))
		resume := sessionResumeArgs(clcommon.SpawnArgs{
			ConvID: "conv-1", Harness: harness.CodexName, FastMode: mode,
		})
		assert.Equal(t, mode, valueAfter(resume, "--fast-mode"))
	}
}

func TestCodexFastModeProbeBoundaryUsesRecordedLaunch(t *testing.T) {
	setupTestDB(t)
	const generation = "fast-mode-generation"
	require.NoError(t, db.UpsertCodexNativePermissionProfile(db.CodexNativePermissionProfile{
		Generation: "launch:" + generation, ProfileName: "tclaude-agent-fast-mode",
		ProfileTOML: "default_permissions = \"workspace-write\"\n",
	}))
	sess := &db.SessionRow{
		ID: "fast-mode-session", ConvID: "fast-mode-conv", Harness: harness.CodexName,
		HarnessBuiltinMode:   harness.SandboxDangerFull,
		ExitLaunchGeneration: generation, ExitLaunchGateState: db.SessionExitGateUngated,
		EffectiveSandbox: &sandboxpolicy.Snapshot{Version: sandboxpolicy.SnapshotVersion, Effective: sandboxpolicy.EffectiveProfile{
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "PATH", Value: "/agent/bin"},
			},
		}},
	}
	require.NoError(t, db.SaveSession(sess))
	agentID, _, err := db.EnsureAgentForConv(sess.ConvID, "test")
	require.NoError(t, err)
	stateRoot := "/agent/codex-home"
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, CodexStateRoot: &stateRoot,
	}))

	environment, profile, err := codexFastModeProbeBoundary(sess)
	require.NoError(t, err)
	assert.Equal(t, "tclaude-agent-fast-mode", profile)
	assert.Equal(t, []sandboxpolicy.EnvironmentEntry{
		{Name: "PATH", Value: "/agent/bin"},
		{Name: "CODEX_HOME", Value: "/agent/codex-home"},
	}, environment)
}

func TestCodexFastModeProbeBoundaryUsesAppServerGeneration(t *testing.T) {
	setupTestDB(t)
	sess := &db.SessionRow{
		ID: "fast-app-server-session", ConvID: "fast-app-server-conv",
		Harness: harness.CodexName, HarnessBuiltinMode: harness.SandboxDangerFull,
	}
	require.NoError(t, db.SaveSession(sess))
	agentID, _, err := db.EnsureAgentForConv(sess.ConvID, "test")
	require.NoError(t, err)
	require.NoError(t, db.UpsertCodexAppServerRuntime(db.CodexAppServerRuntime{
		Generation: "fast-app-server-generation", LaunchID: sess.ID,
		AgentID: agentID, ConvID: sess.ConvID, SocketPath: "/tmp/fast-app-server.sock",
		State: db.CodexAppServerReady,
	}))
	require.NoError(t, db.UpsertCodexNativePermissionProfile(db.CodexNativePermissionProfile{
		Generation: "fast-app-server-generation", ProfileName: "tclaude-agent-fast-app-server",
		ProfileTOML: "default_permissions = \"workspace-write\"\n",
	}))

	_, profile, err := codexFastModeProbeBoundary(sess)
	require.NoError(t, err)
	assert.Equal(t, "tclaude-agent-fast-app-server", profile)
}

func TestCodexFastModeForSessionRejectsPreviousGenerationObservation(t *testing.T) {
	created := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	snap := harness.CodexRuntimeSnapshot{
		FastMode:         true,
		HasFastMode:      true,
		FastModeObserved: created.Add(time.Second),
	}
	first := &db.SessionRow{ID: "same-session", ConvID: "same-conv", CreatedAt: created}
	fast, known := codexFastModeForSession(snap, first)
	assert.True(t, known)
	assert.True(t, fast)

	// Session pruning and a later resume can recreate the same keys. Until
	// Codex appends settings for that new process, the follower's old event is
	// not authoritative for the replacement generation.
	resumed := &db.SessionRow{
		ID: "same-session", ConvID: "same-conv", CreatedAt: created.Add(2 * time.Second),
	}
	fast, known = codexFastModeForSession(snap, resumed)
	assert.False(t, known)
	assert.False(t, fast)

	snap.FastMode = false
	snap.FastModeObserved = created.Add(3 * time.Second)
	fast, known = codexFastModeForSession(snap, resumed)
	assert.True(t, known)
	assert.False(t, fast)
}

func valueAfter(args []string, flag string) string {
	for i := range args {
		if args[i] == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
