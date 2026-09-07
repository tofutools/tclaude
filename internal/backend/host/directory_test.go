package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestDirectoryBrowserListsOnlyBoundedDirectories(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta", ".hidden"} {
		require.NoError(t, os.Mkdir(filepath.Join(root, name), 0700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "not-a-directory"), []byte("private file contents"), 0600))
	require.NoError(t, os.Symlink(filepath.Join(root, "alpha"), filepath.Join(root, "link")))
	browser := DirectoryBrowser{}
	req := ports.DirectoryReadRequest{Path: root, Limit: 1}
	first, err := browser.ReadDirectory(context.Background(), req)
	require.NoError(t, err)
	canonical, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.Equal(t, canonical, first.Path)
	require.Equal(t, "alpha", first.Directories[0].Name)
	require.Equal(t, "alpha", first.NextAfter)
	req.After = first.NextAfter
	req.Limit = 200
	rest, err := browser.ReadDirectory(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, rest.Directories, 2)
	require.Equal(t, "beta", rest.Directories[0].Name)
	require.Equal(t, "link", rest.Directories[1].Name)
	require.Empty(t, rest.NextAfter)
	req.After = ""
	req.IncludeHidden = true
	all, err := browser.ReadDirectory(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, all.Directories, 4)
	req.Path = filepath.Join(root, "link")
	linked, err := browser.ReadDirectory(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(canonical, "alpha"), linked.Path)
	req.Path = "relative"
	_, err = browser.ReadDirectory(context.Background(), req)
	require.Error(t, err)
	contents, err := os.ReadFile(filepath.Join(root, "not-a-directory"))
	require.NoError(t, err)
	require.Equal(t, "private file contents", string(contents))
}
