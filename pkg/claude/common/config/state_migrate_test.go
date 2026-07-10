package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelocateLegacyStateMovesFilesAndDirectories(t *testing.T) {
	t.Setenv("TCLAUDE_AGENTD_SOCKET", "")
	t.Setenv(codexPermissionProfileEnv, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".tclaude")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "processes"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"log_level":"debug"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "output.log.1"), []byte("private history"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "processes", "kept"), []byte("yes"), 0o600))

	require.NoError(t, RelocateLegacyState())
	assert.NoFileExists(t, filepath.Join(root, "config.json"))
	assert.FileExists(t, filepath.Join(root, "data", "config.json"))
	assert.FileExists(t, filepath.Join(root, "data", "processes", "kept"))
	assert.FileExists(t, filepath.Join(root, "data", "output.log.1"))
	require.NoError(t, RelocateLegacyState(), "second migration must be a no-op")
}

func TestRelocateLegacyStatePreservesAuthoritativeConflictWithoutBlocking(t *testing.T) {
	t.Setenv("TCLAUDE_AGENTD_SOCKET", "")
	t.Setenv(codexPermissionProfileEnv, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".tclaude")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "data"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.json"), []byte("old"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "data", "config.json"), []byte("new"), 0o600))

	require.NoError(t, RelocateLegacyState())
	assert.FileExists(t, filepath.Join(root, "config.json"))
	assert.FileExists(t, filepath.Join(root, "data", "config.json"))
}

func TestRelocateLegacyStateQuarantinesRecreatedLogWithoutBlocking(t *testing.T) {
	t.Setenv("TCLAUDE_AGENTD_SOCKET", "")
	t.Setenv(codexPermissionProfileEnv, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".tclaude")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "data"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "output.log"), []byte("legacy writer"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "data", "output.log"), []byte("canonical"), 0o600))

	require.NoError(t, RelocateLegacyState())
	assert.NoFileExists(t, filepath.Join(root, "output.log"))
	canonical, err := os.ReadFile(filepath.Join(root, "data", "output.log"))
	require.NoError(t, err)
	assert.Equal(t, "canonical", string(canonical))
	quarantined, err := filepath.Glob(filepath.Join(root, "data", "output.log.legacy-*"))
	require.NoError(t, err)
	require.Len(t, quarantined, 1)
	legacy, err := os.ReadFile(quarantined[0])
	require.NoError(t, err)
	assert.Equal(t, "legacy writer", string(legacy))
}
