//go:build linux || darwin

package host_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestSandboxPathInspectionIsReadOnlyAndRejectsPrivateAliases(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	public := filepath.Join(root, "public")
	require.NoError(t, os.Mkdir(private, 0700))
	require.NoError(t, os.Mkdir(public, 0700))
	file := filepath.Join(public, "input.txt")
	require.NoError(t, os.WriteFile(file, []byte("retained"), 0600))
	alias := filepath.Join(public, "alias")
	require.NoError(t, os.Symlink(private, alias))
	inspector, err := host.NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	missing := filepath.Join(public, "missing")
	rules := []model.SandboxFilesystemRule{
		{HostPath: file, Access: model.SandboxFilesystemRead, ExpectedKind: "file"},
		{HostPath: alias, Access: model.SandboxFilesystemWrite},
		{HostPath: root, Access: model.SandboxFilesystemRead},
		{HostPath: missing, Access: model.SandboxFilesystemWrite},
		{HostPath: public, Access: model.SandboxFilesystemRead, ExpectedKind: "file"},
		{HostPath: private, Access: model.SandboxFilesystemDeny},
		{HostPath: file, Access: model.SandboxFilesystemDeny},
		{HostPath: file, Access: model.SandboxFilesystemRead, GuestPath: private},
	}
	observations, err := inspector.InspectSandboxPaths(context.Background(), rules)
	require.NoError(t, err)
	require.Len(t, observations, len(rules))
	for index, want := range []string{"available", "refused", "refused", "missing", "refused", "available", "refused", "refused"} {
		require.Equal(t, want, observations[index].State, "index %d", index)
		require.Equal(t, index, observations[index].Index)
	}
	require.NoDirExists(t, missing)
	body, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, "retained", string(body))
	canonical, err := filepath.EvalSymlinks(file)
	require.NoError(t, err)
	require.Equal(t, canonical, observations[0].CanonicalPath)
	require.Equal(t, "file", observations[0].Kind)
}

func TestSandboxPathInspectionRequiresStableCompositionRoots(t *testing.T) {
	_, err := host.NewSandboxPathInspector(nil)
	require.Error(t, err)
	root := t.TempDir()
	private := filepath.Join(root, "private")
	require.NoError(t, os.Mkdir(private, 0700))
	inspector, err := host.NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	require.NoError(t, os.Rename(private, private+"-retained"))
	require.NoError(t, os.Mkdir(private, 0700))
	_, err = inspector.InspectSandboxPaths(context.Background(), nil)
	require.ErrorContains(t, err, "identity changed")
}
