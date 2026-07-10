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

func TestRelocateLegacyStateRefusesSourceDestinationConflict(t *testing.T) {
	t.Setenv("TCLAUDE_AGENTD_SOCKET", "")
	t.Setenv(codexPermissionProfileEnv, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".tclaude")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "data"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.json"), []byte("old"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "data", "config.json"), []byte("new"), 0o600))

	err := RelocateLegacyState()
	require.ErrorContains(t, err, "both migration source and destination exist")
	assert.FileExists(t, filepath.Join(root, "config.json"))
	assert.FileExists(t, filepath.Join(root, "data", "config.json"))
}
