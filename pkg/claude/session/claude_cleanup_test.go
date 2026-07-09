package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readCleanupDays returns the cleanupPeriodDays value on disk (-1 if the key or
// file is absent) and the raw settings map, so a test can also assert that
// unrelated keys survived.
func readCleanupDays(t *testing.T, settingsPath string) (int, map[string]json.RawMessage) {
	t.Helper()
	data, err := os.ReadFile(settingsPath)
	if os.IsNotExist(err) {
		return -1, nil
	}
	require.NoError(t, err)
	var settings map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &settings))
	raw, ok := settings["cleanupPeriodDays"]
	if !ok {
		return -1, settings
	}
	var days int
	require.NoError(t, json.Unmarshal(raw, &days))
	return days, settings
}

// applyClaudeCleanupPeriod creates settings.json when absent and writes the
// requested cleanupPeriodDays.
func TestApplyClaudeCleanupPeriod_CreatesFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	settingsPath := filepath.Join(tmpDir, ".claude", "settings.json")

	require.NoError(t, applyClaudeCleanupPeriod(99999))

	days, _ := readCleanupDays(t, settingsPath)
	assert.Equal(t, 99999, days)
}

// applyClaudeCleanupPeriod merges into an existing settings.json, preserving
// every unrelated key (the raw-message merge shape InstallHooks uses).
func TestApplyClaudeCleanupPeriod_PreservesOtherKeys(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	settingsPath := filepath.Join(tmpDir, ".claude", "settings.json")

	require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o755))
	seed := map[string]any{
		"cleanupPeriodDays": 30,
		"theme":             "dark",
		"hooks":             map[string]any{"Stop": []any{}},
	}
	seedRaw, err := json.MarshalIndent(seed, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(settingsPath, seedRaw, 0o644))

	require.NoError(t, applyClaudeCleanupPeriod(99999))

	days, settings := readCleanupDays(t, settingsPath)
	assert.Equal(t, 99999, days)
	assert.Contains(t, settings, "theme", "unrelated key must survive")
	assert.Contains(t, settings, "hooks", "hooks section must survive")

	var theme string
	require.NoError(t, json.Unmarshal(settings["theme"], &theme))
	assert.Equal(t, "dark", theme)
}

// applyClaudeCleanupPeriod is idempotent: a second call with the same value
// leaves the file byte-identical (the on-disk-value-differs guard skips the
// write), so calling it from every session start is cheap.
func TestApplyClaudeCleanupPeriod_IdempotentNoRewrite(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	settingsPath := filepath.Join(tmpDir, ".claude", "settings.json")

	require.NoError(t, applyClaudeCleanupPeriod(99999))
	first, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	firstStat, err := os.Stat(settingsPath)
	require.NoError(t, err)

	// A same-value re-apply must not rewrite the bytes.
	require.NoError(t, applyClaudeCleanupPeriod(99999))
	second, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
	secondStat, err := os.Stat(settingsPath)
	require.NoError(t, err)
	assert.Equal(t, firstStat.ModTime(), secondStat.ModTime(), "unchanged value must not rewrite the file")

	// A different value does update it.
	require.NoError(t, applyClaudeCleanupPeriod(365))
	days, _ := readCleanupDays(t, settingsPath)
	assert.Equal(t, 365, days)
}

// End-to-end: a positive claude_cleanup_period_days in ~/.tclaude/config.json
// is read live and synced into ~/.claude/settings.json cleanupPeriodDays.
func TestEnsureClaudeCleanupPeriod_SyncsFromConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	tclaudeDir := filepath.Join(tmpDir, ".tclaude")
	require.NoError(t, os.MkdirAll(tclaudeDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tclaudeDir, "config.json"),
		[]byte(`{"claude_cleanup_period_days": 99999}`), 0o644))

	require.NoError(t, EnsureClaudeCleanupPeriod())

	days, _ := readCleanupDays(t, filepath.Join(tmpDir, ".claude", "settings.json"))
	assert.Equal(t, 99999, days)
}

// End-to-end: with the override unset, EnsureClaudeCleanupPeriod is a no-op —
// it must not create settings.json or touch cleanupPeriodDays.
func TestEnsureClaudeCleanupPeriod_UnsetIsNoOp(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	tclaudeDir := filepath.Join(tmpDir, ".tclaude")
	require.NoError(t, os.MkdirAll(tclaudeDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tclaudeDir, "config.json"),
		[]byte(`{"log_level": "info"}`), 0o644))

	require.NoError(t, EnsureClaudeCleanupPeriod())

	_, err := os.Stat(filepath.Join(tmpDir, ".claude", "settings.json"))
	assert.True(t, os.IsNotExist(err), "no override → settings.json must not be created")
}

// A corrupt settings.json is surfaced as an error, not silently overwritten
// (matches InstallHooks' fail-closed parse).
func TestApplyClaudeCleanupPeriod_CorruptSettingsErrors(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	settingsPath := filepath.Join(tmpDir, ".claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o755))
	require.NoError(t, os.WriteFile(settingsPath, []byte("{not json"), 0o644))

	assert.Error(t, applyClaudeCleanupPeriod(99999))
}
