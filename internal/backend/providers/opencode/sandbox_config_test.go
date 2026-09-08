package opencode

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSandboxConfigurationUsesNativeSettingsAndPreservesAuthoredGitignore(t *testing.T) {
	state, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, prepareSandboxStateDirectories(state))
	source := filepath.Join(t.TempDir(), "opencode")
	require.NoError(t, os.Mkdir(source, 0700))
	source, err = filepath.EvalSymlinks(source)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "opencode.json"), []byte("existing settings"), 0600))
	configHome, resources, err := prepareSandboxConfiguration(source, state)
	require.NoError(t, err)
	require.Equal(t, source, resources[0].Path)
	if runtime.GOOS == "linux" {
		require.Equal(t, filepath.Join(state, "config"), configHome)
		require.Len(t, resources, 2)
	} else {
		require.Equal(t, filepath.Dir(source), configHome)
		require.Len(t, resources, 1)
	}
	ignore := filepath.Join(source, ".gitignore")
	content, err := os.ReadFile(ignore)
	require.NoError(t, err)
	require.Equal(t, nativeConfigGitignore, string(content))
	require.NoError(t, os.WriteFile(ignore, []byte("authored ignore\n"), 0600))
	_, _, err = prepareSandboxConfiguration(source, state)
	require.NoError(t, err)
	content, err = os.ReadFile(ignore)
	require.NoError(t, err)
	require.Equal(t, "authored ignore\n", string(content))
	require.NoError(t, os.Remove(ignore))
	require.NoError(t, os.Symlink(filepath.Join(source, "opencode.json"), ignore))
	_, _, err = prepareSandboxConfiguration(source, state)
	require.ErrorContains(t, err, "regular file")
}
