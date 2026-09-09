package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestDirectoryProofRequiresCallerMarkerAndPhysicalDirectory(t *testing.T) {
	ctx := context.Background()
	p := DirectoryProof{}
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Mkdir(actual, 0700))
	require.NoError(t, os.Symlink(actual, alias))
	dirs, err := p.ResolveProofDirectories(ctx, []string{alias, actual})
	require.NoError(t, err)
	physical, err := filepath.EvalSymlinks(actual)
	require.NoError(t, err)
	require.Equal(t, []string{physical}, dirs)
	token := "0123456789abcdef0123456789abcdef"
	marker := filepath.Join(physical, ports.DirectoryWriteProofPrefix+token)
	require.Error(t, p.VerifyProofMarkers(ctx, dirs, token))
	_, err = os.Lstat(marker)
	require.True(t, os.IsNotExist(err), "verification must not manufacture caller evidence")
	other := filepath.Join(root, "unrelated")
	require.NoError(t, os.WriteFile(other, nil, 0600))
	require.NoError(t, os.Symlink(other, marker))
	require.Error(t, p.VerifyProofMarkers(ctx, dirs, token))
	require.NoError(t, os.Remove(marker))
	require.NoError(t, os.WriteFile(marker, nil, 0600))
	require.NoError(t, p.VerifyProofMarkers(ctx, dirs, token))
	require.NoError(t, p.ReassertProofDirectories(ctx, dirs))
	for _, invalid := range []string{"", "../unrelated", "0123456789ABCDEF0123456789ABCDEF"} {
		require.Error(t, p.RemoveProofMarkers(ctx, dirs, invalid))
	}
	require.FileExists(t, other)
	require.NoError(t, p.RemoveProofMarkers(ctx, dirs, token))
	require.NoError(t, p.RemoveProofMarkers(ctx, dirs, token))
	require.NoFileExists(t, marker)
	require.FileExists(t, other)

	// A directory replaced after verification must not redirect launch or cleanup.
	require.NoError(t, os.Rename(actual, actual+"-moved"))
	require.NoError(t, os.Symlink(actual+"-moved", actual))
	require.Error(t, p.ReassertProofDirectories(ctx, dirs))
	require.Error(t, p.VerifyProofMarkers(ctx, dirs, token))
	require.Error(t, p.RemoveProofMarkers(ctx, dirs, token))
}

func TestDirectoryProofRejectsUnresolvedOrNonDirectoryPaths(t *testing.T) {
	p := DirectoryProof{}
	root := t.TempDir()
	file := filepath.Join(root, "file")
	require.NoError(t, os.WriteFile(file, nil, 0600))
	for _, path := range []string{"", "relative", filepath.Join(root, "missing"), file} {
		_, err := p.ResolveProofDirectories(context.Background(), []string{path})
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.ResolveProofDirectories(ctx, []string{root})
	require.ErrorIs(t, err, context.Canceled)
}
