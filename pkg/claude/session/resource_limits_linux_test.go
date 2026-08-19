//go:build linux

package session

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// fakeCurrentResourceCgroup fakes the launching process's own cgroup and returns
// the delegated parent a launch derives from it.
//
// The unified path is pinned rather than read from the host, and pinned to the
// shape the docs prescribe: agentd in a tclaude-supervisor subgroup whose parent
// is the delegation. Reading the real /proc/self/cgroup made every test using
// this helper exercise whichever shape the host happened to be configured for —
// and write its controller files to the process's own cgroup rather than the
// derived delegation, leaving the delegation bare on correctly configured hosts.
// resourceDelegationDir's own unit test covers the derivation for both shapes.
func fakeCurrentResourceCgroup(t *testing.T, controllers, enabled string) string {
	t.Helper()
	return fakeDerivedResourceCgroup(t,
		"/system.slice/tclaude-agentd.service/"+resourceSupervisorCgroup, controllers, enabled)
}

// captureWarnings redirects slog for one test. An accounting degradation has no
// on-disk trace, so the log line is the assertable disclosure.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
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
		// The supervisor subgroup shares the tclaude- prefix and sits beside the
		// workload cgroups in every correctly delegated tree, so a scan for the
		// launch's own boundary has to step over it the way production does.
		if entry.Name() == resourceSupervisorCgroup {
			continue
		}
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

	logged := captureWarnings(t)

	dir, cleanup, err := PrepareResourceCgroup("accounting-partial", sandboxpolicy.ResourceLimits{})
	require.NoError(t, err, "a missing controller costs counters, not enforcement, so it must not refuse")
	t.Cleanup(cleanup)
	assert.DirExists(t, dir)
	enabled, err := os.ReadFile(filepath.Join(current, "cgroup.subtree_control"))
	require.NoError(t, err)
	assert.Equal(t, "+memory", string(enabled), "only the delegated controller is enabled")
	assert.Contains(t, logged.String(), "controller=cpu",
		"an unavailable counter is invisible unless the launch says which one")
}

func TestPrepareResourceCgroupAccountingSurvivesUnwritableSubtreeControl(t *testing.T) {
	current := fakeCurrentResourceCgroup(t, "cpu memory", "")
	readOnlyCgroupNode(t, filepath.Join(current, "cgroup.subtree_control"))
	logged := captureWarnings(t)

	dir, cleanup, err := PrepareResourceCgroup("accounting-no-controllers", sandboxpolicy.ResourceLimits{})
	require.NoError(t, err, "the boundary is still worth creating for its process tracking")
	t.Cleanup(cleanup)
	assert.DirExists(t, dir)
	// The refused write is the only proof the accounting branch ran: with no
	// ceiling there is nothing else to observe in the cgroup afterwards.
	assert.Contains(t, logged.String(), "cannot enable delegated controllers for accounting")
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
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	delegation := filepath.Join(resourceCgroupRoot, "delegated")
	dir := filepath.Join(delegation, "tclaude-removed-axes")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	// Pin both inputs to ExternalResourceDelegationDir. Otherwise a developer
	// running the suite inside tmux can inherit the host's live delegation and
	// turn this value-validation test into an accidental containment test.
	t.Setenv("TMUX", "")
	t.Setenv(ResourceDelegationDirEnv, delegation)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.max"), []byte("max\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cpu.max"), []byte("max 100000\n"), 0o644))
	require.NoError(t, ValidatePreparedResourceCgroup(dir, sandboxpolicy.ResourceLimits{}))
}

func TestReadResourceCgroupOOMKillsReadsTheCumulativeCounter(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.events"),
		[]byte("low 0\nhigh 0\nmax 1\noom 1\noom_kill 3\n"), 0o644))
	kills, known := ReadResourceCgroupOOMKills(dir).Kills()
	assert.True(t, known)
	assert.Equal(t, uint64(3), kills)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.events"),
		[]byte("oom_kill 0\n"), 0o644))
	kills, known = ReadResourceCgroupOOMKills(dir).Kills()
	assert.True(t, known, "zero is a reading, not the absence of one")
	assert.Equal(t, uint64(0), kills)

	// A cgroup whose counters were never delegated has no memory.events at all.
	// Reading that as zero would let a later valid reading look like a rise.
	_, known = ReadResourceCgroupOOMKills(t.TempDir()).Kills()
	assert.False(t, known)
	_, known = ReadResourceCgroupOOMKills("").Kills()
	assert.False(t, known, "a launch with no boundary must not read a relative path")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.events"),
		[]byte("oom_kill notanumber\n"), 0o644))
	_, known = ReadResourceCgroupOOMKills(dir).Kills()
	assert.False(t, known)
}

func oomCount(kills uint64) ResourceCgroupOOMCount {
	return ResourceCgroupOOMCount{kills: kills, known: true}
}

// waitErrorFrom runs a throwaway shell and returns what waiting on it produced,
// so a test can build the real *exec.ExitError shapes the attribution reads
// rather than a hand-rolled stand-in.
func waitErrorFrom(t *testing.T, script string) error {
	t.Helper()
	return exec.Command("/bin/sh", "-c", script).Run()
}

// The counter rising is not the same fact as this workload dying of it. A
// long-lived agent that survives one greedy child, and a durable managed-server
// boundary carrying kills over from an earlier relaunch, both look identical to
// a bare `oom_kill > 0` read.
func TestResourceLimitOOMDeathRequiresBothARiseAndADeathByKill(t *testing.T) {
	// The shape the pane path actually produces: the kernel kills the harness,
	// and the shell waited on relays that as 128+SIGKILL. Asserted here rather
	// than assumed, because requiring the signalled shape alone made the
	// attribution unreachable in production.
	relayed := waitErrorFrom(t, "/bin/sleep 30 & victim=$!; kill -KILL $victim; wait $victim")
	var relayedExit *exec.ExitError
	require.ErrorAs(t, relayed, &relayedExit)
	require.False(t, relayedExit.Sys().(syscall.WaitStatus).Signaled(),
		"the wrapper shell survives its child and exits normally")
	require.Equal(t, 137, relayedExit.ExitCode())

	// The shape a directly killed workload produces.
	signalled := waitErrorFrom(t, "kill -KILL $$")
	require.Error(t, signalled)

	assert.True(t, resourceLimitOOMDeath(oomCount(0), oomCount(1), relayed),
		"a harness killed under its wrapper shell is the ordinary production case")
	assert.True(t, resourceLimitOOMDeath(oomCount(0), oomCount(1), signalled),
		"a workload killed directly is the same death, differently reported")
	assert.True(t, resourceLimitOOMDeath(oomCount(4), oomCount(5), signalled),
		"a durable boundary attributes the rise, not the accumulated total")

	assert.False(t, resourceLimitOOMDeath(oomCount(1), oomCount(1), signalled),
		"a kill inherited from an earlier launch says nothing about this one")
	assert.False(t, resourceLimitOOMDeath(oomCount(0), oomCount(1), nil),
		"an agent that survived the kill and later exited cleanly did not die of it")
	assert.False(t, resourceLimitOOMDeath(oomCount(0), oomCount(1), waitErrorFrom(t, "exit 1")),
		"a non-zero exit after a surviving descendant was killed is an ordinary failure")
	assert.False(t, resourceLimitOOMDeath(oomCount(0), oomCount(1), waitErrorFrom(t, "kill -TERM $$")),
		"the OOM killer sends SIGKILL, so another signal is another cause")
	assert.False(t, resourceLimitOOMDeath(ResourceCgroupOOMCount{}, oomCount(1), signalled),
		"without a baseline there is no rise to establish, only an unknown")
	assert.False(t, resourceLimitOOMDeath(oomCount(0), ResourceCgroupOOMCount{}, signalled),
		"a counter that vanished at exit proves nothing either")
}

// The operator-visible bug this guards: an agent that shrugged off an OOM kill
// and was later exited by hand was still stamped resource_limit_oom.
func TestResourceLimitExecDoesNotAttributeASurvivedOOMToACleanExit(t *testing.T) {
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	dir := filepath.Join(resourceCgroupRoot, "tclaude-oom-survivor")
	require.NoError(t, os.Mkdir(dir, 0o755))
	events := filepath.Join(dir, "memory.events")
	require.NoError(t, os.WriteFile(events, []byte("oom_kill 0\n"), 0o644))

	oldRecord := recordResourceLimitOOMForExec
	recorded := false
	recordResourceLimitOOMForExec = func(string) error { recorded = true; return nil }
	t.Cleanup(func() { recordResourceLimitOOMForExec = oldRecord })

	// The workload outlives a kill in its own cgroup, then exits of its own
	// accord — exactly the shape that used to be reported as an OOM death.
	require.NoError(t, runResourceLimitExec(
		dir, "session-oom-survivor",
		"printf 'oom_kill 1\\n' > '"+events+"'; exit 0", false, false, false,
	))
	assert.False(t, recorded,
		"a workload that exited cleanly must not be recorded as killed by its ceiling")
	kills, known := ReadResourceCgroupOOMKills(dir).Kills()
	require.True(t, known)
	assert.Equal(t, uint64(1), kills,
		"the kill itself must still be observable; only the attribution is withheld")
}

// The other half of the same rule, and the case the first version of this change
// got wrong: a real ceiling hit must still be recorded. It has to run through
// runResourceLimitExec rather than the predicate alone, because what the pane
// path produces — a wrapper shell relaying its harness's death — is exactly what
// a predicate-only test cannot show.
func TestResourceLimitExecRecordsAKillRelayedByTheWrapperShell(t *testing.T) {
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	dir := filepath.Join(resourceCgroupRoot, "tclaude-oom-victim")
	require.NoError(t, os.Mkdir(dir, 0o755))
	events := filepath.Join(dir, "memory.events")
	require.NoError(t, os.WriteFile(events, []byte("oom_kill 0\n"), 0o644))

	oldRecord := recordResourceLimitOOMForExec
	recordedFor := ""
	recordResourceLimitOOMForExec = func(sessionID string) error { recordedFor = sessionID; return nil }
	t.Cleanup(func() { recordResourceLimitOOMForExec = oldRecord })
	oldExit := resourceLimitExecExit
	exitCode := 0
	resourceLimitExecExit = func(code int) { exitCode = code }
	t.Cleanup(func() { resourceLimitExecExit = oldExit })

	// The kernel picks the harness, not the shell wrapping it, so the process
	// this waits on outlives the kill and reports it as a status.
	require.NoError(t, runResourceLimitExec(
		dir, "session-oom-victim",
		"printf 'oom_kill 1\\n' > '"+events+"'; /bin/sleep 30 & victim=$!; kill -KILL $victim; wait $victim",
		false, false, false,
	))
	assert.Equal(t, "session-oom-victim", recordedFor,
		"a ceiling that actually killed the workload has to reach the session's exit reason")
	assert.Equal(t, 137, exitCode, "the pane still reports the workload's own status")
}

func TestResourceDelegationBusyHintNamesTheInternalProcessRule(t *testing.T) {
	delegation := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(delegation, "cgroup.procs"),
		[]byte("1\n2\n3\n"), 0o644))
	busy := &os.PathError{Op: "write", Path: delegation, Err: syscall.EBUSY}

	hint := resourceDelegationBusyHint(delegation, busy)
	assert.Contains(t, hint, "still holds 3 process(es)",
		"the count is what turns an opaque EBUSY into something an operator can act on")
	assert.Contains(t, hint, "process-free")
	assert.Contains(t, hint, "no effect on a scope",
		"a scope is where the documented DelegateSubgroup fix silently does nothing")
	assert.Contains(t, hint, resourceSupervisorCgroup)

	assert.Empty(t, resourceDelegationBusyHint(delegation, &os.PathError{Err: syscall.EACCES}),
		"a permission denial has a different fix and must not be diagnosed as this one")

	// The access control that refuses the controller write can also hide
	// cgroup.procs. Falling silent there would route the operator back to the
	// DelegateSubgroup advice this diagnosis exists to correct, so the rule and
	// the fix still have to be stated — without a count nothing established.
	uncounted := resourceDelegationBusyHint(t.TempDir(), busy)
	assert.Contains(t, uncounted, "could not be read")
	assert.Contains(t, uncounted, "no effect on a scope")
	assert.NotContains(t, uncounted, "0 process(es)")

	require.NoError(t, os.WriteFile(filepath.Join(delegation, "cgroup.procs"), nil, 0o644))
	assert.Empty(t, resourceDelegationBusyHint(delegation, busy),
		"a node proven process-free refused the write for some other reason")
}

// refuseControllerEnable makes the delegated node reject the controller write the
// way a node that still holds processes does. No ordinary filesystem produces
// EBUSY for this write, so the refusal has to be injected at the seam.
func refuseControllerEnable(t *testing.T, delegation string, held int) {
	t.Helper()
	pids := make([]byte, 0, held*2)
	for i := 1; i <= held; i++ {
		pids = append(pids, []byte(strconv.Itoa(i)+"\n")...)
	}
	require.NoError(t, os.WriteFile(filepath.Join(delegation, "cgroup.procs"), pids, 0o644))
	previous := enableDelegatedControllers
	enableDelegatedControllers = func(dir string, toEnable []string) error {
		assert.Equal(t, delegation, dir, "the launch must configure the node it derived")
		assert.NotEmpty(t, toEnable, "a refusal is only reachable when something was requested")
		return &os.PathError{Op: "write", Path: filepath.Join(dir, "cgroup.subtree_control"), Err: syscall.EBUSY}
	}
	t.Cleanup(func() { enableDelegatedControllers = previous })
}

func TestPrepareResourceCgroupAccountingNamesTheInternalProcessRule(t *testing.T) {
	current := fakeCurrentResourceCgroup(t, "cpu memory", "")
	refuseControllerEnable(t, current, 13)
	logged := captureWarnings(t)

	dir, cleanup, err := PrepareResourceCgroup("acct-busy", sandboxpolicy.ResourceLimits{})
	require.NoError(t, err, "no ceiling depends on these controllers")
	t.Cleanup(cleanup)
	assert.DirExists(t, dir)
	assert.Contains(t, logged.String(), "still holds 13 process(es)",
		"the degradation warning is the only trace, so it has to carry the cause")
	assert.Contains(t, logged.String(), "no effect on a scope")
}

func TestPrepareResourceCgroupEnforcedNamesTheInternalProcessRule(t *testing.T) {
	current := fakeCurrentResourceCgroup(t, "cpu memory", "")
	refuseControllerEnable(t, current, 13)

	_, _, err := PrepareResourceCgroup("enforced-busy", sandboxpolicy.ResourceLimits{Memory: "256MB"})
	require.Error(t, err, "a ceiling that cannot be enforced must refuse the launch")
	assert.ErrorContains(t, err, "still holds 13 process(es)")
	assert.ErrorContains(t, err, "no effect on a scope")
	assert.NotContains(t, err.Error(), "is not writable by uid",
		"a busy node is delegated correctly; diagnosing it as undelegated sends the operator the wrong way")
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
		dir, "session-runtime-failure", "exit 0", true, false, false,
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

	err := runResourceLimitExec(dir, "session-runtime-failure", "exit 0", false, false, false)
	assert.ErrorContains(t, err, "attach workload")
}

// fakeSecurityLabel pins what /proc/self/attr/current reports, so a test can
// exercise a confined launch on a host that runs the suite unconfined.
func fakeSecurityLabel(t *testing.T, label string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(resourceProcRoot, "self", "attr"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(resourceProcRoot, "self", "attr", "current"), []byte(label+"\n"), 0o644))
}

// attachDenied is the refusal the kernel returns for the cgroup.procs write, in
// the shape os.WriteFile reports it.
func attachDenied(dir string, errno syscall.Errno) error {
	return &os.PathError{Op: "open", Path: filepath.Join(dir, "cgroup.procs"), Err: errno}
}

func TestResourceAttachDeniedHintNamesTheConfiningPolicy(t *testing.T) {
	// Ownership and delegation are both correct here: the destination belongs to
	// this uid and the launch runs inside the delegated subtree. Nothing tclaude
	// can inspect explains the refusal, which is exactly what an LSM denial looks
	// like from userspace.
	delegation := fakeDerivedResourceCgroup(t,
		"/user.slice/user@1000.service/app.slice/agent-sandbox.service/"+resourceSupervisorCgroup,
		"cpu memory", "cpu memory")
	require.NoError(t, os.WriteFile(filepath.Join(delegation, "cgroup.procs"), nil, 0o644))
	dir := filepath.Join(delegation, "tclaude-confined")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.procs"), nil, 0o644))
	fakeSecurityLabel(t, "agent (enforce)")

	hint := resourceAttachDeniedHint(dir, attachDenied(dir, syscall.EACCES))
	assert.Contains(t, hint, `"agent (enforce)"`,
		"the profile that has to be widened is the one thing the operator needs named")
	assert.Contains(t, hint, "AppArmor")
	assert.Contains(t, hint, ".scope where the runtime now uses a .service",
		"a path rule left behind by a renamed delegation node is how this policy goes stale")
	assert.NotContains(t, hint, "Delegate=",
		"a correctly delegated subtree must not be diagnosed as an undelegated one")
}

func TestResourceAttachDeniedHintFallsBackWithoutASecurityLabel(t *testing.T) {
	delegation := fakeDerivedResourceCgroup(t,
		"/user.slice/user@1000.service/app.slice/agent-sandbox.service/"+resourceSupervisorCgroup,
		"cpu memory", "cpu memory")
	require.NoError(t, os.WriteFile(filepath.Join(delegation, "cgroup.procs"), nil, 0o644))
	dir := filepath.Join(delegation, "tclaude-unconfined")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.procs"), nil, 0o644))
	fakeSecurityLabel(t, "unconfined")

	hint := resourceAttachDeniedHint(dir, attachDenied(dir, syscall.EPERM))
	assert.Contains(t, hint, "above the ownership bits",
		"the refusal is still worth locating even when no policy can be named")
	assert.NotContains(t, hint, `"unconfined"`,
		"naming a label that mediates nothing sends the operator after the wrong policy")

	// A complain-mode profile logs what it would have refused and permits it, so
	// it cannot be the cause of a denial either.
	fakeSecurityLabel(t, "agent (complain)")
	complain := resourceAttachDeniedHint(dir, attachDenied(dir, syscall.EPERM))
	assert.NotContains(t, complain, "agent (complain)",
		"widening a profile that is not enforcing anything is not the fix for this denial")
	assert.Contains(t, complain, "above the ownership bits")
}

func TestResourceAttachDeniedHintDoesNotClaimUnreadableFactsAsCleared(t *testing.T) {
	// The policy that refuses the write refuses the stat behind it just as
	// readily, and a diagnosis that reports an unmade reading as a clean one
	// asserts the operator's actual cause has been ruled out.
	delegation := fakeDerivedResourceCgroup(t,
		"/user.slice/user@1000.service/app.slice/agent-sandbox.service/"+resourceSupervisorCgroup,
		"cpu memory", "cpu memory")
	dir := filepath.Join(delegation, "tclaude-unreadable")
	require.NoError(t, os.Mkdir(dir, 0o755))
	fakeSecurityLabel(t, "agent (enforce)")

	// Neither the destination nor the ancestor has a cgroup.procs to stat.
	hint := resourceAttachDeniedHint(dir, attachDenied(dir, syscall.EACCES))
	assert.Contains(t, hint, "cannot be narrowed down")
	assert.Contains(t, hint, "either cgroup v2's rule",
		"the containment rule is still a live candidate and has to stay named")
	assert.Contains(t, hint, `"agent (enforce)"`, "so is the policy, so both get offered")
	assert.NotContains(t, hint, "is satisfied",
		"a reading that could not be taken must never be reported as one that came back clean")
	assert.NotContains(t, hint, "may write")
}

func TestResourceAttachDeniedHintReportsABoundaryPreparedByAnotherIdentity(t *testing.T) {
	// The delegation is intact and only the boundary inside it is not writable,
	// which is what two identities preparing and joining one boundary produces.
	delegation := fakeDerivedResourceCgroup(t,
		"/system.slice/tclaude-agentd.service/"+resourceSupervisorCgroup, "cpu memory", "cpu memory")
	require.NoError(t, os.WriteFile(filepath.Join(delegation, "cgroup.procs"), nil, 0o644))
	dir := filepath.Join(delegation, "tclaude-foreign")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.procs"), nil, 0o644))
	readOnlyCgroupNode(t, filepath.Join(dir, "cgroup.procs"))

	hint := resourceAttachDeniedHint(dir, attachDenied(dir, syscall.EACCES))
	assert.Contains(t, hint, "created by a different identity")
	assert.Contains(t, hint, delegation)
	assert.NotContains(t, hint, "Delegate=cpu memory",
		"a delegated parent must not be reported as the node that needs delegating")
}

func TestResourceAttachDeniedHintNamesTheCommonAncestor(t *testing.T) {
	// A launch outside the delegated subtree is refused by cgroup v2's delegation
	// containment rule on the common ancestor, with a destination it owns.
	fakeDerivedResourceCgroup(t, "/user.slice/app.slice/other.service", "cpu memory", "cpu memory")
	ancestor := filepath.Join(resourceCgroupRoot, "user.slice", "app.slice")
	dir := filepath.Join(ancestor, "agent-sandbox.service", "tclaude-outside")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.procs"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ancestor, "cgroup.procs"), nil, 0o644))
	readOnlyCgroupNode(t, filepath.Join(ancestor, "cgroup.procs"))
	fakeSecurityLabel(t, "agent (enforce)")

	hint := resourceAttachDeniedHint(dir, attachDenied(dir, syscall.EACCES))
	assert.Contains(t, hint, filepath.Join(ancestor, "cgroup.procs"),
		"the node the kernel actually decided on is the one to name")
	assert.Contains(t, hint, "place it inside the delegation instead")
	assert.NotContains(t, hint, "AppArmor",
		"an established cause must win over the fallback that cannot establish one")
}

func TestResourceAttachDeniedHintReportsAnUndelegatedBoundary(t *testing.T) {
	delegation := fakeDerivedResourceCgroup(t,
		"/system.slice/tclaude-agentd.service/"+resourceSupervisorCgroup, "cpu memory", "cpu memory")
	dir := filepath.Join(delegation, "tclaude-undelegated")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.procs"), nil, 0o644))
	readOnlyCgroupNode(t, filepath.Join(dir, "cgroup.procs"))

	hint := resourceAttachDeniedHint(dir, attachDenied(dir, syscall.EACCES))
	assert.Contains(t, hint, "Delegate=cpu memory",
		"a boundary this uid cannot write is the delegation missing, not a policy above it")
	assert.NotContains(t, hint, "AppArmor")
}

func TestResourceAttachDeniedHintExplainsAReadOnlyHierarchy(t *testing.T) {
	delegation := fakeDerivedResourceCgroup(t,
		"/system.slice/tclaude-agentd.service/"+resourceSupervisorCgroup, "cpu memory", "cpu memory")
	dir := filepath.Join(delegation, "tclaude-readonly")
	require.NoError(t, os.Mkdir(dir, 0o755))

	hint := resourceAttachDeniedHint(dir, attachDenied(dir, syscall.EROFS))
	assert.Contains(t, hint, "read-only in this launch's mount namespace")
	assert.Contains(t, hint, "ProtectControlGroups=")

	assert.Empty(t, resourceAttachDeniedHint(dir, attachDenied(dir, syscall.EISDIR)),
		"a refusal with no delegation or policy reading behind it must not be diagnosed as one")
}

func TestResourceLimitExecAttachFailureCarriesTheDiagnosis(t *testing.T) {
	delegation := fakeDerivedResourceCgroup(t,
		"/system.slice/tclaude-agentd.service/"+resourceSupervisorCgroup, "cpu memory", "cpu memory")
	dir := filepath.Join(delegation, "tclaude-runtime-failure")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.procs"), nil, 0o644))
	readOnlyCgroupNode(t, filepath.Join(dir, "cgroup.procs"))

	err := runResourceLimitExec(dir, "session-attach-denied", "exit 0", false, false, false)
	require.ErrorContains(t, err, "attach workload")
	assert.ErrorContains(t, err, "Delegate=cpu memory",
		"the pane-side refusal is where an operator reads it, so the diagnosis has to reach the error")
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

	err := runResourceLimitExec(dir, "session-runtime-failure", "exit 0", true, false, false)
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

// paneScriptCapturingTmux snapshots the launch script at new-session time. The
// script and the cgroup are both cleaned up when a launch fails, so a test that
// wants to see what the pane was told has to look while tmux is being called.
type paneScriptCapturingTmux struct {
	*launchRecordingTmux
	script string
}

func (c *paneScriptCapturingTmux) Command(args ...string) *exec.Cmd {
	if recordedTmuxCommand(args) == "new-session" {
		// The interpreter is whatever the host resolved (TCL-1038), and it comes
		// with flags — `/bin/bash -p`, not a bare `sh`. Matching a literal word
		// would silently capture nothing and every assertion here would fail on
		// its NotEmpty precondition rather than on what it means to test, so
		// find the shell by the same predicate the launch path uses and take the
		// first non-flag word after it as the script.
		for i, arg := range args {
			if !clcommon.IsBootstrapShellWord(filepath.Base(arg)) {
				continue
			}
			for _, candidate := range args[i+1:] {
				if strings.HasPrefix(candidate, "-") {
					continue
				}
				if body, err := os.ReadFile(candidate); err == nil {
					c.script = string(body)
				}
				break
			}
			break
		}
	}
	return c.launchRecordingTmux.Command(args...)
}

// The operator asked for a boundary this host cannot create. A fresh launch is a
// live decision, so it must say so — reported as "it did not even try" when the
// launch instead succeeded quietly.
func TestRunNewResourceOnlyRefusesWhenTheHostHasNoDelegation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// A cgroup root with no controllers file at all is what an undelegated host
	// looks like to the derivation.
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	t.Setenv(ResourceDelegationDirEnv, "")
	t.Setenv("TMUX", "")
	prevCheck := ClaudeAncestorCheck
	ClaudeAncestorCheck = func() bool { return false }
	t.Cleanup(func() { ClaudeAncestorCheck = prevCheck })
	stubTmuxOnPath(t)
	rec := &launchRecordingTmux{}
	swapTmux(t, rec)

	err := runNew(&NewParams{
		Label:       "spwn-nodeleg",
		Dir:         t.TempDir(),
		Detached:    true,
		SandboxImpl: string(sandboxpolicy.ImplementationResourceOnly),
	})
	require.Error(t, err, "a boundary the host cannot create must not launch silently")
	assert.ErrorContains(t, err, "delegated cgroup v2",
		"the refusal must name what is missing, as it does for a ceiling")
	assert.Empty(t, rec.newSessions(), "the launch must be refused before tmux")
}

// The other half of the same policy: a reincarnated successor or no-copy clone
// forks `session new` with no -r and no operator control of its own, so the
// undelegated host must not make it unlaunchable.
func TestRunNewResourceOnlyContinuationDegradesWhenTheHostHasNoDelegation(t *testing.T) {
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
	logged := captureWarnings(t)
	rec := &paneScriptCapturingTmux{launchRecordingTmux: &launchRecordingTmux{failNewSession: true}}
	swapTmux(t, rec)

	err := runNew(&NewParams{
		Label:               "spwn-continuation",
		Dir:                 t.TempDir(),
		Detached:            true,
		SandboxImpl:         string(sandboxpolicy.ImplementationResourceOnly),
		SandboxContinuation: true,
	})
	require.Error(t, err, "the fake tmux refuses new-session")
	assert.NotContains(t, err.Error(), "delegated cgroup v2",
		"a continuation must reach its pane rather than being stranded")
	require.NotEmpty(t, rec.script, "the launch must still reach tmux")
	assert.NotContains(t, rec.script, "resource-limit-exec", "there is no cgroup to join")
	assert.Contains(t, logged.String(), "resource cgroup unavailable")
}

// The pane seam is where most launches get their cgroup, and resource-only
// expresses the request through the implementation alone — with no sandbox
// snapshot on a direct CLI launch or a CLI resume. A gate that reads only a
// snapshot silently skips the boundary here.
func TestRunNewResourceOnlyWithoutLimitsWrapsPaneInAccountingCgroup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	current := fakeCurrentResourceCgroup(t, "cpu memory", "")
	prevCheck := ClaudeAncestorCheck
	ClaudeAncestorCheck = func() bool { return false }
	t.Cleanup(func() { ClaudeAncestorCheck = prevCheck })
	stubTmuxOnPath(t)
	// The launch is refused at tmux, which is the cheapest way to observe the
	// fully assembled pane command without waiting on a pane that never answers.
	rec := &paneScriptCapturingTmux{launchRecordingTmux: &launchRecordingTmux{failNewSession: true}}
	swapTmux(t, rec)

	err := runNew(&NewParams{
		Label:       "spwn-acctcg",
		Dir:         t.TempDir(),
		Detached:    true,
		SandboxImpl: string(sandboxpolicy.ImplementationResourceOnly),
	})
	require.Error(t, err, "the fake tmux refuses new-session")

	require.NotEmpty(t, rec.script, "the launch never reached tmux with a pane script")
	assert.Contains(t, rec.script, "resource-limit-exec",
		"the pane must join the boundary its implementation names, snapshot or not")
	assert.Contains(t, rec.script, filepath.Join(current, "tclaude-"),
		"the wrapper must point at a cgroup under the delegated parent")
}

// fakeResourceCgroupKill swaps the cgroup.kill seam for one test and reports
// whether it ran. The provided behavior stands in for the kernel's reaction to
// the write: killing every member, which on the fake filesystem a test
// expresses by emptying the directory or flipping cgroup.events.
func fakeResourceCgroupKill(t *testing.T, behavior func(dir string) error) *bool {
	t.Helper()
	called := false
	previous := requestResourceCgroupKill
	requestResourceCgroupKill = func(dir string) error {
		called = true
		return behavior(dir)
	}
	t.Cleanup(func() { requestResourceCgroupKill = previous })
	return &called
}

func TestPrepareResourceCgroupReclaimsPopulatedSameSessionCgroup(t *testing.T) {
	fakeCurrentResourceCgroup(t, "cpu memory", "")
	limits := sandboxpolicy.ResourceLimits{Memory: "256MB"}
	dir, _, err := PrepareResourceCgroup("stray-session", limits)
	require.NoError(t, err)
	// A stray descendant of the session's previous life keeps the boundary
	// populated, which is what makes the real rmdir fail with EBUSY.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.events"),
		[]byte("populated 1\nfrozen 0\n"), 0o644))
	killed := fakeResourceCgroupKill(t, func(killDir string) error {
		require.Equal(t, dir, killDir)
		entries, readErr := os.ReadDir(killDir)
		require.NoError(t, readErr)
		for _, entry := range entries {
			require.NoError(t, os.RemoveAll(filepath.Join(killDir, entry.Name())))
		}
		return nil
	})

	again, cleanup, err := PrepareResourceCgroup("stray-session", limits)
	require.NoError(t, err, "a wake must reclaim its own session's stray processes")
	t.Cleanup(cleanup)
	assert.True(t, *killed, "reclaim must go through the kernel's cgroup.kill")
	assert.Equal(t, dir, again, "the boundary keeps its deterministic per-session path")
	assert.FileExists(t, filepath.Join(again, "memory.max"),
		"the recreated boundary carries the requested ceiling again")
}

func TestPrepareResourceCgroupFailsWhenStrayProcessesSurviveKill(t *testing.T) {
	fakeCurrentResourceCgroup(t, "cpu memory", "")
	limits := sandboxpolicy.ResourceLimits{Memory: "256MB"}
	dir, _, err := PrepareResourceCgroup("stuck-session", limits)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.events"),
		[]byte("populated 1\nfrozen 0\n"), 0o644))
	// The kill write is accepted but the members never die, as with a process
	// stuck in an uninterruptible state.
	fakeResourceCgroupKill(t, func(string) error { return nil })
	previousWait, previousPoll := resourceCgroupKillWait, resourceCgroupKillPoll
	resourceCgroupKillWait, resourceCgroupKillPoll = 30*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { resourceCgroupKillWait, resourceCgroupKillPoll = previousWait, previousPoll })

	_, _, err = PrepareResourceCgroup("stuck-session", limits)
	require.Error(t, err)
	assert.ErrorContains(t, err, "already exists and is active or not reclaimable")
	assert.ErrorContains(t, err, "processes remain after cgroup.kill")
}

func TestKillResourceCgroupMembersReapsWithoutRemovingTheBoundary(t *testing.T) {
	dir := t.TempDir()
	killed := fakeResourceCgroupKill(t, func(killDir string) error {
		return os.WriteFile(filepath.Join(killDir, "cgroup.events"),
			[]byte("populated 0\nfrozen 0\n"), 0o644)
	})
	// No cgroup.events at all: an already-removed or never-created boundary.
	require.NoError(t, KillResourceCgroupMembers(dir))
	require.NoError(t, KillResourceCgroupMembers(""))
	assert.False(t, *killed, "an empty boundary must not be killed")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.events"),
		[]byte("populated 1\nfrozen 0\n"), 0o644))
	require.NoError(t, KillResourceCgroupMembers(dir))
	assert.True(t, *killed)
	assert.DirExists(t, dir, "the durable boundary itself stays for the next relaunch")
}

func TestResourceLimitExecReapsSurvivingDescendantsBeforeRemovingTheBoundary(t *testing.T) {
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	dir := filepath.Join(resourceCgroupRoot, "tclaude-stray-descendant")
	require.NoError(t, os.Mkdir(dir, 0o755))
	// A double-forked descendant keeps the boundary populated after the
	// workload itself has exited.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.events"),
		[]byte("populated 1\nfrozen 0\n"), 0o644))
	killed := fakeResourceCgroupKill(t, func(killDir string) error {
		entries, readErr := os.ReadDir(killDir)
		require.NoError(t, readErr)
		for _, entry := range entries {
			require.NoError(t, os.RemoveAll(filepath.Join(killDir, entry.Name())))
		}
		return nil
	})

	require.NoError(t, runResourceLimitExec(dir, "session-stray-descendant", "exit 0", false, false, false))
	assert.True(t, *killed, "pane exit must reap what outlived the workload")
	assert.NoDirExists(t, dir, "the emptied boundary is removed with the pane")
}

func TestResourceLimitExecPreservedBoundaryReapsMembersButLeavesDirectory(t *testing.T) {
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	dir := filepath.Join(resourceCgroupRoot, "tclaude-managed-retry")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.events"),
		[]byte("populated 1\nfrozen 0\n"), 0o644))
	killed := fakeResourceCgroupKill(t, func(killDir string) error {
		for _, name := range []string{"cgroup.events", "cgroup.procs"} {
			if err := os.Remove(filepath.Join(killDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	})

	require.NoError(t, runResourceLimitExec(
		dir, "session-managed-retry", "exit 0", false, false, true))
	assert.True(t, *killed, "a failed managed-server attempt must reap its workload")
	assert.DirExists(t, dir, "agentd's durable boundary must remain for the next attempt")
	require.NoError(t, RemoveResourceCgroup(dir))
	assert.NoDirExists(t, dir, "final managed-server retirement removes the durable boundary")
}

func TestResourceLimitExecSharedBoundaryLeavesTheServerAndItsDirAlone(t *testing.T) {
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	dir := filepath.Join(resourceCgroupRoot, "tclaude-shared-boundary")
	require.NoError(t, os.Mkdir(dir, 0o755))
	// The managed server that owns this boundary is alive inside it while the
	// attach client comes and goes.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.events"),
		[]byte("populated 1\nfrozen 0\n"), 0o644))
	killed := fakeResourceCgroupKill(t, func(string) error { return nil })

	require.NoError(t, runResourceLimitExec(dir, "session-shared", "exit 0", false, true, false))
	assert.False(t, *killed, "an attach client's exit must not kill the server sharing its boundary")
	assert.DirExists(t, dir, "the server's boundary must survive the attach client")
}

func TestWrapPreparedResourceCgroupCommandMarksManagedBoundaryShared(t *testing.T) {
	managed := WrapPreparedResourceCgroupCommand("s", "/sys/fs/cgroup/x/tclaude-a", "cmd", false)
	assert.Contains(t, managed, "--preserve-boundary",
		"agentd owns a managed server boundary across launch retries")
	assert.NotContains(t, managed, "--shared-boundary",
		"the managed server wrapper still owns and reaps its workload")
	shared := wrapPreparedResourceCgroupCommand("s", "/sys/fs/cgroup/x/tclaude-a", "cmd", false, true, false)
	assert.Contains(t, shared, "--shared-boundary")
}
