package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedGeminiHome(t *testing.T) (settings, command string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv(GeminiHomeEnvVar, home)
	old := geminiHookCommandString
	geminiHookCommandString = func() string { return "tclaude session hook-callback" }
	t.Cleanup(func() { geminiHookCommandString = old })
	return filepath.Join(home, ".gemini", "settings.json"), geminiHookCommandStr()
}

func readGeminiSettingsForTest(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}

func TestGeminiDescriptorHasHooks(t *testing.T) {
	h, ok := Get(GeminiName)
	require.True(t, ok)
	assert.True(t, h.SupportsHooks())
	assert.False(t, h.SessionEndProvesExit(), "Gemini does not wait for SessionEnd")
	assert.True(t, h.AnnouncesSessionAfterPrompt(), "the -i turn can race SessionStart")
}

// The installed command must be inert towards Gemini: stdout is a control
// channel, stderr is parsed when stdout is empty, and exit 2 blocks or
// retries a turn.
func TestGeminiHookCommandIsInert(t *testing.T) {
	_, command := seedGeminiHome(t)
	assert.Equal(t, "tclaude session hook-callback >/dev/null 2>&1 || true", command)
	assert.True(t, isTclaudeHookCommand(command))
}

func TestGeminiHookInstallPreservesSettingsAndIsIdempotent(t *testing.T) {
	path, command := seedGeminiHome(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{
  "general": {"vimMode": true},
  "hooks": {
    "AfterAgent": [{"matcher": "", "hooks": [{"type": "command", "command": "notify-send done"}]}],
    "BeforeTool": [{"matcher": "run_shell_command", "hooks": [
      {"type": "command", "command": "/old/bin/tclaude session hook-callback"},
      {"type": "command", "command": "audit.sh", "timeout": 1000}
    ]}]
  }
}`), 0o600))

	inst := geminiHookInstaller{}
	installed, missing, repair := inst.Check()
	assert.False(t, installed)
	assert.ElementsMatch(t, GeminiHookEvents, missing)
	assert.True(t, repair, "a stale tclaude command under an event tclaude no longer installs")

	require.NoError(t, inst.Install())
	first, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, inst.Install())
	second, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second), "a second install is a no-op")

	installed, missing, repair = inst.Check()
	assert.True(t, installed, "missing=%v", missing)
	assert.False(t, repair)

	settings := readGeminiSettingsForTest(t, path)
	assert.Equal(t, map[string]any{"vimMode": true}, settings["general"], "unrelated settings survive")
	hooks := settings["hooks"].(map[string]any)
	for _, event := range GeminiHookEvents {
		groups := hooks[event].([]any)
		last := groups[len(groups)-1].(map[string]any)
		entry := last["hooks"].([]any)[0].(map[string]any)
		assert.Equal(t, command, entry["command"], event)
		assert.Equal(t, float64(geminiHookTimeoutMs), entry["timeout"], event)
	}
	afterAgent := hooks["AfterAgent"].([]any)
	require.Len(t, afterAgent, 2, "the operator's own AfterAgent hook is kept")
	beforeTool := hooks["BeforeTool"].([]any)
	require.Len(t, beforeTool, 1)
	entries := beforeTool[0].(map[string]any)["hooks"].([]any)
	require.Len(t, entries, 1, "only the stale tclaude command is stripped from a mixed group")
	assert.Equal(t, "audit.sh", entries[0].(map[string]any)["command"])
	assert.Equal(t, "run_shell_command", beforeTool[0].(map[string]any)["matcher"])
}

func TestGeminiHookInstallRefusesNonStrictJSON(t *testing.T) {
	path, _ := seedGeminiHome(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	body := "{\n  // my settings\n  \"general\": {}\n}\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	inst := geminiHookInstaller{}
	installed, _, repair := inst.Check()
	assert.False(t, installed)
	assert.True(t, repair)
	assert.ErrorContains(t, inst.Install(), "strict JSON")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, body, string(after), "a commented settings file is never rewritten")
}

func TestGeminiHookInstallCreatesSettings(t *testing.T) {
	path, _ := seedGeminiHome(t)
	inst := geminiHookInstaller{}
	installed, missing, repair := inst.Check()
	assert.False(t, installed)
	assert.Equal(t, []string{"all"}, missing)
	assert.False(t, repair)
	require.NoError(t, inst.Install())
	assert.Equal(t, path, inst.ConfigTarget())
	installed, _, _ = inst.Check()
	assert.True(t, installed)
	assert.NotEmpty(t, inst.TrustNote())
}
