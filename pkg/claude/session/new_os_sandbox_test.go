package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Who chose the harness's builtin mode: the daemon's tier when it passes
// one, the human for a direct launch that names a mode, and nobody for a
// managed launch that passed nothing. Without the outer layer the verdict is
// the harness's own.
func TestResolveLaunchOSSandboxAttributesTheModeChoice(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	claude := harness.MustGet(harness.DefaultName)
	cwd := t.TempDir()
	mode := harness.ClaudeSandboxOn

	got := resolveLaunchOSSandbox(claude, &NewParams{}, mode, cwd, false, tclaudeLayerNetwork{})
	assert.Equal(t, harness.SandboxChosenExplicitly, got.chosenBy, "a direct launch naming a mode chose it")
	assert.Equal(t, harness.ResolveLaunchOSSandbox(claude, mode, got.chosenBy, cwd), got.launch)
	assert.Empty(t, got.bwrapBinary)
	assert.NoError(t, got.bwrapCapabilityErr)

	got = resolveLaunchOSSandbox(claude, &NewParams{ManagedLaunch: true}, mode, cwd, false, tclaudeLayerNetwork{})
	assert.Empty(t, got.chosenBy, "a managed launch with no tier stays unattributed")

	got = resolveLaunchOSSandbox(claude, &NewParams{ManagedLaunch: true, SandboxChosenBy: "group\tprofile"}, mode, cwd, false, tclaudeLayerNetwork{})
	assert.Equal(t, harness.SanitizeSandboxChosenBy("group\tprofile"), got.chosenBy, "the daemon's tier is scrubbed at the point of record")
}

// The harness-posture gate belongs to the tclaude-layer implementation:
// it refuses a Copilot whose own sandbox the launch cannot turn off, and
// never runs for a launch without the outer layer.
func TestGateTclaudeLayerHarnessPostureRunsOnlyForTheOuterLayer(t *testing.T) {
	home, _ := copilotLaunchRoot(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".copilot"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(home, ".copilot", harness.CopilotSettingsFileName),
		[]byte(`{"sandbox":{"enabled":true}}`), 0o600))
	copilot := harness.MustGet(harness.CopilotName)

	assert.Error(t, GateTclaudeLayerHarnessPosture(copilot, true, nil, nil))
	assert.NoError(t, GateTclaudeLayerHarnessPosture(copilot, false, nil, nil))
}

// An implementation that enforces nothing still discloses the rules it
// ignores, on both the launch snapshot and the effective one it records.
func TestDiscloseUnconfinedAccessRulesNamesTheIgnoredRules(t *testing.T) {
	snapshots := func() (*sandboxpolicy.Snapshot, *sandboxpolicy.Snapshot) {
		launch, effective := &sandboxpolicy.Snapshot{}, &sandboxpolicy.Snapshot{}
		launch.Effective.FilesystemRoot = sandboxpolicy.FilesystemRootSeparate
		effective.Effective.FilesystemRoot = sandboxpolicy.FilesystemRootSeparate
		return launch, effective
	}
	axes := func(s *sandboxpolicy.Snapshot) []string {
		var out []string
		for _, n := range s.Effective.AccessNotices {
			out = append(out, n.Axis)
		}
		return out
	}

	launch, effective := snapshots()
	discloseUnconfinedAccessRules(sandboxpolicy.ImplementationOff, true, launch, effective)
	assert.Equal(t, []string{"access_rules"}, axes(launch))
	assert.Equal(t, []string{"access_rules"}, axes(effective))

	launch, effective = snapshots()
	discloseUnconfinedAccessRules(sandboxpolicy.ImplementationOff, false, launch, effective)
	assert.Empty(t, axes(launch), "a confining launch plans its rules instead")
	assert.Empty(t, axes(effective))
}
