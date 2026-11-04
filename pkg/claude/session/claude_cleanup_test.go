package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

// End-to-end: a positive claude_cleanup_period_days in
// ~/.tclaude/data/config.json is read live and synced into
// ~/.claude/settings.json cleanupPeriodDays.
func TestEnsureClaudeCleanupPeriod_SyncsFromConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	dataDir := filepath.Join(tmpDir, ".tclaude", "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.json"),
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

// A symlinked settings.json (a dotfile manager's) is written through: the link
// survives and the file it points at carries the new value. Mode is kept too.
// This is how InstallHooks has always written the same file.
func TestApplyClaudeCleanupPeriod_WritesThroughSymlinkAndKeepsMode(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	settingsPath := filepath.Join(tmpDir, ".claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o755))
	managed := filepath.Join(tmpDir, "dotfiles", "claude-settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(managed), 0o755))
	require.NoError(t, os.WriteFile(managed, []byte(`{"theme":"dark"}`), 0o640))
	require.NoError(t, os.Symlink(managed, settingsPath))

	require.NoError(t, applyClaudeCleanupPeriod(365))

	fi, err := os.Lstat(settingsPath)
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "settings.json must remain a symlink")
	days, settings := readCleanupDays(t, managed)
	assert.Equal(t, 365, days, "the symlink's target carries the value")
	assert.JSONEq(t, `"dark"`, string(settings["theme"]), "other keys survive")
	mfi, err := os.Stat(managed)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), mfi.Mode().Perm(), "existing mode is preserved")
}

// The cleanup sync takes the same settings.json lock InstallHooks does, so a
// hook install in another tclaude process cannot interleave with it and drop
// one side's change. A held lock makes the sync refuse rather than write.
func TestApplyClaudeCleanupPeriod_HonorsSettingsLock(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	settingsPath := filepath.Join(tmpDir, ".claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o755))
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"theme":"dark"}`), 0o644))

	holder := flock.New(settingsPath + ".tclaude.lock")
	require.NoError(t, holder.Lock())
	defer func() { require.NoError(t, holder.Unlock()) }()
	old := installClaudeHooksLockTimeout
	installClaudeHooksLockTimeout = 50 * time.Millisecond
	t.Cleanup(func() { installClaudeHooksLockTimeout = old })

	err := applyClaudeCleanupPeriod(365)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lock Claude settings")
	days, _ := readCleanupDays(t, settingsPath)
	assert.Equal(t, -1, days, "a sync that could not take the lock must not write")
}

// A hook install and a cleanup sync racing in one process both land.
func TestApplyClaudeCleanupPeriod_ConcurrentWithInstallHooksBothSurvive(t *testing.T) {
	setTestHookCommand(t)
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	for round := range 5 {
		_ = os.Remove(ClaudeSettingsPath())
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); errs[0] = InstallHooks() }()
		go func() { defer wg.Done(); errs[1] = applyClaudeCleanupPeriod(100 + round) }()
		wg.Wait()
		require.NoError(t, errs[0], "round %d install", round)
		require.NoError(t, errs[1], "round %d cleanup", round)

		days, settings := readCleanupDays(t, ClaudeSettingsPath())
		assert.Equal(t, 100+round, days, "round %d: cleanupPeriodDays survives", round)
		assert.Contains(t, settings, "hooks", "round %d: hooks survive", round)
	}
}
