package agentd

import (
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
	"os"
	"path/filepath"
	"testing"
)

func TestTerminalDirectoryListingBoundedAndNoFollow(t *testing.T) {
	path := testutil.CanonicalTempDir(t)
	require.NoError(t, os.Mkdir(filepath.Join(path, "nested"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(path, "nested", "visible.txt"), []byte("hello"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(path, "nested", ".env"), nil, 0600))
	require.NoError(t, os.Symlink("visible.txt", filepath.Join(path, "nested", "alias")))
	root, err := os.Open(path)
	require.NoError(t, err)
	defer root.Close()
	f, err := terminalDirectoryListing(root, path, "nested")
	require.NoError(t, err)
	var out struct {
		Entries []struct {
			Path string
			Kind string
			Size int64
		}
		Truncated bool
	}
	require.NoError(t, json.NewDecoder(f).Decode(&out))
	f.Close()
	require.False(t, out.Truncated)
	require.Len(t, out.Entries, 1)
	require.Equal(t, "nested/visible.txt", out.Entries[0].Path)
	require.Equal(t, int64(5), out.Entries[0].Size)
	require.NoError(t, os.Symlink("nested", filepath.Join(path, "redirect")))
	_, err = terminalDirectoryListing(root, path, "redirect")
	require.Error(t, err)
	_, err = terminalDirectoryListing(root, path, "../")
	require.Error(t, err)
	for i := 0; i < terminalDirectoryMaxEntries+5; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(path, fmt.Sprintf("file-%03d", i)), nil, 0600))
	}
	f, err = terminalDirectoryListing(root, path, ".")
	require.NoError(t, err)
	require.NoError(t, json.NewDecoder(f).Decode(&out))
	f.Close()
	require.True(t, out.Truncated)
	require.LessOrEqual(t, len(out.Entries), terminalDirectoryMaxEntries)
}
