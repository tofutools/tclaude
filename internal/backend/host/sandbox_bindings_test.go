//go:build linux || darwin

package host_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestSandboxBindingsRetainExactSourceAcrossPathReplacement(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	require.NoError(t, os.Mkdir(private, 0700))
	inspector, err := host.NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	source := filepath.Join(root, "input")
	require.NoError(t, os.WriteFile(source, []byte("approved input"), 0600))
	bound, err := inspector.BindSandboxMounts(context.Background(), []model.SandboxFilesystemRule{{HostPath: source, Access: model.SandboxFilesystemRead, ExpectedKind: "file"}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bound.Close() })
	pins := bound.Pins()
	reopened, err := inspector.ReopenSandboxMounts(context.Background(), pins)
	require.NoError(t, err)
	require.NoError(t, reopened.Close())

	// The existing descriptor must still feed the approved object to a real
	// native child; a terminal bootstrap must refuse to reopen the replacement.
	require.NoError(t, os.Rename(source, source+"-retained"))
	require.NoError(t, os.WriteFile(source, []byte("replacement"), 0600))
	_, err = inspector.ReopenSandboxMounts(context.Background(), pins)
	require.ErrorContains(t, err, "identity changed")
	var output bytes.Buffer
	process, err := host.StartProcess(host.ProcessSpec{Executable: "/bin/sh", Args: []string{"-c", "cat <&3"}, ExactEnvironment: true,
		Env: []string{"PATH=/usr/bin:/bin"}, Stdout: &output, ExtraFiles: bound.Files()})
	require.NoError(t, err)
	require.NoError(t, bound.Close())
	require.Eventually(t, func() bool {
		observation := process.Observe()
		return observation.Exited && observation.ExitCode != nil
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, "approved input", output.String())

	require.NoError(t, os.Remove(source))
	require.NoError(t, os.Mkdir(source, 0700))
	_, err = inspector.ReopenSandboxMounts(context.Background(), pins)
	require.Error(t, err, "a retained file grant must never widen to a directory")
}

func TestSandboxBindingsRefusePrivateAndMissingSourcesWithoutCreatingThem(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	require.NoError(t, os.Mkdir(private, 0700))
	inspector, err := host.NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(private, alias))
	missing := filepath.Join(root, "missing")
	for _, source := range []string{private, alias, root, missing} {
		_, err := inspector.BindSandboxMounts(context.Background(), []model.SandboxFilesystemRule{{HostPath: source, Access: model.SandboxFilesystemWrite}})
		require.Error(t, err, source)
	}
	require.NoDirExists(t, missing)

	public := filepath.Join(root, "public")
	require.NoError(t, os.Mkdir(public, 0700))
	bound, err := inspector.BindSandboxMounts(context.Background(), []model.SandboxFilesystemRule{{HostPath: public, Access: model.SandboxFilesystemWrite}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bound.Close() })
	require.NoError(t, os.Rename(private, private+"-retained"))
	require.NoError(t, os.Mkdir(private, 0700))
	_, err = inspector.ReopenSandboxMounts(context.Background(), bound.Pins())
	require.ErrorContains(t, err, "protected sandbox root identity changed")
}
