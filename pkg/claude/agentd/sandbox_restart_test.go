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
	legacySubagent := &db.SessionRow{
		Status: session.StatusIdle, SubagentCount: 1,
	}
	assert.Contains(t, sandboxRestartIdleFailure(legacySubagent, now), "background agent",
		"a pre-ledger row must fall back to its known nonzero count")

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

func TestRequireCurrentSandboxRestartGenerationRejectsSupersededConversation(t *testing.T) {
	setupTestDB(t)
	const oldConv = "sandbox-restart-old"
	const newConv = "sandbox-restart-new"
	agentID, _, err := db.EnsureAgentForConv(oldConv, "test")
	require.NoError(t, err)
	require.NoError(t, requireCurrentSandboxRestartGeneration(agentID, oldConv))

	_, err = db.RotateAgentConv(oldConv, newConv, "clear")
	require.NoError(t, err)
	assert.ErrorContains(t, requireCurrentSandboxRestartGeneration(agentID, oldConv),
		"no longer the agent's current generation")
	require.NoError(t, requireCurrentSandboxRestartGeneration(agentID, newConv))
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

func TestDurableRelaunchKeepsNormalCodexSSHPostureDuringUnlock(t *testing.T) {
	setupTestDB(t)
	const convID = "temporary-codex-relaunch"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	normalMode := harness.SandboxManagedProfile
	approval := harness.ApprovalNever
	ssh := true
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, SandboxMode: &normalMode,
		ApprovalPolicy: &approval, SSHWorkaround: &ssh,
	}))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "temporary-codex-session", ConvID: convID, Cwd: t.TempDir(),
		Harness: harness.CodexName, Status: session.StatusIdle,
		SandboxMode: normalMode, ApprovalPolicy: approval, ResumeProvenance: "test-proof",
	}))
	override := harness.SandboxDangerFull
	require.NoError(t, db.SetTemporarySandboxModeForConv(convID, normalMode, "", &override))

	got, err := durableRelaunchConfigForConv(convID)
	require.NoError(t, err)
	assert.False(t, got.SSHWorkaround, "danger-full-access launch does not materialize managed-profile SSH")
	assert.True(t, got.NormalSSHWorkaround, "a clone/restore still needs the preserved managed-profile SSH posture")
}
