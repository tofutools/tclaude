//go:build darwin

package host

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestSandboxDescriptorNativeConfinement(t *testing.T) {
	wrapper, err := exec.LookPath("sandbox-exec")
	if err != nil {
		if os.Getenv("TCLAUDE_REQUIRE_SANDBOX_NATIVE") == "1" {
			t.Fatal(err)
		}
		t.Skip("sandbox-exec is unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "sb-native-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	private, workspace := filepath.Join(root, "private"), filepath.Join(root, "work")
	require.NoError(t, os.Mkdir(private, 0700))
	require.NoError(t, os.Mkdir(workspace, 0700))
	secret := filepath.Join(private, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("private backend fixture"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "input"), []byte("approved"), 0600))
	inspector, err := NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	rules, err := SandboxRuntimeRules()
	require.NoError(t, err)
	executable, err := os.Executable()
	require.NoError(t, err)
	rules = append(rules,
		model.SandboxFilesystemRule{HostPath: executable, Access: model.SandboxFilesystemRead, ExpectedKind: "file"},
		model.SandboxFilesystemRule{HostPath: workspace, Access: model.SandboxFilesystemWrite, ExpectedKind: "directory"},
		model.SandboxFilesystemRule{HostPath: filepath.Join(workspace, "input"), Access: model.SandboxFilesystemRead, ExpectedKind: "file"})
	bound, err := inspector.BindSandboxMounts(context.Background(), rules)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bound.Close() })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	unixListener, err := net.Listen("unix", filepath.Join(private, "socket"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = unixListener.Close() })
	var output bytes.Buffer
	child := ProcessSpec{Executable: executable, Args: []string{"-test.run=^TestSandboxDescriptorChild$"}, Directory: workspace, ExactEnvironment: true,
		Env: []string{"PATH=/usr/bin:/bin", "TCLAUDE_SANDBOX_CHILD=1", "WORK=" + workspace, "SECRET=" + secret, "PRIVATE_SOCKET=" + unixListener.Addr().String(), "OUTSIDE_LISTENER=" + listener.Addr().String(), "LITERAL=$HOME stays literal"}, Stdout: &output, Stderr: &output}
	// Exercise the platform wrapper independently as well as through the
	// retained bootstrap, so either native boundary has its own evidence.
	t.Run("direct-wrapper", func(t *testing.T) {
		wrapped, _, err := sandboxDescriptorInvocation(wrapper, child, bound, true)
		require.NoError(t, err)
		process, err := StartProcess(wrapped)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			observation := process.Observe()
			return observation.Exited && observation.ExitCode != nil
		}, 10*time.Second, 10*time.Millisecond)
		require.Zero(t, *process.Observe().ExitCode, "%s: %s", process.cmd.ProcessState, output.String())
	})
	output.Reset()
	artifact, err := prepareNativeLaunchPolicy(t, inspector, private, wrapper, child, bound)
	require.NoError(t, err)
	require.NoError(t, bound.Close())
	bootstrap := ProcessSpec{Executable: executable, Args: []string{"-test.run=^TestSandboxBootstrapHelper$"}, ExactEnvironment: true,
		Env: []string{"TCLAUDE_BOOTSTRAP_ARTIFACT=" + artifact.Path, "TCLAUDE_BOOTSTRAP_DIGEST=" + artifact.Digest}, Stdout: &output, Stderr: &output}
	process, err := StartProcess(bootstrap)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		observation := process.Observe()
		return observation.Exited && observation.ExitCode != nil
	}, 10*time.Second, 10*time.Millisecond)
	observation := process.Observe()
	require.NotNil(t, observation.ExitCode, output.String())
	require.Zero(t, *observation.ExitCode, "%s: %s", process.cmd.ProcessState, output.String())
	written, err := os.ReadFile(filepath.Join(workspace, "output"))
	require.NoError(t, err)
	require.Equal(t, "$HOME stays literal", string(written))
	input, err := os.ReadFile(filepath.Join(workspace, "input"))
	require.NoError(t, err)
	require.Equal(t, "approved", string(input))
	output.Reset()
	repeated, err := StartProcess(bootstrap)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		observation := repeated.Observe()
		return observation.Exited && observation.ExitCode != nil
	}, 5*time.Second, 10*time.Millisecond)
	require.Contains(t, output.String(), "already attempted")
}

func TestSandboxDescriptorChild(t *testing.T) {
	if os.Getenv("TCLAUDE_SANDBOX_CHILD") != "1" {
		t.Skip("invoked only within the disposable confinement test")
	}
	workspace := os.Getenv("WORK")
	input, err := os.ReadFile(filepath.Join(workspace, "input"))
	require.NoError(t, err)
	require.Equal(t, "approved", string(input))
	require.Error(t, os.WriteFile(filepath.Join(workspace, "input"), []byte("changed"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "output"), []byte(os.Getenv("LITERAL")), 0600))
	_, err = os.ReadFile(os.Getenv("SECRET"))
	require.Error(t, err)
	_, err = os.ReadDir(filepath.Dir(os.Getenv("SECRET")))
	require.Error(t, err, "runtime root access must not expose the private directory")
	for network, address := range map[string]string{"tcp": os.Getenv("OUTSIDE_LISTENER"), "unix": os.Getenv("PRIVATE_SOCKET")} {
		connection, err := net.DialTimeout(network, address, 200*time.Millisecond)
		if connection != nil {
			_ = connection.Close()
		}
		require.Error(t, err, network)
	}
	command := exec.Command("/bin/sh", "-c", `test ! -e "$SECRET" && test "$(cat "$WORK/input")" = approved && ! printf changed >> "$WORK/input"`)
	require.NoError(t, command.Run(), "descendants must retain the filesystem boundary")
}
