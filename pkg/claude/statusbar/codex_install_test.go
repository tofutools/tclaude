package statusbar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// knownValidCodexItems is the full set of strings Codex's StatusLineItem
// FromStr accepts — every canonical kebab-case identifier plus the explicit
// serialize aliases — transcribed from
// codex-rs/tui/src/bottom_pane/status_line_setup.rs (serialize_all =
// "kebab_case"). It guards tclaude's curated default against drift: an
// identifier Codex doesn't recognise renders nothing, which is exactly the
// dead-config failure this feature exists to avoid.
var knownValidCodexItems = map[string]bool{
	"model": true, "model-name": true,
	"model-with-reasoning": true,
	"reasoning":            true,
	"current-dir":          true,
	"project-name":         true, "project": true, "project-root": true,
	"git-branch":           true,
	"pull-request-number":  true,
	"branch-changes":       true,
	"run-state":            true, "status": true,
	"permissions":          true,
	"approval-mode":        true, "approval": true,
	"context-remaining":   true,
	"context-used":        true, "context-usage": true,
	"five-hour-limit":     true,
	"weekly-limit":        true,
	"codex-version":       true,
	"context-window-size": true,
	"used-tokens":         true,
	"total-input-tokens":  true,
	"total-output-tokens": true,
	"thread-id":           true, "session-id": true,
	"fast-mode":   true,
	"raw-output":  true,
	"thread-title": true,
	"task-progress": true,
}

func TestCodexDefaultItemsAreValid(t *testing.T) {
	for _, item := range codexStatusLineItems {
		assert.Truef(t, knownValidCodexItems[item],
			"curated default %q is not a valid Codex StatusLineItem identifier", item)
	}
	require.NotEmpty(t, codexStatusLineItems)
}

func TestScanCodexConfig(t *testing.T) {
	managed := codexManagedMarker + "\n" + formatCodexStatusLine(codexStatusLineItems)

	cases := []struct {
		name    string
		toml    string
		present bool
		managed bool
		current bool
	}{
		{"empty", "", false, false, false},
		{"unrelated", "[model]\nname = \"gpt-5\"\n", false, false, false},
		{
			"managed-current",
			"[tui]\n" + managed + "\n",
			true, true, true,
		},
		{
			"managed-stale",
			"[tui]\n" + codexManagedMarker + "\nstatus_line = [\"model\"]\n",
			true, true, false,
		},
		{
			"user-owned",
			"[tui]\nstatus_line = [\"model\", \"git-branch\"]\n",
			true, false, false,
		},
		{
			"user-owned-dotted",
			"tui.status_line = [\"model\"]\n",
			true, false, false,
		},
		{
			"status_line-in-other-table-is-not-tui",
			"[other]\nstatus_line = [\"x\"]\n",
			false, false, false,
		},
		{
			"managed-multiline",
			"[tui]\n" + codexManagedPrefix + " custom\nstatus_line = [\n  \"model\",\n]\n",
			true, true, false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := scanCodexConfigData([]byte(tc.toml))
			assert.Equal(t, tc.present, sc.present, "present")
			assert.Equal(t, tc.managed, sc.managed, "managed")
			assert.Equal(t, tc.current, sc.current, "current")
		})
	}
}

// withTempHome points HOME (and USERPROFILE, which os.UserHomeDir reads on
// Windows) at a temp dir so Codex config writes are isolated. Returns the
// ~/.codex/config.toml path under it.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return filepath.Join(dir, ".codex", "config.toml")
}

func TestInstallCodex_FreshFile(t *testing.T) {
	path := withTempHome(t)

	outcome, err := InstallCodex()
	require.NoError(t, err)
	assert.Equal(t, CodexInstalled, outcome)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(got), "[tui]")
	assert.Contains(t, string(got), codexManagedPrefix)
	assert.Contains(t, string(got), formatCodexStatusLine(codexStatusLineItems))
	assert.True(t, CheckCodexInstalled())

	// Idempotent: a second call is a no-op and leaves the bytes unchanged.
	outcome2, err := InstallCodex()
	require.NoError(t, err)
	assert.Equal(t, CodexAlreadyInstalled, outcome2)
	got2, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(got), string(got2), "idempotent install must not rewrite the file")
}

func TestInstallCodex_PreservesExistingKeysAndComments(t *testing.T) {
	path := withTempHome(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	original := `# my codex config
model = "gpt-5-codex"

[mcp_servers.foo]
command = "bar"  # keep this comment
`
	require.NoError(t, os.WriteFile(path, []byte(original), 0o644))

	outcome, err := InstallCodex()
	require.NoError(t, err)
	assert.Equal(t, CodexInstalled, outcome)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	s := string(got)
	// Every original line survives.
	for _, line := range []string{
		"# my codex config",
		`model = "gpt-5-codex"`,
		"[mcp_servers.foo]",
		`command = "bar"  # keep this comment`,
	} {
		assert.Containsf(t, s, line, "original line must be preserved: %q", line)
	}
	// Our managed block was appended.
	assert.Contains(t, s, "[tui]")
	assert.Contains(t, s, formatCodexStatusLine(codexStatusLineItems))
	assert.True(t, CheckCodexInstalled())
}

func TestInstallCodex_InsertsIntoExistingTuiTable(t *testing.T) {
	path := withTempHome(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	original := "[tui]\ntheme = \"dark\"\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o644))

	outcome, err := InstallCodex()
	require.NoError(t, err)
	assert.Equal(t, CodexInstalled, outcome)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	s := string(got)
	assert.Contains(t, s, `theme = "dark"`, "existing [tui] key preserved")
	assert.Contains(t, s, formatCodexStatusLine(codexStatusLineItems))
	// Exactly one [tui] table — we must not create a duplicate (invalid TOML).
	assert.Equal(t, 1, strings.Count(s, "[tui]"), "must reuse the existing [tui] table")
	assert.True(t, CheckCodexInstalled())
}

func TestInstallCodex_DoesNotClobberUserOwned(t *testing.T) {
	path := withTempHome(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	original := "[tui]\nstatus_line = [\"model\", \"git-branch\"]\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o644))

	outcome, err := InstallCodex()
	require.NoError(t, err)
	assert.Equal(t, CodexUserManaged, outcome)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, string(got), "user-owned status_line must be left byte-for-byte")
	assert.False(t, CheckCodexInstalled())
	assert.True(t, CodexStatusLineUserManaged())
}

func TestInstallCodex_RepairsStaleManaged(t *testing.T) {
	path := withTempHome(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	original := "[tui]\n" + codexManagedMarker + "\nstatus_line = [\"model\"]\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o644))

	outcome, err := InstallCodex()
	require.NoError(t, err)
	assert.Equal(t, CodexRepaired, outcome)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	s := string(got)
	assert.Contains(t, s, formatCodexStatusLine(codexStatusLineItems))
	assert.NotContains(t, s, `status_line = ["model"]`, "stale value must be gone")
	assert.Contains(t, s, codexManagedPrefix, "marker preserved")
	assert.True(t, CheckCodexInstalled())
}

func TestPlanCodexStatusLine_NoopReturnsOriginalBytes(t *testing.T) {
	// User-owned and already-installed are pure no-ops that must hand back the
	// exact original bytes (the caller relies on this to skip the write).
	userOwned := []byte("[tui]\nstatus_line = [\"model\"]\n")
	outcome, out := planCodexStatusLine(userOwned)
	assert.Equal(t, CodexUserManaged, outcome)
	assert.Equal(t, userOwned, out)

	installed := []byte("[tui]\n" + codexManagedMarker + "\n" + formatCodexStatusLine(codexStatusLineItems) + "\n")
	outcome, out = planCodexStatusLine(installed)
	assert.Equal(t, CodexAlreadyInstalled, outcome)
	assert.Equal(t, installed, out)
}

func TestPlanCodexStatusLine_PreservesCRLF(t *testing.T) {
	original := []byte("# cfg\r\nmodel = \"gpt-5\"\r\n")
	outcome, out := planCodexStatusLine(original)
	require.Equal(t, CodexInstalled, outcome)
	assert.Truef(t, strings.Contains(string(out), "\r\n"), "CRLF line endings must be preserved")
	assert.NotContains(t, strings.ReplaceAll(string(out), "\r\n", ""), "\n",
		"must not introduce bare LF into a CRLF file")
	// And the result still parses as installed.
	assert.True(t, scanCodexConfigData(out).current)
}
