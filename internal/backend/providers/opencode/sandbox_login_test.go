//go:build linux || darwin

package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestSandboxLoginCopiesExactCredentialsWithoutSharingRefresh(t *testing.T) {
	source, state := t.TempDir(), t.TempDir()
	require.NoError(t, prepareSandboxStateDirectories(state))
	for _, name := range []string{"auth.json", "mcp-auth.json"} {
		original := []byte("{\n  \"fixture\": \"" + name + "\"\n}\n")
		src, dst := filepath.Join(source, name), filepath.Join(state, "data", "opencode", name)
		require.NoError(t, os.WriteFile(src, original, 0600))
		require.NoError(t, seedSandboxLogin(source, state))
		copied, err := os.ReadFile(dst)
		require.NoError(t, err)
		require.Equal(t, original, copied)
		info, err := os.Stat(dst)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
		sourceInfo, err := os.Stat(src)
		require.NoError(t, err)
		require.False(t, os.SameFile(sourceInfo, info))
		require.NoError(t, os.WriteFile(dst, []byte("independent refresh"), 0600))
		require.NoError(t, seedSandboxLogin(source, state))
		copied, err = os.ReadFile(dst)
		require.NoError(t, err)
		require.Equal(t, "independent refresh", string(copied))
		untouched, err := os.ReadFile(src)
		require.NoError(t, err)
		require.Equal(t, original, untouched)
	}
	require.NoError(t, os.WriteFile(filepath.Join(source, "unrelated-history.json"), []byte("not copied"), 0600))
	require.NoError(t, seedSandboxLogin(source, state))
	require.NoFileExists(t, filepath.Join(state, "data", "opencode", "unrelated-history.json"))
}

func TestSandboxLoginRefusesLinksAndSpecialFiles(t *testing.T) {
	for _, kind := range []string{"source link", "source fifo", "destination link"} {
		t.Run(kind, func(t *testing.T) {
			source, state := t.TempDir(), t.TempDir()
			require.NoError(t, prepareSandboxStateDirectories(state))
			outside := filepath.Join(t.TempDir(), "unrelated")
			require.NoError(t, os.WriteFile(outside, []byte("untouched"), 0600))
			src, dst := filepath.Join(source, "auth.json"), filepath.Join(state, "data", "opencode", "auth.json")
			switch kind {
			case "source link":
				require.NoError(t, os.Symlink(outside, src))
			case "source fifo":
				require.NoError(t, unix.Mkfifo(src, 0600))
			case "destination link":
				require.NoError(t, os.WriteFile(src, []byte("login"), 0600))
				require.NoError(t, os.Symlink(outside, dst))
			}
			require.Error(t, seedSandboxLogin(source, state))
			untouched, err := os.ReadFile(outside)
			require.NoError(t, err)
			require.Equal(t, "untouched", string(untouched))
		})
	}
}
