package session

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The exit guard never fails a launch: whatever setup succeeds, the caller
// gets a guard it can wrap with and abort.
func TestNewLaunchExitGuardAlwaysReturnsAGuard(t *testing.T) {
	isolateCacheAndDB(t)
	guard := newLaunchExitGuard("spwn-guard", "spwn-guard", "gen-guard")
	require.NotNil(t, guard)
	assert.NotEmpty(t, guard.wrap("exec claude"))
	guard.abort()
}

// Without a write proof or profile write grants the command is left alone and
// there is nothing to wait for or clean up.
func TestGuardLaunchWithDirProofLeavesAnUnprovenLaunchAlone(t *testing.T) {
	cmd, readyPath, cleanup, err := guardLaunchWithDirProof(&NewParams{}, nil, "exec claude")
	require.NoError(t, err)
	assert.Equal(t, "exec claude", cmd)
	assert.Empty(t, readyPath)
	require.NotNil(t, cleanup)
	cleanup()
}

// A dir write proof wraps the command and hands back a readiness file the
// caller's deferred cleanup removes.
func TestGuardLaunchWithDirProofWrapsAProvenLaunch(t *testing.T) {
	isolateCacheAndDB(t)
	cmd, readyPath, cleanup, err := guardLaunchWithDirProof(&NewParams{DirWriteProof: "tok"}, nil, "exec claude")
	require.NoError(t, err)
	assert.NotEqual(t, "exec claude", cmd)
	require.NotEmpty(t, readyPath)
	cleanup()
	_, statErr := os.Stat(readyPath)
	assert.True(t, os.IsNotExist(statErr), "cleanup must remove the readiness file")
}

// A launched pane that cannot be settled is killed before the error returns,
// so the caller's uncommitted undo never leaves a live pane behind.
func TestSettleLaunchedPaneKillsThePaneOnFailure(t *testing.T) {
	isolateCacheAndDB(t)
	prev := clcommon.Default
	clcommon.Default = fakeTmux{} // every tmux query fails, so provenance cannot be read
	t.Cleanup(func() { clcommon.Default = prev })

	kills := 0
	err := settleLaunchedPane(launchSettleInput{
		exitGuard:      disabledExitLaunchGuard("spwn-settle", "spwn-settle", "gen-settle"),
		killLaunchPane: func() { kills++ },
		timing:         func(string, ...any) {},
		tmuxSession:    "spwn-settle", sessionID: "spwn-settle",
		params: &NewParams{ManagedLaunch: true}, h: harness.MustGet(harness.DefaultName),
		state: &SessionState{ID: "spwn-settle"},
	})
	require.ErrorContains(t, err, "capture managed launch provenance metadata")
	assert.Equal(t, 1, kills)
}
