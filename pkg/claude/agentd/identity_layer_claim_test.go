package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestAgentIdentityForPIDProvesVersionNamedTclaudeLayerCaller(t *testing.T) {
	setupTestDB(t)

	const (
		callerPID  = 34720
		hookPID    = 34710
		claudePID  = 34700
		bwrapPID   = 34699
		launchPID  = 34685
		panePID    = 34660
		label      = "spwn-versioned-claude"
		convID     = "8b4bd152-5a56-48db-b474-7f49d8020e1e"
		generation = "ce54fd4146b5c9a15d76474729b00de7"
	)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: label, PID: panePID, ConvID: convID,
		TmuxSession: "tmux-versioned-claude", Harness: harness.DefaultName,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		ExitLaunchGeneration:  generation,
	}))
	fakeProcTree{
		name: map[int]string{
			callerPID: "tclaude", hookPID: "sh", claudePID: "2.1.234",
			bwrapPID: "bwrap", launchPID: "tclaude", panePID: "bash",
		},
		exe: map[int]string{claudePID: "2.1.234"},
		parent: map[int]int{
			callerPID: hookPID, hookPID: claudePID, claudePID: bwrapPID,
			bwrapPID: launchPID, launchPID: panePID, panePID: 1,
		},
	}.install(t)
	previousPaneProbe := brokerLivePaneProbe
	brokerLivePaneProbe = func(string) (lifecyclePaneProbe, error) {
		return lifecyclePaneProbe{
			state: paneProbeLive, panePID: panePID, generation: generation,
		}, nil
	}
	t.Cleanup(func() { brokerLivePaneProbe = previousPaneProbe })

	gotConv, hasAncestor := agentIdentityForPID(callerPID, label)
	assert.Equal(t, convID, gotConv)
	assert.True(t, hasAncestor)

	row, err := db.LoadSession(label)
	require.NoError(t, err)
	require.NotNil(t, row)
	row.ConvID = ""
	require.NoError(t, db.SaveSession(row))
	gotConv, hasAncestor = agentIdentityForPID(callerPID, label)
	assert.Empty(t, gotConv)
	assert.True(t, hasAncestor,
		"the first hook must reach classAgentUnknown before it establishes a conversation id")

	brokerLivePaneProbe = func(string) (lifecyclePaneProbe, error) {
		return lifecyclePaneProbe{
			state: paneProbeLive, panePID: panePID, generation: "replacement-launch",
		}, nil
	}
	gotConv, hasAncestor = agentIdentityForPID(callerPID, label)
	assert.Empty(t, gotConv)
	assert.True(t, hasAncestor,
		"a failed layer claim must remain agent-shaped and fail closed")
}

func TestAgentIdentityForPIDKeepsLegacyOutsideSandboxResolution(t *testing.T) {
	setupTestDB(t)

	const (
		callerPID = 4101
		claudePID = 4100
		panePID   = 4099
		convID    = "legacy-outside-sandbox"
	)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "legacy-row", PID: panePID, ConvID: convID,
		TmuxSession: "tmux-legacy", Harness: harness.DefaultName,
	}))
	fakeProcTree{
		name: map[int]string{callerPID: "tclaude", claudePID: "claude", panePID: "bash"},
		parent: map[int]int{
			callerPID: claudePID, claudePID: panePID, panePID: 1,
		},
	}.install(t)

	gotConv, hasAncestor := agentIdentityForPID(callerPID, "")
	assert.Equal(t, convID, gotConv)
	assert.True(t, hasAncestor)
}

func TestAgentIdentityForPIDRejectsLayerRowWithoutProofClaim(t *testing.T) {
	setupTestDB(t)

	const (
		callerPID = 5101
		claudePID = 5100
		panePID   = 5099
		convID    = "layer-must-not-use-legacy"
	)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "layer-row", PID: panePID, ConvID: convID,
		TmuxSession: "tmux-layer", Harness: harness.DefaultName,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
	}))
	fakeProcTree{
		name: map[int]string{callerPID: "tclaude", claudePID: "claude", panePID: "bash"},
		parent: map[int]int{
			callerPID: claudePID, claudePID: panePID, panePID: 1,
		},
	}.install(t)

	legacyConv, legacyAncestor := convIDForPID(callerPID)
	assert.Equal(t, convID, legacyConv, "fixture must reach the compatibility resolver")
	assert.True(t, legacyAncestor)

	gotConv, hasAncestor := agentIdentityForPID(callerPID, "")
	assert.Empty(t, gotConv)
	assert.True(t, hasAncestor,
		"a layer caller cannot omit its claim and inherit legacy PID authorization")
}
