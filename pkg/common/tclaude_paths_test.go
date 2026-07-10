package common

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreSplitAgentdReachableDistinguishesNewDaemon(t *testing.T) {
	// Keep the socket path below macOS's short sockaddr_un limit. t.TempDir's
	// /var/folders/... path is already too long before the socket suffix.
	home, err := os.MkdirTemp("/tmp", "tc-paths-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	legacyPath := filepath.Join(home, ".tclaude-agentd.sock")
	legacy, err := net.Listen("unix", legacyPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = legacy.Close() })
	assert.True(t, PreSplitAgentdReachable(), "legacy-only listener is a pre-split daemon")

	apiDir := filepath.Join(home, ".tclaude", "api")
	require.NoError(t, os.MkdirAll(apiDir, 0o700))
	canonical, err := net.Listen("unix", filepath.Join(apiDir, "agentd.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = canonical.Close() })
	assert.False(t, PreSplitAgentdReachable(), "new daemon also binds legacy sockets but canonical wins")
}

func TestTclaudeStatePathKeepsUnmigratedLegacyEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := filepath.Join(home, ".tclaude")
	require.NoError(t, os.MkdirAll(root, 0o700))
	legacy := filepath.Join(root, "config.json")
	require.NoError(t, os.WriteFile(legacy, []byte("legacy"), 0o600))
	assert.Equal(t, legacy, TclaudeStatePath("config.json"))

	canonical := filepath.Join(root, "data", "config.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(canonical), 0o700))
	require.NoError(t, os.WriteFile(canonical, []byte("new"), 0o600))
	assert.Equal(t, canonical, TclaudeStatePath("config.json"), "a conflict is handled by fail-closed migration, not silently preferring legacy")
}
