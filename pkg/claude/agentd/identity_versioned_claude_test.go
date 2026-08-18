package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Native Claude installs expose `claude` as a symlink to a version-named
// executable. A constructed tclaude-layer launch resolves that symlink before
// entering the sandbox, so both comm and /proc/<pid>/exe report the version.
// Identity must come from the live daemon-recorded pane, never from hard-coded
// version strings or a symlink target that can move during an auto-update.
func TestVersionNamedClaudeResolvesThroughTclaudeLayerPane(t *testing.T) {
	setupTestDB(t)
	haveLiveTmuxSessions(t, "tmux-versioned-claude")

	const (
		callerPID   = 34720
		hookShell   = 34710
		claudePID   = 34700
		bwrapPID    = 34699
		launcherPID = 34685
		panePID     = 34660
		label       = "spwn-versioned-claude"
		convID      = "8b4bd152-5a56-48db-b474-7f49d8020e1e"
		generation  = "ce54fd4146b5c9a15d76474729b00de7"
	)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: label, PID: panePID, ConvID: convID,
		TmuxSession: "tmux-versioned-claude", Harness: harness.DefaultName,
		Status: "idle", SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		ExitLaunchGeneration: generation,
	}))
	fakeProcTree{
		name: map[int]string{
			callerPID: "tclaude", hookShell: "sh", claudePID: "2.1.234",
			bwrapPID: "bwrap", launcherPID: "tclaude", panePID: "bash",
		},
		exe: map[int]string{claudePID: "2.1.234"},
		parent: map[int]int{
			callerPID: hookShell, hookShell: claudePID, claudePID: bwrapPID,
			bwrapPID: launcherPID, launcherPID: panePID, panePID: 1,
		},
	}.install(t)
	previousPaneProbe := brokerLivePaneProbe
	brokerLivePaneProbe = func(string) (lifecyclePaneProbe, error) {
		return lifecyclePaneProbe{
			state: paneProbeLive, panePID: panePID, generation: generation,
		}, nil
	}
	t.Cleanup(func() { brokerLivePaneProbe = previousPaneProbe })

	assert.False(t, session.IsHarnessProcessName("2.1.234"),
		"the fix must not turn a version pattern into a harness identity")
	gotConv, hasAncestor := convIDForPID(callerPID)
	assert.True(t, hasAncestor)
	assert.Equal(t, convID, gotConv)

	row, harnessPID := hookSessionRowForPID(callerPID)
	require.NotNil(t, row)
	assert.Equal(t, label, row.ID)
	assert.Zero(t, harnessPID,
		"an unrecognised version-named process must not be promoted to harness pid")

	brokerLivePaneProbe = func(string) (lifecyclePaneProbe, error) {
		return lifecyclePaneProbe{
			state: paneProbeLive, panePID: panePID, generation: "replacement-launch",
		}, nil
	}
	staleConv, staleAncestor := convIDForPID(callerPID)
	assert.Empty(t, staleConv)
	assert.False(t, staleAncestor,
		"a reused tmux name with another generation must not prove the historical row")
	staleRow, _ := hookSessionRowForPID(callerPID)
	assert.Nil(t, staleRow)

	brokerLivePaneProbe = func(string) (lifecyclePaneProbe, error) {
		return lifecyclePaneProbe{
			state: paneProbeLive, panePID: 99999, generation: generation,
		}, nil
	}
	reusedPIDConv, reusedPIDAncestor := convIDForPID(callerPID)
	assert.Empty(t, reusedPIDConv)
	assert.False(t, reusedPIDAncestor,
		"the launch generation cannot authorize a caller outside the live pane ancestry")
}
