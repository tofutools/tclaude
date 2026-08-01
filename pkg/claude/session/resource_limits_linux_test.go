//go:build linux

package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func fakeCurrentResourceCgroup(t *testing.T, controllers, enabled string) string {
	t.Helper()
	t.Setenv(ResourceDelegationDirEnv, "")
	t.Setenv("TMUX", "")
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	dir, err := currentCgroupDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.controllers"), []byte(controllers), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(enabled), 0o644))
	return dir
}

func TestPrepareResourceCgroupUsesExplicitExternalDelegationDir(t *testing.T) {
	current := fakeCurrentResourceCgroup(t, "cpu memory", "")
	external := filepath.Join(resourceCgroupRoot, "system.slice", "tclaude-tmux.service")
	require.NoError(t, os.MkdirAll(external, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(external, "cgroup.controllers"), []byte("cpu memory io"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(external, "cgroup.subtree_control"), nil, 0o644))
	t.Setenv(ResourceDelegationDirEnv, external)

	dir, cleanup, err := PrepareResourceCgroup(
		"external-session", sandboxpolicy.ResourceLimits{Memory: "256MB"})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.Equal(t, external, filepath.Dir(dir))
	assert.NotEqual(t, current, filepath.Dir(dir))
	assert.FileExists(t, filepath.Join(dir, "memory.max"))
}

func TestPaneRecoversExternalDelegationDirFromTmuxGlobalEnvironment(t *testing.T) {
	t.Setenv(ResourceDelegationDirEnv, "")
	t.Setenv("TMUX", "/tmp/tmux.sock,1,0")
	want := "/sys/fs/cgroup/system.slice/tclaude-tmux.service"
	swapTmux(t, &launchRecordingTmux{resourceEnv: want})

	assert.Equal(t, want, configuredResourceDelegationDir())
	assert.Equal(t, want, os.Getenv(ResourceDelegationDirEnv),
		"the recovered value must reach later launch preflights and child processes")
}

func TestValidateResourceDelegationDirRequiresContainedCPUAndMemoryRoot(t *testing.T) {
	fakeCurrentResourceCgroup(t, "cpu memory", "")
	external := filepath.Join(resourceCgroupRoot, "system.slice", "tclaude-tmux.service")
	require.NoError(t, os.MkdirAll(external, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(external, "cgroup.controllers"), []byte("cpu memory"), 0o644))

	got, err := ValidateResourceDelegationDir(external)
	require.NoError(t, err)
	assert.Equal(t, external, got)

	require.NoError(t, os.WriteFile(filepath.Join(external, "cgroup.controllers"), []byte("memory"), 0o644))
	_, err = ValidateResourceDelegationDir(external)
	assert.ErrorContains(t, err, "cpu controller")
	_, err = ValidateResourceDelegationDir(t.TempDir())
	assert.ErrorContains(t, err, "must be below")
}

func TestValidatePreparedResourceCgroupRejectsStoredPathFromPreviousDelegation(t *testing.T) {
	fakeCurrentResourceCgroup(t, "cpu memory", "")
	external := filepath.Join(resourceCgroupRoot, "system.slice", "tclaude-tmux.service")
	old := filepath.Join(resourceCgroupRoot, "system.slice", "tclaude-agentd.service", "tclaude-old")
	require.NoError(t, os.MkdirAll(external, 0o755))
	require.NoError(t, os.MkdirAll(old, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(old, "memory.max"), []byte("max"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(old, "cpu.max"), []byte("max 100000"), 0o644))
	t.Setenv(ResourceDelegationDirEnv, external)

	err := ValidatePreparedResourceCgroup(old, sandboxpolicy.ResourceLimits{})
	assert.ErrorContains(t, err, "outside the configured resource delegation directory")
}

func TestWrapResourceLimitedCommandRendersIndependentAxes(t *testing.T) {
	current := fakeCurrentResourceCgroup(t, "cpu memory io", "")
	cpu := 0.5
	wrapped, cleanup, err := wrapResourceLimitedCommand(
		"session-one", sandboxpolicy.ResourceLimits{Memory: "1.5GiB", CPU: &cpu}, "exec harness", true,
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	entries, err := os.ReadDir(current)
	require.NoError(t, err)
	var cgroup string
	for _, entry := range entries {
		if entry.IsDir() && len(entry.Name()) > len("tclaude-") && entry.Name()[:len("tclaude-")] == "tclaude-" {
			cgroup = filepath.Join(current, entry.Name())
		}
	}
	require.NotEmpty(t, cgroup)
	memory, err := os.ReadFile(filepath.Join(cgroup, "memory.max"))
	require.NoError(t, err)
	assert.Equal(t, "1610612736", string(memory))
	cpuMax, err := os.ReadFile(filepath.Join(cgroup, "cpu.max"))
	require.NoError(t, err)
	assert.Equal(t, "50000 100000", string(cpuMax))
	require.NoError(t, ValidatePreparedResourceCgroup(
		cgroup, sandboxpolicy.ResourceLimits{Memory: "1.5GiB", CPU: &cpu}))
	changedCPU := 1.0
	assert.Error(t, ValidatePreparedResourceCgroup(
		cgroup, sandboxpolicy.ResourceLimits{Memory: "1.5GiB", CPU: &changedCPU}))
	assert.Error(t, ValidatePreparedResourceCgroup(
		cgroup, sandboxpolicy.ResourceLimits{Memory: "1.5GiB"}),
		"removing CPU must not reuse a cgroup that retains the old quota")
	assert.Contains(t, wrapped, "resource-limit-exec")
	assert.Contains(t, wrapped, "exec harness")
	assert.Contains(t, wrapped, "--allow-unenforced")
	enabled, err := os.ReadFile(filepath.Join(current, "cgroup.subtree_control"))
	require.NoError(t, err)
	assert.Equal(t, "+memory +cpu", string(enabled))
}

func TestWrapResourceLimitedCommandRequiresOnlyConfiguredController(t *testing.T) {
	current := fakeCurrentResourceCgroup(t, "memory", "memory")
	wrapped, cleanup, err := wrapResourceLimitedCommand(
		"memory-only", sandboxpolicy.ResourceLimits{Memory: "512MB"}, "harness", false,
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.NotEmpty(t, wrapped)
	enabled, err := os.ReadFile(filepath.Join(current, "cgroup.subtree_control"))
	require.NoError(t, err)
	assert.Equal(t, "memory", string(enabled), "already-enabled controller is not rewritten")
}

func TestWrapResourceLimitedCommandFailsWhenControllerIsNotDelegated(t *testing.T) {
	fakeCurrentResourceCgroup(t, "memory", "")
	cpu := 1.0
	_, _, err := wrapResourceLimitedCommand(
		"cpu-missing", sandboxpolicy.ResourceLimits{CPU: &cpu}, "harness", false,
	)
	assert.ErrorContains(t, err, "Delegate=cpu memory")
}

func TestResourceDelegationDirUsesSystemdSupervisorParent(t *testing.T) {
	assert.Equal(t, "/sys/fs/cgroup/system.slice/tclaude-agentd.service",
		resourceDelegationDir("/sys/fs/cgroup/system.slice/tclaude-agentd.service/tclaude-supervisor"))
	assert.Equal(t, "/sys/fs/cgroup/user.slice/session.scope",
		resourceDelegationDir("/sys/fs/cgroup/user.slice/session.scope"))
}

func TestConfigureProcessResourceCgroupUsesAtomicClonePlacement(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("/bin/true")
	closeFD, err := ConfigureProcessResourceCgroup(cmd, dir)
	require.NoError(t, err)
	t.Cleanup(closeFD)
	require.NotNil(t, cmd.SysProcAttr)
	assert.True(t, cmd.SysProcAttr.UseCgroupFD)
	assert.GreaterOrEqual(t, cmd.SysProcAttr.CgroupFD, 0)
}

func TestValidatePreparedResourceCgroupAcceptsRemovedAxesOnlyAtMax(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.max"), []byte("max\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cpu.max"), []byte("max 100000\n"), 0o644))
	require.NoError(t, ValidatePreparedResourceCgroup(dir, sandboxpolicy.ResourceLimits{}))
}

func TestResourceCgroupOOMKilled(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.events"),
		[]byte("low 0\nhigh 0\nmax 1\noom 1\noom_kill 1\n"), 0o644))
	assert.True(t, ResourceCgroupOOMKilled(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.events"),
		[]byte("oom_kill 0\n"), 0o644))
	assert.False(t, ResourceCgroupOOMKilled(dir))
}

func TestResourceLimitExecOperatorOverrideFallsBackAfterAttachFailure(t *testing.T) {
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	dir := filepath.Join(resourceCgroupRoot, "tclaude-runtime-failure")
	require.NoError(t, os.Mkdir(dir, 0o755))
	// A directory at cgroup.procs deterministically makes the attachment write
	// fail on an ordinary test filesystem.
	require.NoError(t, os.Mkdir(filepath.Join(dir, "cgroup.procs"), 0o755))
	oldRecord := recordResourceLimitRuntimeOverrideForExec
	recorded := false
	recordResourceLimitRuntimeOverrideForExec = func(sessionID string, cause error) error {
		recorded = true
		assert.Equal(t, "session-runtime-failure", sessionID)
		assert.ErrorContains(t, cause, "attach workload")
		return nil
	}
	t.Cleanup(func() { recordResourceLimitRuntimeOverrideForExec = oldRecord })

	require.NoError(t, runResourceLimitExec(
		dir, "session-runtime-failure", "exit 0", true,
	))
	assert.True(t, recorded)
}

func TestResourceLimitExecFailsClosedAfterAttachFailure(t *testing.T) {
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	dir := filepath.Join(resourceCgroupRoot, "tclaude-runtime-failure")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "cgroup.procs"), 0o755))

	err := runResourceLimitExec(dir, "session-runtime-failure", "exit 0", false)
	assert.ErrorContains(t, err, "attach workload")
}

func TestResourceLimitExecFailsClosedWhenOverrideDisclosureCannotPersist(t *testing.T) {
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	dir := filepath.Join(resourceCgroupRoot, "tclaude-runtime-failure")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "cgroup.procs"), 0o755))
	oldRecord := recordResourceLimitRuntimeOverrideForExec
	recordResourceLimitRuntimeOverrideForExec = func(string, error) error {
		return errors.New("database unavailable")
	}
	t.Cleanup(func() { recordResourceLimitRuntimeOverrideForExec = oldRecord })

	err := runResourceLimitExec(dir, "session-runtime-failure", "exit 0", true)
	assert.ErrorContains(t, err, "required resource-limit override disclosure")
}

func TestResourceLimitChildExitCodeUsesShellSignalConvention(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "kill -KILL $$")
	err := cmd.Run()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 137, resourceLimitChildExitCode(exitErr))

	cmd = exec.Command("/bin/sh", "-c", "exit 23")
	err = cmd.Run()
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 23, resourceLimitChildExitCode(exitErr))
}
