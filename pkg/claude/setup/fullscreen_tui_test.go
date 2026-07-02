package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// readTUIFromDisk returns the raw "tui" value from the live settings.json
// (whatever HOME currently points at), or ("", false) when the file or key
// is absent — the assertion surface for the fullscreen-TUI tests.
func readTUIFromDisk(t *testing.T) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(session.ClaudeSettingsPath())
	if os.IsNotExist(err) {
		return "", false
	}
	require.NoError(t, err)
	var tree map[string]any
	require.NoError(t, json.Unmarshal(data, &tree))
	raw, ok := tree["tui"]
	if !ok {
		return "", false
	}
	s, ok := raw.(string)
	require.True(t, ok, "tui should be a string")
	return s, true
}

// seedSettings writes a settings.json with the given raw content at the live
// path, creating ~/.claude.
func seedSettings(t *testing.T, content string) {
	t.Helper()
	path := session.ClaudeSettingsPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// On a fresh config (no settings.json), the prompt fires and writes
// "tui": "fullscreen".
func TestConfigureFullscreenTUI_WritesWhenAbsent(t *testing.T) {
	tempHome(t)

	out := captureStdout(t, func() {
		configureFullscreenTUI(&Params{Yes: true})
	})
	assert.Contains(t, out, "✓ Fullscreen TUI enabled")

	got, ok := readTUIFromDisk(t)
	require.True(t, ok, "tui must be written")
	assert.Equal(t, fullscreenTUIValue, got)
}

// A settings.json that exists but has no "tui" key is still a fresh choice —
// the prompt fires and the key is added through the configure entry point,
// preserving every other key AND the file's (private 0600) permission mode.
func TestConfigureFullscreenTUI_AddsKeyPreservingOthers(t *testing.T) {
	tempHome(t)
	// Seed a private-mode file directly so we can also assert mode is kept
	// across the whole configure→enable path (not just the writer in isolation).
	path := session.ClaudeSettingsPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{"model":"sonnet","hooks":{"Stop":[]}}`), 0o600))

	out := captureStdout(t, func() {
		configureFullscreenTUI(&Params{Yes: true})
	})
	assert.Contains(t, out, "✓ Fullscreen TUI enabled")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var tree map[string]any
	require.NoError(t, json.Unmarshal(data, &tree))
	assert.Equal(t, fullscreenTUIValue, tree["tui"])
	assert.Equal(t, "sonnet", tree["model"], "sibling keys must be preserved")
	assert.Contains(t, tree, "hooks", "sibling keys must be preserved")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"configure→enable must preserve the private file mode")
}

// Already-fullscreen: no prompt, no rewrite — a true no-op.
func TestConfigureFullscreenTUI_AlreadyFullscreen(t *testing.T) {
	tempHome(t)
	seedSettings(t, `{"tui":"fullscreen"}`)
	before, err := os.ReadFile(session.ClaudeSettingsPath())
	require.NoError(t, err)

	out := captureStdout(t, func() {
		configureFullscreenTUI(&Params{Yes: true})
	})
	assert.Contains(t, out, "already enabled")

	after, err := os.ReadFile(session.ClaudeSettingsPath())
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "file must be left byte-identical")
}

// A deliberate non-fullscreen value ("classic") is respected: no prompt, no
// overwrite — the operator's choice survives a re-run.
func TestConfigureFullscreenTUI_RespectsExplicitValue(t *testing.T) {
	tempHome(t)
	seedSettings(t, `{"tui":"classic"}`)

	out := captureStdout(t, func() {
		configureFullscreenTUI(&Params{Yes: true})
	})
	assert.Contains(t, out, "leaving it as-is")

	got, ok := readTUIFromDisk(t)
	require.True(t, ok)
	assert.Equal(t, "classic", got, "an explicit choice must not be overwritten")
}

// Declining the prompt writes nothing — the file is not created / touched.
func TestConfigureFullscreenTUI_DeclineWritesNothing(t *testing.T) {
	tempHome(t)

	var out string
	withStdin(t, "n\n", func() {
		out = captureStdout(t, func() {
			configureFullscreenTUI(&Params{Yes: false})
		})
	})
	assert.Contains(t, out, "Skipped")

	_, ok := readTUIFromDisk(t)
	assert.False(t, ok, "declining must not write tui")
}

// A corrupt settings.json is never clobbered: configure warns and leaves the
// unparseable file exactly as-is (it also holds hooks/permissions/sandbox).
func TestConfigureFullscreenTUI_CorruptNotClobbered(t *testing.T) {
	tempHome(t)
	const garbage = "{ this is not valid json"
	seedSettings(t, garbage)

	out := captureStdout(t, func() {
		configureFullscreenTUI(&Params{Yes: true})
	})
	assert.Contains(t, out, "leaving it untouched")

	data, err := os.ReadFile(session.ClaudeSettingsPath())
	require.NoError(t, err)
	assert.Equal(t, garbage, string(data), "a corrupt settings.json must not be overwritten")
}

// enableFullscreenTUI preserves the file's permission mode (a private 0600
// settings.json stays 0600).
func TestEnableFullscreenTUI_PreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"model":"opus"}`), 0o600))

	require.NoError(t, enableFullscreenTUI(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "private mode must be preserved")

	var tree map[string]any
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &tree))
	assert.Equal(t, fullscreenTUIValue, tree["tui"])
	assert.Equal(t, "opus", tree["model"])
}

// enableFullscreenTUI creates a missing file (with just the one key).
func TestEnableFullscreenTUI_CreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "settings.json")

	require.NoError(t, enableFullscreenTUI(path))

	var tree map[string]any
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &tree))
	assert.Equal(t, fullscreenTUIValue, tree["tui"])
}

// enableFullscreenTUI refuses to clobber an unparseable file.
func TestEnableFullscreenTUI_CorruptErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o644))

	assert.Error(t, enableFullscreenTUI(path))
}

// readClaudeTUIMode's contract: absent file/key -> (present=false); present
// key -> its value; corrupt -> error.
func TestReadClaudeTUIMode(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		_, present, err := readClaudeTUIMode(filepath.Join(dir, "nope.json"))
		require.NoError(t, err)
		assert.False(t, present)
	})
	t.Run("absent key", func(t *testing.T) {
		p := filepath.Join(dir, "a.json")
		require.NoError(t, os.WriteFile(p, []byte(`{"model":"opus"}`), 0o644))
		_, present, err := readClaudeTUIMode(p)
		require.NoError(t, err)
		assert.False(t, present)
	})
	t.Run("null key is absent", func(t *testing.T) {
		p := filepath.Join(dir, "n.json")
		require.NoError(t, os.WriteFile(p, []byte(`{"tui":null}`), 0o644))
		_, present, err := readClaudeTUIMode(p)
		require.NoError(t, err)
		assert.False(t, present)
	})
	t.Run("present value", func(t *testing.T) {
		p := filepath.Join(dir, "f.json")
		require.NoError(t, os.WriteFile(p, []byte(`{"tui":"fullscreen"}`), 0o644))
		mode, present, err := readClaudeTUIMode(p)
		require.NoError(t, err)
		assert.True(t, present)
		assert.Equal(t, "fullscreen", mode)
	})
	t.Run("corrupt errors", func(t *testing.T) {
		p := filepath.Join(dir, "bad.json")
		require.NoError(t, os.WriteFile(p, []byte("{oops"), 0o644))
		_, _, err := readClaudeTUIMode(p)
		assert.Error(t, err)
	})
}

// checkFullscreenTUI reports each state without writing anything.
func TestCheckFullscreenTUI(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		tempHome(t)
		seedSettings(t, `{"tui":"fullscreen"}`)
		out := captureStdout(t, checkFullscreenTUI)
		assert.Contains(t, out, "✓ Fullscreen TUI enabled")
	})
	t.Run("other value", func(t *testing.T) {
		tempHome(t)
		seedSettings(t, `{"tui":"classic"}`)
		out := captureStdout(t, checkFullscreenTUI)
		assert.Contains(t, out, "not fullscreen")
	})
	t.Run("absent", func(t *testing.T) {
		tempHome(t)
		out := captureStdout(t, checkFullscreenTUI)
		assert.Contains(t, out, "✗ Fullscreen TUI not enabled")
	})
}

// The fullscreen-TUI section shows up in both `tclaude setup --check` output
// and (as a smoke check) doesn't write during --check.
func TestCheckStatus_PrintsFullscreenTUISection(t *testing.T) {
	tempHome(t)

	out := captureStdout(t, func() {
		require.NoError(t, checkStatus(""))
	})
	assert.Contains(t, out, "=== Fullscreen TUI ===")

	// --check must never write settings.json.
	assert.NoFileExists(t, session.ClaudeSettingsPath())
}
