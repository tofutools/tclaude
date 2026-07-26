package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestSandboxRestartIdleFailureRequiresNoBackgroundWork(t *testing.T) {
	now := time.Now()
	assert.Contains(t, sandboxRestartIdleFailure(nil, now), "must be online")
	assert.Empty(t, sandboxRestartIdleFailure(&db.SessionRow{Status: session.StatusIdle}, now))

	working := &db.SessionRow{Status: session.StatusWorking}
	assert.Contains(t, sandboxRestartIdleFailure(working, now), "status is working")

	subagents := db.SubagentSet{
		"sub-1": {Type: "worker", Seen: now},
	}
	withSubagent := &db.SessionRow{
		Status: session.StatusIdle, SubagentsJSON: subagents.Encode(),
	}
	assert.Contains(t, sandboxRestartIdleFailure(withSubagent, now), "background agent")

	shells := db.BgShellSet{
		"shell-1": {Command: "go test ./...", Seen: now},
	}
	withShell := &db.SessionRow{
		Status: session.StatusIdle, BgShellsJSON: shells.Encode(),
	}
	assert.Contains(t, sandboxRestartIdleFailure(withShell, now), "background shell command")

	expired := &db.SessionRow{
		Status: session.StatusIdle,
		SubagentsJSON: db.SubagentSet{
			"old-sub": {Seen: now.Add(-db.SubagentTTL - time.Second)},
		}.Encode(),
		BgShellsJSON: db.BgShellSet{
			"old-shell": {Seen: now.Add(-db.BgShellTTL - time.Second)},
		}.Encode(),
	}
	assert.Empty(t, sandboxRestartIdleFailure(expired, now),
		"expired ledger ghosts must not permanently wedge the transition")
}

func TestDurableRelaunchAppliesTemporaryAgentOverrideButKeepsNormalPosture(t *testing.T) {
	setupTestDB(t)
	const convID = "temporary-relaunch"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	normalMode := harness.ClaudeSandboxOn
	normalSource := `group default profile "confined"`
	approval := "default"
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, SandboxMode: &normalMode,
		SandboxModeSource: &normalSource, ApprovalPolicy: &approval,
	}))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "temporary-relaunch-session", ConvID: convID, Cwd: t.TempDir(),
		Harness: harness.DefaultName, Status: session.StatusIdle,
		SandboxMode: normalMode, SandboxModeSource: normalSource,
		ApprovalPolicy: approval, ResumeProvenance: "test-proof",
	}))
	override := harness.ClaudeSandboxOff
	require.NoError(t, db.SetTemporarySandboxModeForConv(
		convID, normalMode, normalSource, &override,
	))

	got, err := durableRelaunchConfigForConv(convID)
	require.NoError(t, err)
	assert.True(t, got.TemporarySandboxMode)
	assert.Equal(t, harness.ClaudeSandboxOff, got.Sandbox)
	assert.Equal(t, "temporary dashboard unlock", got.SandboxModeSource)
	assert.Equal(t, harness.ClaudeSandboxOn, got.NormalSandbox)
	assert.Equal(t, normalSource, got.NormalSandboxSource)
}
