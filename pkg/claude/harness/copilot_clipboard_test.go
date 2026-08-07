package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func copilotClipboardFixture(t *testing.T) (string, func(string) string) {
	t.Helper()
	home := t.TempDir()
	stateDir := filepath.Join(home, ".copilot")
	getenv := func(name string) string {
		if name == CopilotHomeEnvVar {
			return stateDir
		}
		return ""
	}
	return home, getenv
}

func TestEnableCopilotCopyOnSelectPreservesSettingsAndMode(t *testing.T) {
	home, getenv := copilotClipboardFixture(t)
	stateDir, err := CopilotStateDirForLaunch(getenv, home)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(stateDir, 0o700))
	path := filepath.Join(stateDir, CopilotSettingsFileName)
	require.NoError(t, os.WriteFile(path, []byte(`{"theme":"dim"}`), 0o600))

	require.NoError(t, EnableCopilotCopyOnSelect(getenv, home))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	assert.Equal(t, true, settings[CopilotCopyOnSelectKey])
	assert.Equal(t, "dim", settings["theme"])
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	before := string(data)
	require.NoError(t, EnableCopilotCopyOnSelect(getenv, home))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, string(after), "an enabled setting is byte-stable on re-run")
}

func TestResolveCopilotCopyOnSelectHonorsLegacyPrecedence(t *testing.T) {
	home, getenv := copilotClipboardFixture(t)
	stateDir, err := CopilotStateDirForLaunch(getenv, home)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(stateDir, 0o700))
	settingsPath := filepath.Join(stateDir, CopilotSettingsFileName)
	configPath := filepath.Join(stateDir, CopilotConfigFileName)
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"copyOnSelect":true}`), 0o600))
	require.NoError(t, os.WriteFile(configPath, []byte(`{"copyOnSelect":false}`), 0o600))

	state, err := ResolveCopilotCopyOnSelect(getenv, home)
	require.NoError(t, err)
	assert.True(t, state.Present)
	assert.True(t, state.Valid)
	assert.False(t, state.Enabled)
	assert.Equal(t, configPath, state.Source)

	settingsBefore, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	require.NoError(t, EnableCopilotCopyOnSelect(getenv, home))
	settingsAfter, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Equal(t, string(settingsBefore), string(settingsAfter),
		"the effective legacy false is an explicit operator choice")
}

func TestResolveCopilotCopyOnSelectLeavesUnknownValueDeliberate(t *testing.T) {
	home, getenv := copilotClipboardFixture(t)
	stateDir, err := CopilotStateDirForLaunch(getenv, home)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(stateDir, 0o700))
	path := filepath.Join(stateDir, CopilotSettingsFileName)
	require.NoError(t, os.WriteFile(path, []byte(`{"copyOnSelect":"sometimes"}`), 0o600))

	state, err := ResolveCopilotCopyOnSelect(getenv, home)
	require.NoError(t, err)
	assert.True(t, state.Present)
	assert.False(t, state.Valid)
	assert.Equal(t, path, state.Source)
}

func TestResolveCopilotCopyOnSelectLeavesNullDeliberate(t *testing.T) {
	home, getenv := copilotClipboardFixture(t)
	stateDir, err := CopilotStateDirForLaunch(getenv, home)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(stateDir, 0o700))
	settingsPath := filepath.Join(stateDir, CopilotSettingsFileName)
	configPath := filepath.Join(stateDir, CopilotConfigFileName)
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"copyOnSelect":true}`), 0o600))
	require.NoError(t, os.WriteFile(configPath, []byte(`{"copyOnSelect":null}`), 0o600))

	state, err := ResolveCopilotCopyOnSelect(getenv, home)
	require.NoError(t, err)
	assert.True(t, state.Present)
	assert.False(t, state.Valid)
	assert.Equal(t, configPath, state.Source)

	before, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	require.NoError(t, EnableCopilotCopyOnSelect(getenv, home))
	after, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

func TestEnableCopilotCopyOnSelectRefusesNullSettingsDocument(t *testing.T) {
	home, getenv := copilotClipboardFixture(t)
	stateDir, err := CopilotStateDirForLaunch(getenv, home)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(stateDir, 0o700))
	path := filepath.Join(stateDir, CopilotSettingsFileName)
	require.NoError(t, os.WriteFile(path, []byte(`null`), 0o600))

	err = EnableCopilotCopyOnSelect(getenv, home)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "top-level null is not an object")
	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "null", string(data))
}
