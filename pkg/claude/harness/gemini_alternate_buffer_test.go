package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeminiAlternateBufferEnableKeepsOtherSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(GeminiHomeEnvVar, "")
	path := filepath.Join(home, ".gemini", "settings.json")

	state, err := ReadGeminiAlternateBuffer()
	require.NoError(t, err)
	assert.False(t, state.Present, "a missing settings file has no value")

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path,
		[]byte(`{"hooks":{"AfterAgent":[]},"ui":{"theme":"Dracula"},"security":{"auth":{"selectedType":"gemini-api-key"}}}`), 0o600))
	require.NoError(t, EnableGeminiAlternateBuffer())

	state, err = ReadGeminiAlternateBuffer()
	require.NoError(t, err)
	assert.True(t, state.Present && state.Valid && state.Enabled)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	for _, kept := range []string{`"theme": "Dracula"`, `"AfterAgent"`, `"selectedType": "gemini-api-key"`} {
		assert.Contains(t, string(data), kept)
	}
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	require.NoError(t, EnableGeminiAlternateBuffer(), "enabling twice is a no-op")
}

func TestGeminiAlternateBufferLeavesCommentedSettingsAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(GeminiHomeEnvVar, "")
	path := filepath.Join(home, ".gemini", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	original := []byte("{\n  // mine\n  \"ui\": {}\n}\n")
	require.NoError(t, os.WriteFile(path, original, 0o600))

	_, err := ReadGeminiAlternateBuffer()
	require.Error(t, err)
	require.Error(t, EnableGeminiAlternateBuffer())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, data)
}

func TestGeminiAlternateBufferReportsADeliberateFalse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(GeminiHomeEnvVar, "")
	path := filepath.Join(home, ".gemini", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{"ui":{"useAlternateBuffer":false}}`), 0o600))
	state, err := ReadGeminiAlternateBuffer()
	require.NoError(t, err)
	assert.True(t, state.Present && state.Valid)
	assert.False(t, state.Enabled)
}
