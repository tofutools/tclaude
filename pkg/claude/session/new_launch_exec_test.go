package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// A harness-native Claude launch pins no executable (the pane resolves it on
// PATH), but the managed instructions root is still kept readable.
func TestResolveLaunchExecutableLeavesAnUnwrappedLaunchOnPath(t *testing.T) {
	exe, err := resolveLaunchExecutable(&NewParams{ManagedInstructionsRoot: "/managed"},
		harness.MustGet(harness.DefaultName), false, sandboxpolicy.RootHostInherited, nil)
	require.NoError(t, err)
	assert.Empty(t, exe.path)
	assert.Empty(t, exe.interpreter)
	assert.Equal(t, []string{"/managed"}, exe.readPaths)
}

// Under tclaude's sandbox the workspace is always writable and the managed
// instructions root readable, even with no sandbox profile; no Codex
// app-server means no runtime directory.
func TestPlanLaunchDirsGrantsTheWorkspaceUnderTheOuterLayer(t *testing.T) {
	cwd := testutil.CanonicalTempDir(t)
	dirs, err := planLaunchDirs(launchDirsInput{
		params: &NewParams{ManagedInstructionsRoot: "/managed"},
		h:      harness.MustGet(harness.DefaultName), cwd: cwd,
		sandboxImplementation: sandboxpolicy.ImplementationTclaudeLayer, outerLayer: true,
	})
	require.NoError(t, err)
	assert.Nil(t, dirs.codexRuntime)
	assert.Contains(t, dirs.write, CanonicalSandboxPath(cwd))
	assert.Contains(t, dirs.read, "/managed")
	assert.Empty(t, dirs.deny)
}

// A harness-native launch outside any sandbox profile is not handed the
// workspace as an extra write grant: the harness owns its cwd itself.
func TestPlanLaunchDirsLeavesAHarnessNativeWorkspaceToTheHarness(t *testing.T) {
	cwd := testutil.CanonicalTempDir(t)
	dirs, err := planLaunchDirs(launchDirsInput{
		params: &NewParams{}, h: harness.MustGet(harness.DefaultName), cwd: cwd,
		sandboxImplementation: sandboxpolicy.ImplementationHarnessBuiltin,
	})
	require.NoError(t, err)
	assert.NotContains(t, dirs.write, CanonicalSandboxPath(cwd))
}
