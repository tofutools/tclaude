package host

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestExecutionFileReadConfinesPathsAndRejectsSpecialFiles(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	reader := DirectoryBrowser{}
	ctx := context.Background()
	require.NoError(t, os.WriteFile(filepath.Join(root, "report.txt"), []byte("report content"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "private.txt"), []byte("outside"), 0600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "private.txt"), filepath.Join(root, "escape")))
	require.NoError(t, os.Symlink("report.txt", filepath.Join(root, "inside")))
	for _, path := range []string{"report.txt", filepath.Join(root, "report.txt"), "inside"} {
		result, err := reader.ReadExecutionFile(ctx, ports.ExecutionFileReadRequest{Root: root, Path: path})
		require.NoError(t, err)
		require.Equal(t, "report content", string(result.Content))
	}
	require.NoError(t, syscall.Mkfifo(filepath.Join(root, "pipe"), 0600))
	huge, err := os.Create(filepath.Join(root, "huge"))
	require.NoError(t, err)
	require.NoError(t, huge.Truncate(32<<20+1))
	require.NoError(t, huge.Close())
	for _, path := range []string{"escape", filepath.Join(outside, "private.txt"), "../private.txt", "pipe", "huge", ".", "missing"} {
		_, err := reader.ReadExecutionFile(ctx, ports.ExecutionFileReadRequest{Root: root, Path: path})
		require.Error(t, err, path)
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "empty"), nil, 0600))
	empty, err := reader.ReadExecutionFile(ctx, ports.ExecutionFileReadRequest{Root: root, Path: "empty"})
	require.NoError(t, err)
	require.Empty(t, empty.Content)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = reader.ReadExecutionFile(cancelled, ports.ExecutionFileReadRequest{Root: root, Path: "report.txt"})
	require.ErrorIs(t, err, context.Canceled)
}
