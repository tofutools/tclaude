//go:build linux

package host

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestSandboxResourceLimitsNativeBootstrap(t *testing.T) {
	root := os.Getenv("TCLAUDE_TEST_CGROUP_ROOT")
	if root == "" {
		t.Skip("requires an explicitly delegated disposable cgroup subtree")
	}
	wrapper, err := exec.LookPath("bwrap")
	require.NoError(t, err)
	private, workspace := t.TempDir(), t.TempDir()
	require.NoError(t, os.Chmod(private, 0700))
	inspector, err := NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	executable, err := os.Executable()
	require.NoError(t, err)
	planner, err := NewSandboxLaunchPreparer(SandboxLaunchConfig{Inspector: inspector, Wrapper: wrapper,
		Bootstrap: executable, Artifacts: private, ResourceDelegationDirectory: root})
	require.NoError(t, err)
	selected, policy := materializedLaunchPolicy(t, inspector, model.SandboxPolicy{
		FilesystemRoot: model.SandboxRootSeparate,
		Filesystem:     []model.SandboxFilesystemRule{{HostPath: workspace, Access: model.SandboxFilesystemWrite}},
		Resources:      model.SandboxResources{Memory: "512MiB", CPU: "1"},
		Network:        &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny},
	})
	artifact, err := planner.Prepare(context.Background(), selected, policy, ProcessSpec{
		Executable: "/bin/sh", Args: []string{"-c", "printf limited; exit 7"}, Directory: workspace,
		ExactEnvironment: true, Env: []string{"PATH=/usr/bin:/bin"},
	})
	require.NoError(t, err)
	input, _, err := readSandboxChild(artifact)
	require.NoError(t, err)
	require.NotNil(t, input.Resources)
	t.Cleanup(func() { require.NoError(t, input.Resources.settle()) })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var output bytes.Buffer
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestSandboxBootstrapHelper$")
	cmd.Env = []string{"TCLAUDE_BOOTSTRAP_ARTIFACT=" + artifact.Path, "TCLAUDE_BOOTSTRAP_DIGEST=" + artifact.Digest}
	cmd.Stdout, cmd.Stderr = &output, &output
	err = cmd.Run()
	require.NoError(t, ctx.Err(), output.String())
	var exited *exec.ExitError
	require.ErrorAs(t, err, &exited, output.String())
	require.Equal(t, 7, exited.ExitCode(), output.String())
	require.Equal(t, "limited", output.String())
	require.NoDirExists(t, input.Resources.Path)
	require.NoError(t, SettleSandboxChild(&artifact), "observed-exit cleanup is idempotent")
	aborted, err := planner.Prepare(context.Background(), selected, policy, ProcessSpec{
		Executable: "/bin/sh", Args: []string{"-c", "exit 0"}, Directory: workspace,
		ExactEnvironment: true, Env: []string{"PATH=/usr/bin:/bin"},
	})
	require.NoError(t, err)
	abortedInput, _, err := readSandboxChild(aborted)
	require.NoError(t, err)
	require.NoError(t, AbortSandboxChild(aborted))
	require.NoDirExists(t, abortedInput.Resources.Path)
	require.NoFileExists(t, aborted.Path)
}

func TestSandboxResourceLimitsRequireRealDelegation(t *testing.T) {
	empty, err := prepareSandboxCgroup("", model.SandboxResources{})
	require.NoError(t, err)
	require.Nil(t, empty)
	root := t.TempDir()
	_, err = prepareSandboxCgroup(root, model.SandboxResources{Memory: "512MiB"})
	require.ErrorContains(t, err, "cgroup v2 filesystem")
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
	_, err = prepareSandboxCgroup(root, model.SandboxResources{CPU: "0"})
	require.ErrorContains(t, err, "at least 0.01")
}

func TestSandboxResourceLimitsNativeEnforcement(t *testing.T) {
	root := os.Getenv("TCLAUDE_TEST_CGROUP_ROOT")
	if root == "" {
		t.Skip("requires an explicitly delegated disposable cgroup subtree")
	}
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	boundary, err := prepareSandboxCgroup(root, model.SandboxResources{Memory: "64MiB", CPU: ".125"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, boundary.settle()) })
	file, err := boundary.open()
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	// v1 limits resident memory via memory.max, leaving swap policy to the
	// delegation. Disable swap in this disposable fixture so excess resident
	// demand produces a deterministic OOM rather than swapping on the runner.
	require.NoError(t, writeCgroupFile(file, "memory.swap.max", "0"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Exercise the same kernel placement used by the bootstrap: CPU work must
	// throttle, then touching memory beyond the ceiling must be killed by OOM.
	cmd := exec.CommandContext(ctx, python, "-c", "import time\nend=time.monotonic()+2\nwhile time.monotonic()<end: pass\nx=bytearray(128*1024*1024)\nfor i in range(0,len(x),4096): x[i]=1\n")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(file.Fd())}
	err = cmd.Run()
	require.Error(t, err)
	require.NoError(t, ctx.Err())
	var exited *exec.ExitError
	require.ErrorAs(t, err, &exited)
	status, ok := exited.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	require.True(t, status.Signaled())
	require.Equal(t, syscall.SIGKILL, status.Signal())
	for _, check := range []struct{ file, counter string }{{"cpu.stat", "nr_throttled"}, {"memory.events", "oom_kill"}} {
		raw, err := readCgroupFile(file, check.file)
		require.NoError(t, err)
		found := false
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == check.counter {
				require.NotEqual(t, "0", fields[1], string(raw))
				found = true
			}
		}
		require.True(t, found, string(raw))
	}
}

func TestSandboxResourceLimitsNative(t *testing.T) {
	root := os.Getenv("TCLAUDE_TEST_CGROUP_ROOT")
	if root == "" {
		t.Skip("requires an explicitly delegated disposable cgroup subtree")
	}
	boundary, err := prepareSandboxCgroup(root, model.SandboxResources{Memory: "64MiB", CPU: ".125"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, boundary.kill()); require.NoError(t, boundary.remove()) })
	require.NoError(t, boundary.verify())
	raw, err := os.ReadFile(filepath.Join(boundary.Path, "memory.max"))
	require.NoError(t, err)
	require.Equal(t, "67108864\n", string(raw))
	raw, err = os.ReadFile(filepath.Join(boundary.Path, "cpu.max"))
	require.NoError(t, err)
	require.Equal(t, "12500 100000\n", string(raw))
	require.NoError(t, os.WriteFile(filepath.Join(boundary.Path, "memory.max"), []byte("max"), 0644))
	require.ErrorContains(t, boundary.verify(), "memory.max changed")
}
