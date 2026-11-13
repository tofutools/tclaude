package session

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/launchundo"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A launch that is not Darwin route-capable claims nothing: no reservation,
// no undo step, and its params are left as they were.
func TestClaimDarwinRouteLaunchIgnoresAnOrdinaryLaunch(t *testing.T) {
	var undo launchundo.Undo
	params := &NewParams{DarwinRouteAgentID: "agt_keep"}
	reservation, err := claimDarwinRouteLaunch(params, harness.DefaultName, true, "spwn-1", "conv-1", "gen-1", &undo)
	require.NoError(t, err)
	assert.Nil(t, reservation)
	assert.Equal(t, "agt_keep", params.DarwinRouteAgentID)
}

// A route-capable launch is refused before any slot is reserved when its
// preconditions do not hold.
func TestClaimDarwinRouteLaunchRefusesUnmetPreconditions(t *testing.T) {
	var undo launchundo.Undo
	params := &NewParams{DarwinRouteCapable: true}
	if runtime.GOOS != "darwin" {
		reservation, err := claimDarwinRouteLaunch(params, harness.DefaultName, true, "spwn-1", "conv-1", "gen-1", &undo)
		require.ErrorContains(t, err, "requires macOS")
		assert.Nil(t, reservation)
		return
	}
	reservation, err := claimDarwinRouteLaunch(params, harness.DefaultName, false, "spwn-1", "conv-1", "gen-1", &undo)
	require.ErrorContains(t, err, "requires the pane tclaude")
	assert.Nil(t, reservation)
}

func testLaunchRowInput(sessionID, generation string, paneCommand *string) launchRowInput {
	return launchRowInput{
		sessionID: sessionID, tmuxSession: sessionID, cwd: "/work", convID: "conv-row",
		exitGeneration: generation, h: harness.MustGet(harness.CodexName), params: &NewParams{},
		harnessBuiltinMode:    "workspace-write",
		sandboxImplementation: sandboxpolicy.ImplementationHarnessBuiltin,
		sandboxChosenBy:       "default",
		approvalPolicy:        "on-request", autoReview: true, askTimeout: "off",
		paneCommand: paneCommand,
	}
}

// The launch row records the launch's posture, and unwinding takes back a
// row this launch created.
func TestCreateLaunchRowSavesAndRollsBackAFreshRow(t *testing.T) {
	isolateCacheAndDB(t)
	var undo launchundo.Undo
	paneCommand := ""
	state, err := createLaunchRow(testLaunchRowInput("spwn-row-fresh", "gen-fresh", &paneCommand), &undo)
	require.NoError(t, err)
	assert.Equal(t, "conv-row", state.ConvID)

	saved, err := LoadSessionState("spwn-row-fresh")
	require.NoError(t, err)
	assert.Equal(t, "/work", saved.Cwd)
	assert.Equal(t, harness.CodexName, saved.Harness)
	assert.Equal(t, "on-request", saved.ApprovalPolicy)
	assert.True(t, saved.ApprovalAutoReview)

	paneCommand = "exec codex" // assigned after the row, read at unwind time
	undo.Unwind()
	_, err = LoadSessionState("spwn-row-fresh")
	require.Error(t, err, "a row this launch created must be rolled back")
}

// A relaunch reuses an existing row, and a failure keeps it.
func TestCreateLaunchRowKeepsAReusedRowOnRollback(t *testing.T) {
	isolateCacheAndDB(t)
	require.NoError(t, SaveSessionState(&SessionState{ID: "spwn-row-reuse", Status: StatusIdle}))
	var undo launchundo.Undo
	paneCommand := ""
	_, err := createLaunchRow(testLaunchRowInput("spwn-row-reuse", "gen-reuse", &paneCommand), &undo)
	require.NoError(t, err)

	undo.Unwind()
	saved, err := LoadSessionState("spwn-row-reuse")
	require.NoError(t, err)
	assert.Equal(t, "conv-row", saved.ConvID)
}
