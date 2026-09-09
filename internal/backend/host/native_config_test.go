package host

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeConfigEditPreservesLinkModeAndConcurrentBytes(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "config")
	link := filepath.Join(root, "link")
	require.NoError(t, os.WriteFile(target, []byte("original"), 0400))
	require.NoError(t, os.Symlink(target, link))
	calls := 0
	plan := func(data []byte) (bool, []byte, error) { return true, append(data, []byte("+trust")...), nil }
	prepare := func(path string, data []byte, mode os.FileMode) (*atomicFileReplacement, error) {
		staged, err := prepareAtomicWriteFile(path, data, mode)
		if calls == 0 {
			require.NoError(t, os.Chmod(target, 0600))
			require.NoError(t, os.WriteFile(target, []byte("native update"), 0600))
			require.NoError(t, os.Chmod(target, 0400))
		}
		calls++
		return staged, err
	}
	require.NoError(t, editHarnessConfigFile("fixture", link, 0600, plan, prepare))
	require.Equal(t, 2, calls)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "native update+trust", string(data))
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0400), info.Mode().Perm())
	info, err = os.Lstat(link)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
	require.NoError(t, EditNativeConfigFile("fixture", link, 0600, func(current []byte) (bool, []byte, error) { return !bytes.Equal(current, data), data, nil }))
	temps, err := filepath.Glob(filepath.Join(root, ".config-*.tmp"))
	require.NoError(t, err)
	require.Empty(t, temps)
}
