package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigureCopilotClipboardEnablesAbsentSetting(t *testing.T) {
	home := tempHome(t)
	t.Setenv("COPILOT_HOME", "")

	out := captureStdout(t, func() {
		configureCopilotClipboard(&Params{Yes: true})
	})
	assert.Contains(t, out, "Copilot copy-on-select enabled")

	data, err := os.ReadFile(filepath.Join(home, ".copilot", "settings.json"))
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	assert.Equal(t, true, settings["copyOnSelect"])
}

func TestConfigureCopilotClipboardRespectsExplicitFalse(t *testing.T) {
	home := tempHome(t)
	t.Setenv("COPILOT_HOME", "")
	path := filepath.Join(home, ".copilot", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	const original = `{"copyOnSelect":false,"theme":"github"}`
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))

	out := captureStdout(t, func() {
		configureCopilotClipboard(&Params{Yes: true})
	})
	assert.Contains(t, out, "leaving it as-is")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, string(after))
}

func TestCheckCopilotClipboardReportsEnabled(t *testing.T) {
	home := tempHome(t)
	t.Setenv("COPILOT_HOME", "")
	path := filepath.Join(home, ".copilot", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{"copyOnSelect":true}`), 0o600))

	out := captureStdout(t, checkCopilotClipboard)
	assert.Contains(t, out, "✓ Copilot copy-on-select enabled")
}
