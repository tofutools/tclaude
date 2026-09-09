package codex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDirectoryTrustPreservesNativeConfigAndRefusesConflictingShape(t *testing.T) {
	original := []byte("# operator comment\r\nmodel = \"fixture\"\r\n[projects.\"/other\"]\r\ntrust_level = \"trusted\"\r\n")
	changed, out, err := planCodexDirTrust(original, "/work")
	require.NoError(t, err)
	require.True(t, changed)
	require.Contains(t, string(out), string(original))
	require.Contains(t, string(out), "[projects.\"/work\"]\r\ntrust_level = \"trusted\"")
	changed, again, err := planCodexDirTrust(out, "/work")
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, out, again)
	for _, input := range []string{"projects = {}\n", "projects = 5\n", "[[projects]]\n"} {
		changed, _, err = planCodexDirTrust([]byte(input), "/work")
		require.Error(t, err)
		require.False(t, changed)
	}
}

func TestDirectoryTrustEditsOnlyExplicitNativeRoot(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	path := filepath.Join(root, "config.toml")
	original := []byte("# retain operator comment\nmodel = \"fixture\"\n")
	require.NoError(t, os.WriteFile(path, original, 0600))
	require.NoError(t, ensureDirectoryTrusted(root, cwd))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEqual(t, original, after)
	require.NoError(t, ensureDirectoryTrusted(root, cwd))
	again, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, after, again)
	malformed := []byte("projects = 5\n")
	require.NoError(t, os.WriteFile(path, malformed, 0600))
	require.Error(t, ensureDirectoryTrusted(root, cwd))
	again, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, malformed, again)
	require.Error(t, ensureDirectoryTrusted("relative", cwd))
	require.Error(t, ensureDirectoryTrusted(root, "relative"))
}
