//go:build linux

package host

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestSandboxDescriptorArgumentsAreSealedAndNotInWrapperEnvironment(t *testing.T) {
	wrapped, args, err := sandboxDescriptorInvocation("/usr/bin/bwrap", ProcessSpec{Executable: "/bin/sh", Directory: "/work", ExactEnvironment: true,
		Env: []string{"LITERAL=private literal $HOME"}}, &SandboxMountBindings{}, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = args.Close() })
	require.Empty(t, wrapped.Env)
	require.NotContains(t, strings.Join(wrapped.Args, " "), "private literal")
	_, err = args.WriteAt([]byte("replacement"), 0)
	require.Error(t, err, "the kernel must reject changing prepared launch arguments")
}

func TestSandboxDescriptorNativeConfinement(t *testing.T) {
	wrapper, err := exec.LookPath("bwrap")
	if err != nil {
		if os.Getenv("TCLAUDE_REQUIRE_SANDBOX_NATIVE") == "1" {
			t.Fatal(err)
		}
		t.Skip("bubblewrap is unavailable")
	}
	root := t.TempDir()
	private, workspace := filepath.Join(root, "private"), filepath.Join(root, "work")
	require.NoError(t, os.Mkdir(private, 0700))
	require.NoError(t, os.Mkdir(workspace, 0700))
	secret := filepath.Join(private, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("private backend fixture"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "input"), []byte("approved"), 0600))
	inspector, err := NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	var rules []model.SandboxFilesystemRule
	for _, path := range []string{"/usr", "/bin", "/lib", "/lib64"} {
		if _, err := os.Stat(path); err == nil {
			rules = append(rules, model.SandboxFilesystemRule{HostPath: path, GuestPath: path, Access: model.SandboxFilesystemRead})
		}
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	rules = append(rules,
		model.SandboxFilesystemRule{HostPath: executable, GuestPath: "/sandbox-test", Access: model.SandboxFilesystemRead, ExpectedKind: "file"},
		model.SandboxFilesystemRule{HostPath: workspace, GuestPath: "/work", Access: model.SandboxFilesystemWrite, ExpectedKind: "directory"},
		model.SandboxFilesystemRule{HostPath: filepath.Join(workspace, "input"), GuestPath: "/work/input", Access: model.SandboxFilesystemRead, ExpectedKind: "file"})
	bound, err := inspector.BindSandboxMounts(context.Background(), rules)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bound.Close() })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	var output bytes.Buffer
	artifact, err := inspector.PrepareSandboxChild(private, wrapper, ProcessSpec{Executable: "/sandbox-test", Args: []string{"-test.run=^TestSandboxDescriptorChild$"}, Directory: "/work", ExactEnvironment: true,
		Env: []string{"PATH=/usr/bin:/bin", "TCLAUDE_SANDBOX_CHILD=1", "SECRET=" + secret, "OUTSIDE_LISTENER=" + listener.Addr().String(), "LITERAL=$HOME stays literal"}, Stdout: &output, Stderr: &output}, bound, true)
	require.NoError(t, err)
	require.NoError(t, bound.Close())
	bootstrap := ProcessSpec{Executable: executable, Args: []string{"-test.run=^TestSandboxBootstrapHelper$"}, ExactEnvironment: true,
		Env: []string{"TCLAUDE_BOOTSTRAP_ARTIFACT=" + artifact.Path, "TCLAUDE_BOOTSTRAP_DIGEST=" + artifact.Digest}, Stdout: &output, Stderr: &output}
	process, err := StartProcess(bootstrap)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return process.Observe().Exited }, 10*time.Second, 10*time.Millisecond)
	observation := process.Observe()
	if observation.ExitCode != nil && *observation.ExitCode != 0 && os.Getenv("TCLAUDE_REQUIRE_SANDBOX_NATIVE") != "1" {
		for _, unavailable := range []string{"No permissions to create a new namespace", "Creating new namespace failed: Operation not permitted", "loopback: Failed RTM_NEWADDR: Operation not permitted"} {
			if strings.Contains(output.String(), unavailable) {
				t.Skipf("host forbids disposable namespace creation: %s", output.String())
			}
		}
	}
	require.NotNil(t, observation.ExitCode)
	require.Zero(t, *observation.ExitCode, output.String())
	written, err := os.ReadFile(filepath.Join(workspace, "output"))
	require.NoError(t, err)
	require.Equal(t, "$HOME stays literal", string(written))
	input, err := os.ReadFile(filepath.Join(workspace, "input"))
	require.NoError(t, err)
	require.Equal(t, "approved", string(input))
	output.Reset()
	repeated, err := StartProcess(bootstrap)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return repeated.Observe().Exited }, 5*time.Second, 10*time.Millisecond)
	require.Contains(t, output.String(), "already attempted")
}

func TestSandboxDescriptorChild(t *testing.T) {
	if os.Getenv("TCLAUDE_SANDBOX_CHILD") != "1" {
		t.Skip("invoked only within the disposable confinement test")
	}
	input, err := os.ReadFile("/work/input")
	require.NoError(t, err)
	require.Equal(t, "approved", string(input))
	require.Error(t, os.WriteFile("/work/input", []byte("changed"), 0600))
	require.NoError(t, os.WriteFile("/work/output", []byte(os.Getenv("LITERAL")), 0600))
	_, err = os.ReadFile(os.Getenv("SECRET"))
	require.Error(t, err)
	connection, err := net.DialTimeout("tcp", os.Getenv("OUTSIDE_LISTENER"), 200*time.Millisecond)
	if connection != nil {
		_ = connection.Close()
	}
	require.Error(t, err, "private namespace must not reach the host loopback listener")
	command := exec.Command("/bin/sh", "-c", `test ! -e "$SECRET" && test "$(cat /work/input)" = approved && ! printf changed >> /work/input`)
	require.NoError(t, command.Run(), "descendants must retain the filesystem boundary")
}
