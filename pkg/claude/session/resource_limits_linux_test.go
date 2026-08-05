//go:build linux

package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
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
	assert.Equal(t, "-N", ExternalTmuxNoStartArgs("new-session")[0],
		"a pre-existing pane must adopt external mode before its next launch")
}

func TestPaneTreatsMissingTmuxGlobalAsAuthoritativeLegacyMode(t *testing.T) {
	t.Setenv(ResourceDelegationDirEnv,
		"/sys/fs/cgroup/system.slice/tclaude-tmux.service")
	t.Setenv("TMUX", "/tmp/tmux.sock,1,0")
	swapTmux(t, &launchRecordingTmux{resourceEnvGone: true})

	assert.Empty(t, ExternalResourceDelegationDir())
	assert.Empty(t, os.Getenv(ResourceDelegationDirEnv))
	assert.Equal(t, []string{"new-session"}, ExternalTmuxNoStartArgs("new-session"),
		"a pre-existing pane must stop using external mode after agentd clears it")
}

func TestTclaudePaneCannotAutoStartServerWhenModeProbeFails(t *testing.T) {
	t.Setenv(ResourceDelegationDirEnv, "")
	t.Setenv("TMUX", "/tmp/tmux-1000/tclaude,123,0")
	swapTmux(t, &launchRecordingTmux{failResourceEnv: true})

	assert.Empty(t, ExternalResourceDelegationDir(),
		"the disappeared server cannot reveal the newly enabled external root")
	assert.Equal(t, "-N", ExternalTmuxNoStartArgs("new-session")[0],
		"a pane from the named tclaude server must never replace its dead server")
}

func TestExternalLaunchCannotAutoStartServerAfterSuccessfulProbe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMUX", "")
	oldRoot, oldProc := resourceCgroupRoot, resourceProcRoot
	resourceCgroupRoot, resourceProcRoot = t.TempDir(), t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot, resourceProcRoot = oldRoot, oldProc })
	external := filepath.Join(resourceCgroupRoot, "system.slice", "tclaude-tmux.service")
	require.NoError(t, os.MkdirAll(filepath.Join(resourceProcRoot, "4242"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(resourceProcRoot, "4242", "cgroup"),
		[]byte("0::/system.slice/tclaude-tmux.service/tclaude-tmux\n"), 0o644))
	t.Setenv(ResourceDelegationDirEnv, external)
	rec := &launchRecordingTmux{serverPID: 4242, failNewSession: true}
	swapTmux(t, rec)

	err := launchDetachedTmuxSession("external-race", t.TempDir(), "exec claude")
	require.Error(t, err)
	launches := rec.newSessions()
	require.Len(t, launches, 1)
	assert.Equal(t, "-N", launches[0][0],
		"the actual new-session client must refuse to start a replacement server")
}

func TestValidateExternalTmuxServerCgroupRejectsWrongUnit(t *testing.T) {
	oldRoot, oldProc := resourceCgroupRoot, resourceProcRoot
	resourceCgroupRoot, resourceProcRoot = t.TempDir(), t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot, resourceProcRoot = oldRoot, oldProc })
	require.NoError(t, os.MkdirAll(filepath.Join(resourceProcRoot, "77"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(resourceProcRoot, "77", "cgroup"),
		[]byte("0::/system.slice/tclaude-agentd.service/tclaude-supervisor\n"), 0o644))
	external := filepath.Join(resourceCgroupRoot, "system.slice", "tclaude-tmux.service")

	err := ValidateExternalTmuxServerCgroup(77, external)
	assert.ErrorContains(t, err, "outside")
}

func TestClearResourceDelegationFromTmuxRemovesStaleGlobalValue(t *testing.T) {
	t.Setenv(ResourceDelegationDirEnv, "")
	rec := &launchRecordingTmux{resourceEnv: "/sys/fs/cgroup/old.service"}
	swapTmux(t, rec)

	require.NoError(t, ClearResourceDelegationFromTmux())
	require.Len(t, rec.argv, 2)
	assert.Equal(t, []string{"-N", "set-environment", "-gu", ResourceDelegationDirEnv}, rec.argv[1])
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

func TestPrepareResourceCgroupCreatesAccountingBoundaryWithoutLimits(t *testing.T) {
	current := fakeCurrentResourceCgroup(t, "cpu memory io", "")

	dir, cleanup, err := PrepareResourceCgroup("accounting-only", sandboxpolicy.ResourceLimits{})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.Equal(t, current, filepath.Dir(dir))
	assert.NoFileExists(t, filepath.Join(dir, "memory.max"), "no ceiling was authored")
	assert.NoFileExists(t, filepath.Join(dir, "cpu.max"))
	enabled, err := os.ReadFile(filepath.Join(current, "cgroup.subtree_control"))
	require.NoError(t, err)
	// The counters and the memory.events OOM attribution are the whole point, and
	// both need their controller enabled in the parent.
	assert.Equal(t, "+memory +cpu", string(enabled))
	require.NoError(t, ValidatePreparedResourceCgroup(dir, sandboxpolicy.ResourceLimits{}),
		"a limitless boundary must still validate for a managed-server relaunch")
}

func TestPrepareResourceCgroupAccountingDegradesWhenControllerIsMissing(t *testing.T) {
	current := fakeCurrentResourceCgroup(t, "memory io", "")

	dir, cleanup, err := PrepareResourceCgroup("accounting-partial", sandboxpolicy.ResourceLimits{})
	require.NoError(t, err, "a missing controller costs counters, not enforcement, so it must not refuse")
	t.Cleanup(cleanup)
	assert.DirExists(t, dir)
	enabled, err := os.ReadFile(filepath.Join(current, "cgroup.subtree_control"))
	require.NoError(t, err)
	assert.Equal(t, "+memory", string(enabled), "only the delegated controller is enabled")
}

func TestPrepareResourceCgroupAccountingSurvivesUnwritableSubtreeControl(t *testing.T) {
	current := fakeCurrentResourceCgroup(t, "cpu memory", "")
	readOnlyCgroupNode(t, filepath.Join(current, "cgroup.subtree_control"))

	dir, cleanup, err := PrepareResourceCgroup("accounting-no-controllers", sandboxpolicy.ResourceLimits{})
	require.NoError(t, err, "the boundary is still worth creating for its process tracking")
	t.Cleanup(cleanup)
	assert.DirExists(t, dir)
}

func TestWrapResourceLimitedCommandWrapsPaneWithoutLimits(t *testing.T) {
	fakeCurrentResourceCgroup(t, "cpu memory", "cpu memory")

	wrapped, cleanup, err := wrapResourceLimitedCommand(
		"accounting-pane", sandboxpolicy.ResourceLimits{}, "exec harness", false)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.Contains(t, wrapped, "resource-limit-exec",
		"the pane must still join the boundary, or nothing is accounted to it")
	assert.Contains(t, wrapped, "exec harness")
}

func TestWrapResourceLimitedCommandFailsWhenControllerIsNotDelegated(t *testing.T) {
	fakeCurrentResourceCgroup(t, "memory", "")
	cpu := 1.0
	_, _, err := wrapResourceLimitedCommand(
		"cpu-missing", sandboxpolicy.ResourceLimits{CPU: &cpu}, "harness", false,
	)
	assert.ErrorContains(t, err, "Delegate=cpu memory")
}

// readOnlyCgroupNode makes a prepared cgroup directory or interface file reject
// writes the way an undelegated node does for an unprivileged launch.
func readOnlyCgroupNode(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root may write an undelegated cgroup node, so the denial cannot be simulated")
	}
	require.NoError(t, os.Chmod(path, 0o555))
	t.Cleanup(func() { _ = os.Chmod(path, 0o755) })
}

// fakeDerivedResourceCgroup pins the unified path this process appears to be in,
// so a test controls which delegated parent the derivation reaches instead of
// inheriting the host's own /proc/self/cgroup.
func fakeDerivedResourceCgroup(t *testing.T, unified, controllers, enabled string) string {
	t.Helper()
	t.Setenv(ResourceDelegationDirEnv, "")
	t.Setenv("TMUX", "")
	oldRoot, oldProc := resourceCgroupRoot, resourceProcRoot
	resourceCgroupRoot, resourceProcRoot = t.TempDir(), t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot, resourceProcRoot = oldRoot, oldProc })
	require.NoError(t, os.MkdirAll(filepath.Join(resourceProcRoot, "self"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(resourceProcRoot, "self", "cgroup"),
		[]byte("0::"+unified+"\n"), 0o644))
	current, err := currentCgroupDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(current, 0o755))
	delegation := resourceDelegationDir(current)
	require.NoError(t, os.WriteFile(filepath.Join(delegation, "cgroup.controllers"), []byte(controllers), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(delegation, "cgroup.subtree_control"), []byte(enabled), 0o644))
	return delegation
}

func TestPrepareResourceCgroupExplainsUndelegatedRootCgroup(t *testing.T) {
	// A container or an unshared cgroup namespace reports the unified path "/",
	// so the derivation lands on the root of whatever is mounted there.
	delegation := fakeDerivedResourceCgroup(t, "/", "cpu memory", "cpu memory")
	require.Equal(t, resourceCgroupRoot, delegation)
	readOnlyCgroupNode(t, delegation)

	_, _, err := PrepareResourceCgroup("cgroup-test", sandboxpolicy.ResourceLimits{Memory: "256MB"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "derived the delegated parent from /proc/self/cgroup")
	assert.ErrorContains(t, err, "root of the mounted hierarchy")
	assert.ErrorContains(t, err, "unshared cgroup namespace")
	assert.ErrorContains(t, err, "nsdelegate")
	assert.ErrorContains(t, err, "DelegateSubgroup="+resourceSupervisorCgroup)
	assert.NotContains(t, err.Error(), "--resource-delegation-dir",
		"no path under a hierarchy this process cannot write is worth pointing the flag at")
}

func TestPrepareResourceCgroupExplainsUnwritableDelegatedNode(t *testing.T) {
	fakeCurrentResourceCgroup(t, "cpu memory", "memory")
	external := filepath.Join(resourceCgroupRoot, "system.slice", "tclaude-tmux.service")
	require.NoError(t, os.MkdirAll(external, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(external, "cgroup.controllers"), []byte("cpu memory"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(external, "cgroup.subtree_control"), []byte("cpu memory"), 0o644))
	t.Setenv(ResourceDelegationDirEnv, external)
	readOnlyCgroupNode(t, external)

	_, _, err := PrepareResourceCgroup("cgroup-test", sandboxpolicy.ResourceLimits{Memory: "256MB"})
	require.Error(t, err)
	assert.ErrorContains(t, err, external+" is not writable by uid")
	assert.ErrorContains(t, err, "Delegate=cpu memory")
	assert.NotContains(t, err.Error(), "root of the mounted hierarchy",
		"an explicitly configured node must not be diagnosed as the hierarchy root")
}

func TestPrepareResourceCgroupExplainsUndelegatedControllerEnable(t *testing.T) {
	delegation := fakeDerivedResourceCgroup(t,
		"/system.slice/tclaude-agentd.service/"+resourceSupervisorCgroup, "cpu memory", "")
	require.NotEqual(t, resourceCgroupRoot, delegation)
	readOnlyCgroupNode(t, delegation)
	require.NoError(t, os.Chmod(filepath.Join(delegation, "cgroup.subtree_control"), 0o444))

	_, _, err := PrepareResourceCgroup("cgroup-test", sandboxpolicy.ResourceLimits{Memory: "256MB"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "enable delegated cgroup v2 controllers memory")
	assert.ErrorContains(t, err, delegation+" is not writable by uid",
		"a refused controller enable must name the delegation it could not configure")
}

func TestDelegationWriteRefusedCoversReadOnlyCgroupMount(t *testing.T) {
	assert.True(t, delegationWriteRefused(&os.PathError{Err: syscall.EACCES}))
	// A sandbox that binds the hierarchy read-only refuses the same writes with
	// EROFS, and that launch needs the same delegation diagnosis.
	assert.True(t, delegationWriteRefused(&os.PathError{Err: syscall.EROFS}))
	assert.False(t, delegationWriteRefused(&os.PathError{Err: syscall.EBUSY}))
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
