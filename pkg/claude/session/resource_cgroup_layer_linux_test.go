//go:build linux

package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// stubTclaudeLayerTooling makes the outer wall resolvable without depending on
// the test host's unprivileged user namespaces. What these tests are about is
// the cgroup the layer now also asks for, so the bubblewrap capability probe is
// exactly the environmental dependency they must not carry.
func stubTclaudeLayerTooling(t *testing.T) {
	t.Helper()
	oldLookPath, oldProbe := lookPathBwrap, probeBwrap
	t.Cleanup(func() { lookPathBwrap, probeBwrap = oldLookPath, oldProbe })
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrap = func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) error {
		return nil
	}
}

// The pane seam is where a tclaude-layer launch gets its boundary, and the
// request arrives through the implementation alone — a direct CLI launch and a
// CLI resume both carry no sandbox snapshot at all. A gate that reads only the
// authored limits silently skips the cgroup here.
func TestRunNewTclaudeLayerWithoutLimitsWrapsPaneInAccountingCgroup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	current := fakeCurrentResourceCgroup(t, "cpu memory", "")
	prevCheck := ClaudeAncestorCheck
	ClaudeAncestorCheck = func() bool { return false }
	t.Cleanup(func() { ClaudeAncestorCheck = prevCheck })
	stubTmuxOnPath(t)
	stubTclaudeLayerTooling(t)
	rec := &paneScriptCapturingTmux{launchRecordingTmux: &launchRecordingTmux{failNewSession: true}}
	swapTmux(t, rec)

	err := runNew(&NewParams{
		Label:       "spwn-layercg",
		Dir:         t.TempDir(),
		Detached:    true,
		SandboxImpl: string(sandboxpolicy.ImplementationTclaudeLayer),
	})
	require.Error(t, err, "the fake tmux refuses new-session")

	require.NotEmpty(t, rec.script, "the launch never reached tmux with a pane script")
	assert.Contains(t, rec.script, "resource-limit-exec",
		"tclaude owns this workload's boundary, so the pane must join a cgroup it can account for")
	assert.Contains(t, rec.script, filepath.Join(current, "tclaude-"),
		"the wrapper must point at a cgroup under the delegated parent")
}

// The difference from resource-only, at the seam that decides it. The layer was
// chosen for confinement the cgroup has no part in, so an undelegated host must
// still launch — on a fresh spawn too, where a resource-only launch is refused
// by name.
func TestRunNewTclaudeLayerLaunchesWhenTheHostHasNoDelegation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	t.Setenv(ResourceDelegationDirEnv, "")
	t.Setenv("TMUX", "")
	prevCheck := ClaudeAncestorCheck
	ClaudeAncestorCheck = func() bool { return false }
	t.Cleanup(func() { ClaudeAncestorCheck = prevCheck })
	stubTmuxOnPath(t)
	stubTclaudeLayerTooling(t)
	logged := captureWarnings(t)
	rec := &paneScriptCapturingTmux{launchRecordingTmux: &launchRecordingTmux{failNewSession: true}}
	swapTmux(t, rec)

	err := runNew(&NewParams{
		Label:       "spwn-layernodeleg",
		Dir:         t.TempDir(),
		Detached:    true,
		SandboxImpl: string(sandboxpolicy.ImplementationTclaudeLayer),
	})
	require.Error(t, err, "the fake tmux refuses new-session")
	assert.NotContains(t, err.Error(), "delegated cgroup v2",
		"counters are a bonus here; refusing would cost the operator the wall they came for")
	require.NotEmpty(t, rec.script, "the launch must still reach tmux")
	assert.NotContains(t, rec.script, "resource-limit-exec", "there is no cgroup to join")
	assert.Contains(t, logged.String(), "resource cgroup unavailable")
}
