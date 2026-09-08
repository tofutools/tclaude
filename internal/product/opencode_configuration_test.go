package product

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenCodeCompositionUsesExplicitNativeEnvironmentAndXDGRoots(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("OPENAI_API_KEY", "synthetic API key")
	t.Setenv("UNRELATED_DAEMON_SECRET", "must not forward")
	config, err := (opencodeNativeConfig{environment: []string{"OPENAI_API_KEY"}}).providerConfig(state)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, ".config", "opencode"), config.NativeConfigDirectory)
	require.Equal(t, filepath.Join(home, ".local", "share", "opencode"), config.NativeDataDirectory)
	require.Equal(t, []string{"OPENAI_API_KEY=synthetic API key"}, config.Environment)
	// Configuration discovery derives paths; it does not read or create native
	// user login/config state before an explicitly admitted preparation.
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	require.Empty(t, entries)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "chosen-config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "chosen-data"))
	config, err = (opencodeNativeConfig{}).providerConfig(state)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "chosen-config", "opencode"), config.NativeConfigDirectory)
	require.Equal(t, filepath.Join(home, "chosen-data", "opencode"), config.NativeDataDirectory)
	require.Empty(t, config.Environment)
	_, err = (opencodeNativeConfig{environment: []string{"HOME"}}).providerConfig(state)
	require.Error(t, err)
	t.Setenv("XDG_DATA_HOME", "relative")
	_, err = (opencodeNativeConfig{}).providerConfig(state)
	require.ErrorContains(t, err, "absolute")
}
