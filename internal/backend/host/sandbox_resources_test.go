//go:build linux || darwin

package host

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestSandboxDescriptorProviderResourcesStayPrivateAndRetainIdentity(t *testing.T) {
	root, err := os.MkdirTemp("", "sb-resource-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	private := filepath.Join(root, "private")
	require.NoError(t, os.Mkdir(private, 0700))
	resource := filepath.Join(private, "credential")
	require.NoError(t, os.WriteFile(resource, []byte("fixture credential"), 0600))
	inspector, err := NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	_, err = inspector.BindSandboxMounts(context.Background(), []model.SandboxFilesystemRule{{HostPath: resource, Access: model.SandboxFilesystemRead}})
	require.Error(t, err, "public policy cannot select private resources")
	for _, path := range []string{private, root} {
		_, err = inspector.BindSandboxProviderResources(context.Background(), []SandboxProviderResource{{Path: path, Access: model.SandboxFilesystemRead}})
		require.Error(t, err)
	}
	selected, materialized := materializedLaunchPolicy(t, inspector, model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate})
	planner, err := NewSandboxLaunchPreparer(SandboxLaunchConfig{Inspector: inspector, Wrapper: "/bin/sh", Bootstrap: "/bin/sh", Artifacts: private})
	require.NoError(t, err)
	artifact, err := planner.Prepare(context.Background(), selected, materialized, ProcessSpec{Executable: "/bin/sh", Directory: "/", ExactEnvironment: true}, SandboxProviderResource{Path: resource, Access: model.SandboxFilesystemRead})
	require.NoError(t, err)
	require.NoError(t, VerifySandboxChild(context.Background(), artifact))
	input, _, err := readSandboxChild(artifact)
	require.NoError(t, err)
	require.Len(t, input.ProviderResources, 1)
	require.NoError(t, os.Rename(resource, resource+".old"))
	require.NoError(t, os.WriteFile(resource, []byte("replacement"), 0600))
	require.ErrorContains(t, VerifySandboxChild(context.Background(), artifact), "identity changed")
	require.NoFileExists(t, artifact.Path+".started")
	socketPath := filepath.Join(private, "socket")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	socketArtifact, err := planner.Prepare(context.Background(), selected, materialized, ProcessSpec{Executable: "/bin/sh", Directory: "/", ExactEnvironment: true}, SandboxProviderResource{Path: socketPath, Access: model.SandboxFilesystemRead})
	require.NoError(t, err)
	require.NoError(t, VerifySandboxChild(context.Background(), socketArtifact))
	// Keep the original vnode alive under another name so replacement cannot
	// reuse its inode. The retained endpoint must never become the new listener.
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, os.Rename(socketPath, socketPath+".old"))
	replacement, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = replacement.Close() })
	require.ErrorContains(t, VerifySandboxChild(context.Background(), socketArtifact), "identity changed")
}

// This runs in the required native CI slice alongside the original filesystem
// and network journey. Only this fixture's exact credential, spool and socket
// cross the private-state floor; sibling backend resources must stay private.
func TestSandboxDescriptorProviderResourcesNative(t *testing.T) {
	wrapperName := "bwrap"
	if runtime.GOOS == "darwin" {
		wrapperName = "sandbox-exec"
	}
	wrapper, err := exec.LookPath(wrapperName)
	if err != nil {
		if os.Getenv("TCLAUDE_REQUIRE_SANDBOX_NATIVE") == "1" {
			t.Fatal(err)
		}
		t.Skip("native wrapper unavailable")
	}
	root, err := os.MkdirTemp("", "sb-provider-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	private := filepath.Join(root, "private")
	require.NoError(t, os.Mkdir(private, 0700))
	credential, secret, spool := filepath.Join(private, "credential"), filepath.Join(private, "secret"), filepath.Join(private, "spool")
	require.NoError(t, os.WriteFile(credential, []byte("fixture credential"), 0600))
	require.NoError(t, os.WriteFile(secret, []byte("private sibling"), 0600))
	require.NoError(t, os.Mkdir(spool, 0700))
	socket := filepath.Join(private, "s")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	forbiddenSocket := filepath.Join(private, "f")
	forbidden, err := net.Listen("unix", forbiddenSocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = forbidden.Close() })
	inspector, err := NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	executable, err := os.Executable()
	require.NoError(t, err)
	selected, materialized := materializedLaunchPolicy(t, inspector, model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate, Filesystem: []model.SandboxFilesystemRule{{HostPath: executable, Access: model.SandboxFilesystemRead}}, Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny}})
	planner, err := NewSandboxLaunchPreparer(SandboxLaunchConfig{Inspector: inspector, Wrapper: wrapper, Bootstrap: executable, Artifacts: private})
	require.NoError(t, err)
	artifact, err := planner.Prepare(context.Background(), selected, materialized, ProcessSpec{Executable: executable, Args: []string{"-test.run=^TestSandboxDescriptorProviderResourcesChild$"}, Directory: "/", ExactEnvironment: true, Env: []string{"PATH=/usr/bin:/bin", "TCLAUDE_PROVIDER_RESOURCE_CHILD=1", "CREDENTIAL=" + credential, "SECRET=" + secret, "SPOOL=" + spool, "SOCKET=" + socket, "FORBIDDEN_SOCKET=" + forbiddenSocket}},
		SandboxProviderResource{Path: credential, Access: model.SandboxFilesystemRead}, SandboxProviderResource{Path: spool, Access: model.SandboxFilesystemWrite}, SandboxProviderResource{Path: socket, Access: model.SandboxFilesystemRead})
	require.NoError(t, err)
	require.NoError(t, VerifySandboxChild(context.Background(), artifact))
	var output bytes.Buffer
	process, err := StartProcess(ProcessSpec{Executable: executable, Args: []string{"-test.run=^TestSandboxBootstrapHelper$"}, ExactEnvironment: true, Env: []string{"TCLAUDE_BOOTSTRAP_ARTIFACT=" + artifact.Path, "TCLAUDE_BOOTSTRAP_DIGEST=" + artifact.Digest}, Stdout: &output, Stderr: &output})
	require.NoError(t, err)
	require.Eventually(t, func() bool { o := process.Observe(); return o.Exited && o.ExitCode != nil }, 10*time.Second, 10*time.Millisecond)
	observation := process.Observe()
	if *observation.ExitCode != 0 && os.Getenv("TCLAUDE_REQUIRE_SANDBOX_NATIVE") != "1" {
		if sandboxNamespaceUnavailable(output.String()) {
			t.Skipf("host forbids disposable native namespace: %s", output.String())
		}
	}
	require.Zero(t, *observation.ExitCode, output.String())
	require.FileExists(t, filepath.Join(spool, "observed"))
	unchanged, err := os.ReadFile(credential)
	require.NoError(t, err)
	require.Equal(t, "fixture credential", string(unchanged))
}

func TestSandboxDescriptorProviderResourcesChild(t *testing.T) {
	if os.Getenv("TCLAUDE_PROVIDER_RESOURCE_CHILD") != "1" {
		t.Skip("native child only")
	}
	credential, err := os.ReadFile(os.Getenv("CREDENTIAL"))
	require.NoError(t, err)
	require.Equal(t, "fixture credential", string(credential))
	require.Error(t, os.WriteFile(os.Getenv("CREDENTIAL"), []byte("overwrite"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("SPOOL"), "observed"), []byte("event"), 0600))
	_, err = os.ReadFile(os.Getenv("SECRET"))
	require.Error(t, err)
	socket, err := net.DialTimeout("unix", os.Getenv("SOCKET"), time.Second)
	require.NoError(t, err)
	require.NoError(t, socket.Close())
	forbidden, err := net.DialTimeout("unix", os.Getenv("FORBIDDEN_SOCKET"), time.Second)
	if forbidden != nil {
		_ = forbidden.Close()
	}
	require.Error(t, err)
	command := exec.Command("/bin/sh", "-c", `test "$(cat "$CREDENTIAL")" = 'fixture credential' && ! cat "$SECRET" && printf event > "$SPOOL/descendant"`)
	require.NoError(t, command.Run(), "descendants retain narrow provider resource access")
}

func TestSandboxDescriptorControlDirectoryContainsOnlyReplaceableEndpoint(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "control-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	control := filepath.Join(root, "api")
	require.NoError(t, os.Mkdir(control, 0700))
	socket := filepath.Join(control, "socket")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	resource, err := SandboxControlResource(socket, control)
	require.NoError(t, err)
	require.Equal(t, SandboxProviderResource{Path: control, Access: model.SandboxFilesystemRead}, resource)
	_, err = SandboxControlResource(socket, root)
	require.Error(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(control, "credential"), []byte("private"), 0600))
	_, err = SandboxControlResource(socket, control)
	require.ErrorContains(t, err, "unrelated state")
	exact, err := SandboxControlResource(socket, "")
	require.NoError(t, err)
	require.Equal(t, socket, exact.Path)
}
