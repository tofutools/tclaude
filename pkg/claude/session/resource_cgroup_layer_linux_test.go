//go:build linux

package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// stubTclaudeLayerTooling makes the outer wall resolvable without depending on
// what the test host happens to have installed. What these tests are about is
// the cgroup the layer now also asks for, so the two things a tclaude-layer
// launch needs before it can get that far — working unprivileged user
// namespaces, and a harness entry point frozen before the launch enters its
// filesystem namespace — are exactly the environmental dependencies they must
// not carry. A CI runner has neither.
func stubTclaudeLayerTooling(t *testing.T) {
	t.Helper()
	oldLookPath, oldProbe := lookPathBwrap, probeBwrap
	t.Cleanup(func() { lookPathBwrap, probeBwrap = oldLookPath, oldProbe })
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrap = func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) error {
		return nil
	}
	// ResolveClaudeLaunchExecutable is a PATH lookup plus a regular-and-executable
	// check, so a stub file satisfies it the same way stubTmuxOnPath satisfies
	// the tmux lookup. Nothing here executes it.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
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

// The other half of "it only tries". A boundary can be created perfectly well
// and still refuse the workload: cgroup v2 requires write access on the common
// ancestor of the mover and the target, and the pane's tmux server is not
// necessarily inside the delegated node the launch derived. Failing there would
// kill the harness and cost the operator the bubblewrap wall — the exact
// outcome the creation-side policy exists to prevent.
func TestResourceLimitExecOptionalBoundaryRunsTheWorkloadAfterAttachFailure(t *testing.T) {
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	dir := filepath.Join(resourceCgroupRoot, "tclaude-optional-attach")
	require.NoError(t, os.Mkdir(dir, 0o755))
	// A directory at cgroup.procs deterministically makes the attachment write
	// fail on an ordinary test filesystem.
	require.NoError(t, os.Mkdir(filepath.Join(dir, "cgroup.procs"), 0o755))
	accounting := captureAccountingDisclosure(t)
	overridden := captureOverrideDisclosure(t)

	require.NoError(t, runResourceLimitExec(
		dir, "session-optional-attach", "exit 0", false, false, false, true,
	), "the workload must run; only its counters were lost")
	assert.True(t, *accounting, "the counters the launch asked for and did not get must be disclosed")
	assert.False(t, *overridden,
		"no ceiling was authored, so the sticky operator override must not be recorded")
}

// The override is not merely unnecessary here, it is harmful: an override
// notice suppresses the boundary on every LATER launch of this conversation, so
// recording it for a bonus boundary would silently retire the accounting for
// good. Accounting wins even when the operator did tick the dashboard box.
func TestResourceLimitExecOptionalBoundaryOutranksTheOperatorOverride(t *testing.T) {
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	dir := filepath.Join(resourceCgroupRoot, "tclaude-optional-both")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "cgroup.procs"), 0o755))
	accounting := captureAccountingDisclosure(t)
	overridden := captureOverrideDisclosure(t)

	require.NoError(t, runResourceLimitExec(
		dir, "session-optional-both", "exit 0", true, false, false, true,
	))
	assert.True(t, *accounting)
	assert.False(t, *overridden, "an override notice here would be sticky and wrong")
}

// A boundary that vanished between preparation and the pane takes the same
// answer, and it has to be taken BEFORE the workload is forked — so this path
// execs the harness directly rather than gating it on a cgroup that is gone.
func TestResourceLimitExecOptionalBoundaryExecsWhenTheCgroupIsGone(t *testing.T) {
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	dir := filepath.Join(resourceCgroupRoot, "tclaude-optional-gone")
	accounting := captureAccountingDisclosure(t)
	var execArgv []string
	oldExec := resourceLimitExecReplaceProcess
	resourceLimitExecReplaceProcess = func(_ string, argv []string, _ []string) error {
		execArgv = argv
		return nil
	}
	t.Cleanup(func() { resourceLimitExecReplaceProcess = oldExec })

	require.NoError(t, runResourceLimitExec(
		dir, "session-optional-gone", "exec harness", false, false, false, true,
	))
	assert.True(t, *accounting)
	require.NotEmpty(t, execArgv)
	assert.Equal(t, "exec harness", execArgv[len(execArgv)-1],
		"the harness command still runs, just with no boundary around it")

	// A required boundary in the same state still fails closed.
	err := runResourceLimitExec(dir, "session-required-gone", "exec harness", false, false, false, false)
	assert.ErrorContains(t, err, "cgroup is invalid")
}

// An argument shape no launch path can produce is a different matter: running
// the command anyway would honor input the wrapper is supposed to reject.
func TestResourceLimitExecOptionalBoundaryStillRefusesAnInvalidPath(t *testing.T) {
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })

	err := runResourceLimitExec(
		filepath.Join(resourceCgroupRoot, "not-a-tclaude-cgroup"),
		"session-bad-path", "exec harness", false, false, false, true)
	assert.ErrorContains(t, err, "invalid resource cgroup path")
}

// captureAccountingDisclosure swaps the accounting-unavailable recorder and
// reports whether the wrapper reached it.
func captureAccountingDisclosure(t *testing.T) *bool {
	t.Helper()
	recorded := false
	previous := recordResourceCgroupUnavailableForExec
	recordResourceCgroupUnavailableForExec = func(_ string, cause error) error {
		recorded = true
		assert.Error(t, cause)
		return nil
	}
	t.Cleanup(func() { recordResourceCgroupUnavailableForExec = previous })
	return &recorded
}

// captureOverrideDisclosure swaps the sticky operator-override recorder, so a
// test can assert the wrapper did NOT reach it.
func captureOverrideDisclosure(t *testing.T) *bool {
	t.Helper()
	recorded := false
	previous := recordResourceLimitRuntimeOverrideForExec
	recordResourceLimitRuntimeOverrideForExec = func(string, error) error {
		recorded = true
		return nil
	}
	t.Cleanup(func() { recordResourceLimitRuntimeOverrideForExec = previous })
	return &recorded
}
