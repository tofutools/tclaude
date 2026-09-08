package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSandboxStateDirectoriesRetainLoginAndRefuseEscapingLink(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, prepareSandboxStateDirectories(root))
	auth := filepath.Join(root, "data", "opencode", "auth.json")
	require.NoError(t, os.WriteFile(auth, []byte("retained native login"), 0600))
	require.NoError(t, prepareSandboxStateDirectories(root))
	content, err := os.ReadFile(auth)
	require.NoError(t, err)
	require.Equal(t, "retained native login", string(content))

	changed, outside := t.TempDir(), t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(changed, "data")))
	require.ErrorContains(t, prepareSandboxStateDirectories(changed), "owned directory")
	require.NoDirExists(t, filepath.Join(outside, "opencode"))
}
