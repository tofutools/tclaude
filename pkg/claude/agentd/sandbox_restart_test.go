package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
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

func TestSandboxRestartTmuxHandoffCarriesAttachedClients(t *testing.T) {
	w := testharness.New(t)
	previousTmux := clcommon.Default
	clcommon.Default = w.Tmux
	t.Cleanup(func() { clcommon.Default = previousTmux })

	const (
		oldTmux = "sandbox-old-pane"
		ttyOne  = "/dev/pts/41"
		ttyTwo  = "/dev/pts/42"
	)
	w.Tmux.MarkAlive(oldTmux)
	w.Tmux.AttachClient(ttyOne, oldTmux)
	w.Tmux.AttachClient(ttyTwo, oldTmux)
	w.Tmux.SetNewSessionRemainOnExit(true)

	handoff := beginSandboxRestartTmuxHandoff(oldTmux)
	require.NotNil(t, handoff)
	holding := handoff.holdingSession
	require.NotEmpty(t, holding)
	assert.False(t, w.Tmux.PaneRemainOnExit(holding),
		"the self-expiring bridge must override an inherited remain-on-exit")
	assert.Equal(t, holding, w.Tmux.ClientSession(ttyOne))
	assert.Equal(t, holding, w.Tmux.ClientSession(ttyTwo))

	// The same-name production resume cannot appear until the old pane is
	// gone. Model that gap, then reuse the original name for the new pane.
	w.Tmux.MarkOffline(oldTmux)
	w.Tmux.MarkAlive(oldTmux)
	assert.Equal(t, 2, handoff.finish(oldTmux))
	assert.Equal(t, oldTmux, w.Tmux.ClientSession(ttyOne))
	assert.Equal(t, oldTmux, w.Tmux.ClientSession(ttyTwo))
	assert.False(t, w.Tmux.IsAlive(holding), "the bridge must not leak")
}

func TestSandboxRestartTmuxBridgeHasSelfExpiry(t *testing.T) {
	assert.Contains(t, sandboxRestartHoldingCommand, "sleep 300")
	assert.NotContains(t, sandboxRestartHoldingCommand, "while :",
		"the bridge must not outlive a crashed handler forever")
}

func TestSandboxRestartTmuxHandoffSkipsBridgeWithoutAttachedClients(t *testing.T) {
	w := testharness.New(t)
	previousTmux := clcommon.Default
	clcommon.Default = w.Tmux
	t.Cleanup(func() { clcommon.Default = previousTmux })

	w.Tmux.MarkAlive("unattended-pane")
	assert.Nil(t, beginSandboxRestartTmuxHandoff("unattended-pane"))
	assert.Zero(t, w.Tmux.CommandCount("new-session"),
		"an unattended restart does not need a bridge shell")
}

func TestSandboxRestartTmuxHandoffFailureDoesNotBlockRestart(t *testing.T) {
	w := testharness.New(t)
	previousTmux := clcommon.Default
	clcommon.Default = w.Tmux
	t.Cleanup(func() { clcommon.Default = previousTmux })

	const oldTmux = "sandbox-old-pane"
	w.Tmux.MarkAlive(oldTmux)
	w.Tmux.AttachClient("/dev/pts/41", oldTmux)
	w.Tmux.FailNextCommand("new-session")

	assert.Nil(t, beginSandboxRestartTmuxHandoff(oldTmux))
	assert.Equal(t, oldTmux, w.Tmux.ClientSession("/dev/pts/41"),
		"a failed best-effort bridge must leave the existing client alone")
}

func TestSandboxRestartTmuxHandoffRefusesUnboundedBridge(t *testing.T) {
	w := testharness.New(t)
	previousTmux := clcommon.Default
	clcommon.Default = w.Tmux
	t.Cleanup(func() { clcommon.Default = previousTmux })

	const oldTmux = "sandbox-old-pane"
	w.Tmux.MarkAlive(oldTmux)
	w.Tmux.AttachClient("/dev/pts/41", oldTmux)
	w.Tmux.SetNewSessionRemainOnExit(true)
	w.Tmux.FailNextCommand("set-option")

	assert.Nil(t, beginSandboxRestartTmuxHandoff(oldTmux))
	assert.Equal(t, oldTmux, w.Tmux.ClientSession("/dev/pts/41"),
		"clients must stay on the agent when bridge expiry cannot be enforced")
	assert.Len(t, w.Tmux.Sessions(), 1, "the rejected bridge must be removed")
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
	codex, ok := harness.Get(harness.CodexName)
	require.True(t, ok)
	assert.Equal(t, codex.CanSSHWorkaround(), got.NormalSSHWorkaround,
		"a clone/restore preserves the normal platform-supported SSH posture")
}
