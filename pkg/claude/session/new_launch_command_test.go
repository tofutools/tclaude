package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/launchundo"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Outside tclaude's sandbox there is no outer wall to build, so the spec is
// the zero value.
func TestPrepareTclaudeLayerLaunchIsANoOpWithoutTheOuterLayer(t *testing.T) {
	var undo launchundo.Undo
	spec, err := prepareTclaudeLayerLaunch(tclaudeLayerLaunchStepInput{
		h: harness.MustGet(harness.DefaultName), sessionID: "spwn-nolayer", cwd: testutil.CanonicalTempDir(t),
	}, &undo)
	require.NoError(t, err)
	assert.Equal(t, TclaudeLayerLaunchSpec{}, spec)
}

func testPaneCommandInput(t *testing.T, harnessName, sessionID string) launchPaneCommandInput {
	cwd := testutil.CanonicalTempDir(t)
	h := harness.MustGet(harnessName)
	return launchPaneCommandInput{
		h: h, params: &NewParams{}, spawnSpec: harness.SpawnSpec{Cwd: cwd},
		layerSpec: &TclaudeLayerLaunchSpec{}, sessionID: sessionID, cwd: cwd, exitGeneration: "gen-cmd",
		sandboxImplementation: sandboxpolicy.ImplementationHarnessBuiltin,
		timing:                func(string, ...any) {},
	}
}

// A harness-native launch gets its proxy spawn command unwrapped, and the
// stage timings runNew reports are still emitted from inside the step.
func TestAssembleLaunchPaneCommandRendersAnUnwrappedCommand(t *testing.T) {
	isolateCacheAndDB(t)
	require.NoError(t, SaveSessionState(&SessionState{ID: "spwn-cmd", Status: StatusIdle}))
	in := testPaneCommandInput(t, harness.DefaultName, "spwn-cmd")
	var stages []string
	in.timing = func(stage string, _ ...any) { stages = append(stages, stage) }

	cmd, _, err := assembleLaunchPaneCommand(in)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(cmd, "claude "), "an unwrapped Claude launch runs claude directly: %s", cmd)
	assert.NotContains(t, cmd, "bwrap", "a harness-native launch is not wrapped in tclaude's sandbox")
	assert.Equal(t, []string{"sandbox_and_command_prepared", "codex_version_checked"}, stages)
}

// OpenCode's tool-executing boundary belongs to its managed server, so its
// attach-only pane records none.
func TestAssembleLaunchPaneCommandRecordsNoBoundaryForOpenCode(t *testing.T) {
	isolateCacheAndDB(t)
	require.NoError(t, SaveSessionState(&SessionState{ID: "spwn-cmd-oc", Status: StatusIdle}))
	_, boundary, err := assembleLaunchPaneCommand(testPaneCommandInput(t, harness.OpenCodeName, "spwn-cmd-oc"))
	require.NoError(t, err)
	assert.Nil(t, boundary)
}
