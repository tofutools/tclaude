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
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestAgentRestartIdleFailureRequiresNoBackgroundWork(t *testing.T) {
	now := time.Now()
	assert.Contains(t, agentRestartIdleFailure(nil, now), "must be online")
	assert.Empty(t, agentRestartIdleFailure(&db.SessionRow{Status: session.StatusIdle}, now))

	working := &db.SessionRow{Status: session.StatusWorking}
	assert.Contains(t, agentRestartIdleFailure(working, now), "status is working")

	subagents := db.SubagentSet{
		"sub-1": {Type: "worker", Seen: now},
	}
	withSubagent := &db.SessionRow{
		Status: session.StatusIdle, SubagentsJSON: subagents.Encode(),
	}
	assert.Contains(t, agentRestartIdleFailure(withSubagent, now), "background agent")
	legacySubagent := &db.SessionRow{
		Status: session.StatusIdle, SubagentCount: 1,
	}
	assert.Contains(t, agentRestartIdleFailure(legacySubagent, now), "background agent",
		"a pre-ledger row must fall back to its known nonzero count")

	shells := db.BgShellSet{
		"shell-1": {Command: "go test ./...", Seen: now},
	}
	withShell := &db.SessionRow{
		Status: session.StatusIdle, BgShellsJSON: shells.Encode(),
	}
	assert.Contains(t, agentRestartIdleFailure(withShell, now), "background shell command")

	expired := &db.SessionRow{
		Status: session.StatusIdle,
		SubagentsJSON: db.SubagentSet{
			"old-sub": {Seen: now.Add(-db.SubagentTTL - time.Second)},
		}.Encode(),
		BgShellsJSON: db.BgShellSet{
			"old-shell": {Seen: now.Add(-db.BgShellTTL - time.Second)},
		}.Encode(),
	}
	assert.Empty(t, agentRestartIdleFailure(expired, now),
		"expired ledger ghosts must not permanently wedge the transition")
}

func TestAgentRestartTmuxHandoffCarriesAttachedClients(t *testing.T) {
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

	handoff := beginAgentRestartTmuxHandoff(oldTmux)
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

func TestAgentRestartTmuxBridgeHasSelfExpiry(t *testing.T) {
	assert.Contains(t, agentRestartHoldingCommand, "sleep 300")
	assert.NotContains(t, agentRestartHoldingCommand, "while :",
		"the bridge must not outlive a crashed handler forever")
}

func TestAgentRestartTmuxHandoffSkipsBridgeWithoutAttachedClients(t *testing.T) {
	w := testharness.New(t)
	previousTmux := clcommon.Default
	clcommon.Default = w.Tmux
	t.Cleanup(func() { clcommon.Default = previousTmux })

	w.Tmux.MarkAlive("unattended-pane")
	assert.Nil(t, beginAgentRestartTmuxHandoff("unattended-pane"))
	assert.Zero(t, w.Tmux.CommandCount("new-session"),
		"an unattended restart does not need a bridge shell")
}

func TestAgentRestartTmuxHandoffFailureDoesNotBlockRestart(t *testing.T) {
	w := testharness.New(t)
	previousTmux := clcommon.Default
	clcommon.Default = w.Tmux
	t.Cleanup(func() { clcommon.Default = previousTmux })

	const oldTmux = "sandbox-old-pane"
	w.Tmux.MarkAlive(oldTmux)
	w.Tmux.AttachClient("/dev/pts/41", oldTmux)
	w.Tmux.FailNextCommand("new-session")

	assert.Nil(t, beginAgentRestartTmuxHandoff(oldTmux))
	assert.Equal(t, oldTmux, w.Tmux.ClientSession("/dev/pts/41"),
		"a failed best-effort bridge must leave the existing client alone")
}

func TestAgentRestartTmuxHandoffRefusesUnboundedBridge(t *testing.T) {
	w := testharness.New(t)
	previousTmux := clcommon.Default
	clcommon.Default = w.Tmux
	t.Cleanup(func() { clcommon.Default = previousTmux })

	const oldTmux = "sandbox-old-pane"
	w.Tmux.MarkAlive(oldTmux)
	w.Tmux.AttachClient("/dev/pts/41", oldTmux)
	w.Tmux.SetNewSessionRemainOnExit(true)
	w.Tmux.FailNextCommand("set-option")

	assert.Nil(t, beginAgentRestartTmuxHandoff(oldTmux))
	assert.Equal(t, oldTmux, w.Tmux.ClientSession("/dev/pts/41"),
		"clients must stay on the agent when bridge expiry cannot be enforced")
	assert.Len(t, w.Tmux.Sessions(), 1, "the rejected bridge must be removed")
}

func TestRequireCurrentAgentGenerationRejectsSupersededConversation(t *testing.T) {
	setupTestDB(t)
	const oldConv = "sandbox-restart-old"
	const newConv = "sandbox-restart-new"
	agentID, _, err := db.EnsureAgentForConv(oldConv, "test")
	require.NoError(t, err)
	require.NoError(t, requireCurrentAgentGeneration(agentID, oldConv))

	_, err = db.RotateAgentConv(oldConv, newConv, "clear")
	require.NoError(t, err)
	assert.ErrorContains(t, requireCurrentAgentGeneration(agentID, oldConv),
		"no longer the agent's current generation")
	require.NoError(t, requireCurrentAgentGeneration(agentID, newConv))
}

func TestDurableRelaunchAppliesTemporaryAgentOverrideButKeepsNormalPosture(t *testing.T) {
	setupTestDB(t)
	const convID = "temporary-relaunch"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	normalMode := harness.ClaudeSandboxOn
	implementation := string(sandboxpolicy.ImplementationTclaudeLayer)
	normalSource := `group default profile "confined"`
	approval := "default"
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, SandboxMode: &normalMode,
		SandboxImplementation: &implementation,
		SandboxModeSource:     &normalSource, ApprovalPolicy: &approval,
	}))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "temporary-relaunch-session", ConvID: convID, Cwd: t.TempDir(),
		Harness: harness.DefaultName, Status: session.StatusIdle,
		SandboxMode: normalMode, SandboxImplementation: implementation,
		SandboxModeSource: normalSource,
		ApprovalPolicy:    approval, ResumeProvenance: "test-proof",
	}))
	override := harness.ClaudeSandboxOff
	require.NoError(t, db.SetTemporarySandboxModeForConv(
		convID, normalMode, implementation, normalSource, &override,
	))

	got, err := durableRelaunchConfigForConv(convID)
	require.NoError(t, err)
	assert.True(t, got.TemporarySandboxMode)
	assert.Equal(t, harness.ClaudeSandboxOff, got.Sandbox)
	assert.Equal(t, "temporary dashboard unlock", got.SandboxModeSource)
	assert.Equal(t, harness.ClaudeSandboxOn, got.NormalSandbox)
	assert.Equal(t, normalSource, got.NormalSandboxSource)
	assert.Equal(t, implementation, got.SandboxImplementation)
	assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin),
		got.activeSandboxImplementation(),
		"temporary off disables the tclaude outer layer for the replacement process")
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
	require.NoError(t, db.SetTemporarySandboxModeForConv(
		convID, normalMode, string(sandboxpolicy.ImplementationHarnessBuiltin), "", &override,
	))

	got, err := durableRelaunchConfigForConv(convID)
	require.NoError(t, err)
	assert.False(t, got.SSHWorkaround, "danger-full-access launch does not materialize managed-profile SSH")
	codex, ok := harness.Get(harness.CodexName)
	require.True(t, ok)
	assert.Equal(t, codex.CanSSHWorkaround(), got.NormalSSHWorkaround,
		"a clone/restore preserves the normal platform-supported SSH posture")
}

func TestDurableRelaunchRecoversImplementationLostByTemporaryUnlock(t *testing.T) {
	setupTestDB(t)
	const convID = "temporary-implementation-recovery"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	normalMode := harness.ClaudeSandboxOn
	override := harness.ClaudeSandboxOff
	implementation := string(sandboxpolicy.ImplementationTclaudeLayer)
	overwritten := string(sandboxpolicy.ImplementationHarnessBuiltin)
	approval := "default"

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "before-temporary-unlock", ConvID: convID, Cwd: t.TempDir(),
		Harness: harness.DefaultName, Status: session.StatusExited,
		SandboxMode: normalMode, SandboxImplementation: implementation,
		ApprovalPolicy: approval, CreatedAt: time.Now().Add(-time.Minute),
	}))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "temporary-unlocked", ConvID: convID, Cwd: t.TempDir(),
		Harness: harness.DefaultName, Status: session.StatusIdle,
		SandboxMode: override, SandboxImplementation: overwritten,
		SandboxModeSource: db.TemporarySandboxModeSource,
		ApprovalPolicy:    approval, CreatedAt: time.Now(),
	}))
	// Reproduce the durable shape written by the deployed faulty version.
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, SandboxMode: &normalMode,
		SandboxImplementation: &overwritten, TemporarySandboxMode: &override,
		ApprovalPolicy: &approval,
	}))

	got, err := durableRelaunchConfigForConv(convID)
	require.NoError(t, err)
	assert.True(t, got.TemporarySandboxMode)
	assert.Equal(t, implementation, got.SandboxImplementation,
		"the pre-unlock session must recover the lost exact normal implementation")
	assert.Equal(t, overwritten, got.activeSandboxImplementation(),
		"the still-temporary process remains fully off")

	require.NoError(t, db.SetTemporarySandboxMode(
		agentID, got.NormalSandbox, got.SandboxImplementation,
		got.NormalSandboxSource, nil,
	))
	profile, err := db.AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.SandboxImplementation)
	assert.Equal(t, implementation, *profile.SandboxImplementation,
		"clearing the override must persist the recovered normal implementation")
}

func TestDurableRelaunchRecoversImplementationAfterFailedRestore(t *testing.T) {
	setupTestDB(t)
	const convID = "failed-restore-implementation-recovery"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	normalMode := harness.ClaudeSandboxOff
	implementation := string(sandboxpolicy.ImplementationTclaudeLayer)
	overwritten := string(sandboxpolicy.ImplementationHarnessBuiltin)
	approval := "default"

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "before-failed-restore", ConvID: convID, Cwd: t.TempDir(),
		Harness: harness.DefaultName, Status: session.StatusExited,
		SandboxMode: normalMode, SandboxImplementation: implementation,
		ApprovalPolicy: approval, CreatedAt: time.Now().Add(-time.Minute),
	}))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "temporary-unlock-evidence", ConvID: convID, Cwd: t.TempDir(),
		Harness: harness.DefaultName, Status: session.StatusExited,
		SandboxMode: normalMode, SandboxImplementation: overwritten,
		SandboxModeSource: db.TemporarySandboxModeSource,
		ApprovalPolicy:    approval,
		CreatedAt:         time.Now().Add(-30 * time.Second),
	}))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "after-failed-restore", ConvID: convID, Cwd: t.TempDir(),
		Harness: harness.DefaultName, Status: session.StatusIdle,
		SandboxMode: normalMode, SandboxImplementation: overwritten,
		ApprovalPolicy: approval, CreatedAt: time.Now(),
	}))
	// The faulty restore has already cleared TemporarySandboxMode, leaving a
	// plain-OFF durable record even though the earlier session proves the
	// agent's exact normal implementation was the TClaude outer layer.
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, SandboxMode: &normalMode,
		SandboxImplementation: &overwritten, ApprovalPolicy: &approval,
	}))

	got, err := durableRelaunchConfigForConv(convID)
	require.NoError(t, err)
	assert.False(t, got.TemporarySandboxMode)
	assert.Equal(t, implementation, got.SandboxImplementation)
	assert.Equal(t, implementation, got.activeSandboxImplementation(),
		"the next ordinary restart must recover the exact pre-bug combination")
}

func TestDurableRelaunchDoesNotUndoExplicitHarnessBuiltinChange(t *testing.T) {
	setupTestDB(t)
	const convID = "intentional-harness-builtin"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	mode := harness.ClaudeSandboxOn
	layered := string(sandboxpolicy.ImplementationTclaudeLayer)
	builtin := string(sandboxpolicy.ImplementationHarnessBuiltin)
	approval := "default"

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "older-layered", ConvID: convID, Cwd: t.TempDir(),
		Harness: harness.DefaultName, Status: session.StatusExited,
		SandboxMode: mode, SandboxImplementation: layered,
		ApprovalPolicy: approval,
	}))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "intentional-builtin", ConvID: convID, Cwd: t.TempDir(),
		Harness: harness.DefaultName, Status: session.StatusIdle,
		SandboxMode: mode, SandboxImplementation: builtin,
		SandboxModeSource: "explicit request", ApprovalPolicy: approval,
	}))
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, SandboxMode: &mode,
		SandboxImplementation: &builtin, ApprovalPolicy: &approval,
	}))

	got, err := durableRelaunchConfigForConv(convID)
	require.NoError(t, err)
	assert.Equal(t, builtin, got.SandboxImplementation,
		"outer-layer history alone is not evidence of the temporary-unlock bug")
}
