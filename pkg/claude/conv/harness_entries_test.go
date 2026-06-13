package conv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// setupHarnessTestHome gives the test a throwaway HOME that both the Claude
// read path and the Codex ConvStore (os.UserHomeDir) resolve to, with a
// fresh SQLite store.
func setupHarnessTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()
	return home
}

// writeClaudeConv lays down a minimal indexable Claude `.jsonl` under the
// encoded project dir for cwd.
func writeClaudeConv(t *testing.T, cwd, convID, prompt string) {
	t.Helper()
	projDir := GetClaudeProjectPath(cwd)
	require.NoError(t, os.MkdirAll(projDir, 0o755))
	line := `{"type":"user","sessionId":"` + convID + `","cwd":"` + cwd +
		`","message":{"role":"user","content":"` + prompt + `"},"timestamp":"2026-03-01T10:00:00Z"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(projDir, convID+".jsonl"), []byte(line), 0o644))
}

// writeCodexRollout lays down a minimal valid Codex rollout (session_meta +
// turn_context + first user_message) under the date-indexed sessions tree.
func writeCodexRollout(t *testing.T, home, convID, cwd, prompt string) {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "06", "13")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "rollout-2026-06-13T10-00-00-"+convID+".jsonl")
	lines := []string{
		`{"timestamp":"2026-06-13T08:06:09.418Z","type":"session_meta","payload":{"id":"` + convID +
			`","timestamp":"2026-06-13T08:06:05.136Z","cwd":"` + cwd + `"}}`,
		`{"timestamp":"2026-06-13T08:06:09.420Z","type":"turn_context","payload":{"model":"gpt-5.5","cwd":"` + cwd + `"}}`,
		`{"timestamp":"2026-06-13T08:06:09.421Z","type":"event_msg","payload":{"type":"user_message","message":"` + prompt + `"}}`,
	}
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
}

func readFileContents(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func tempOutErr(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	stdout, err := os.CreateTemp(t.TempDir(), "stdout")
	require.NoError(t, err)
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	require.NoError(t, err)
	t.Cleanup(func() { _ = stdout.Close(); _ = stderr.Close() })
	return stdout, stderr
}

// --- the merge helper -------------------------------------------------------

func TestAppendNonClaudeHarnessEntries_AppendsCodexTagsAndSkipsClaude(t *testing.T) {
	home := setupHarnessTestHome(t)
	cwd := filepath.Join(home, "proj")
	writeCodexRollout(t, home, "22222222-2222-2222-2222-222222222222", cwd, "hello from codex")

	// A pre-loaded CC entry the caller already has.
	base := []SessionEntry{{SessionID: "11111111-1111-1111-1111-111111111111", Harness: "claude"}}
	got := appendNonClaudeHarnessEntries(base, cwd)

	require.Len(t, got, 2, "the CC entry is preserved and the Codex conv is appended (CC not re-enumerated)")
	assert.Equal(t, "claude", got[0].Harness, "the pre-loaded CC entry is untouched and first")

	var codex *SessionEntry
	for i := range got {
		if got[i].Harness == "codex" {
			codex = &got[i]
		}
	}
	require.NotNil(t, codex, "a codex-tagged entry was appended")
	assert.Equal(t, "hello from codex", codex.FirstPrompt)
	assert.Equal(t, cwd, codex.ProjectPath)
}

// --- conv ls ----------------------------------------------------------------

func TestRunList_MergesCodexAndClaude(t *testing.T) {
	home := setupHarnessTestHome(t)
	cwd := filepath.Join(home, "proj")
	writeClaudeConv(t, cwd, "11111111-1111-1111-1111-111111111111", "hello from claude")
	writeCodexRollout(t, home, "22222222-2222-2222-2222-222222222222", cwd, "hello from codex")

	stdout, stderr := tempOutErr(t)
	code := RunList(&ListParams{Dir: cwd, SortBy: "modified"}, stdout, stderr)
	require.Equal(t, 0, code)

	out := readFileContents(t, stdout.Name())
	assert.Contains(t, out, "11111111", "claude conv listed")
	assert.Contains(t, out, "22222222", "codex conv listed")
	assert.Contains(t, out, "hello from claude")
	assert.Contains(t, out, "hello from codex")
	assert.Contains(t, strings.ToLower(out), "harness", "the badge column appears for a mixed-harness list")
	assert.Contains(t, out, "codex")
	assert.Contains(t, out, "claude")
}

func TestRunList_ClaudeOnly_NoHarnessColumn(t *testing.T) {
	home := setupHarnessTestHome(t)
	cwd := filepath.Join(home, "proj")
	writeClaudeConv(t, cwd, "11111111-1111-1111-1111-111111111111", "hello from claude")

	stdout, stderr := tempOutErr(t)
	code := RunList(&ListParams{Dir: cwd, SortBy: "modified"}, stdout, stderr)
	require.Equal(t, 0, code)

	out := readFileContents(t, stdout.Name())
	assert.Contains(t, out, "11111111")
	assert.NotContains(t, strings.ToLower(out), "harness", "a CC-only list keeps the original columns")
}

func TestRunList_CodexOnlyDir(t *testing.T) {
	home := setupHarnessTestHome(t)
	cwd := filepath.Join(home, "codexonly") // no Claude project dir at all
	writeCodexRollout(t, home, "33333333-3333-3333-3333-333333333333", cwd, "codex only here")

	stdout, stderr := tempOutErr(t)
	code := RunList(&ListParams{Dir: cwd, SortBy: "modified"}, stdout, stderr)
	require.Equal(t, 0, code, "a missing Claude project dir is no longer fatal")

	out := readFileContents(t, stdout.Name())
	assert.Contains(t, out, "33333333", "the Codex-only dir still lists its conv")
	assert.Contains(t, out, "codex")
}

func TestRunList_EmptyDir_UnifiedNotFound(t *testing.T) {
	home := setupHarnessTestHome(t)
	cwd := filepath.Join(home, "nothing-here")

	stdout, stderr := tempOutErr(t)
	code := RunList(&ListParams{Dir: cwd, SortBy: "modified"}, stdout, stderr)
	require.Equal(t, 0, code, "nothing-anywhere is the unified empty case, exit 0")

	out := readFileContents(t, stdout.Name())
	assert.Contains(t, out, "No conversations found")
}

// A non-canonical --dir (here a trailing separator) must still list Codex
// convs: the dir is canonicalized so the Codex exact-match cwd filter agrees
// with the Claude project-dir encode. Without the fix Codex convs drop.
func TestRunList_NonCanonicalDir(t *testing.T) {
	home := setupHarnessTestHome(t)
	cwd := filepath.Join(home, "proj")
	writeClaudeConv(t, cwd, "11111111-1111-1111-1111-111111111111", "hello from claude")
	writeCodexRollout(t, home, "22222222-2222-2222-2222-222222222222", cwd, "hello from codex")

	stdout, stderr := tempOutErr(t)
	// Trailing separator → filepath.Abs cleans it back to cwd.
	code := RunList(&ListParams{Dir: cwd + string(filepath.Separator), SortBy: "modified"}, stdout, stderr)
	require.Equal(t, 0, code)

	out := readFileContents(t, stdout.Name())
	assert.Contains(t, out, "11111111", "claude conv listed for non-canonical dir")
	assert.Contains(t, out, "22222222", "codex conv still listed for non-canonical dir")
}

// A control char / ANSI escape embedded in an (untrusted) Codex first
// message must be scrubbed before it reaches the terminal.
func TestRunList_ScrubsControlCharsInUntrustedTitle(t *testing.T) {
	home := setupHarnessTestHome(t)
	cwd := filepath.Join(home, "proj")
	// \\u001b[31m is a JSON-escaped ESC + ANSI color sequence.
	writeCodexRollout(t, home, "44444444-4444-4444-4444-444444444444", cwd, "danger\\u001b[31mRED")

	stdout, stderr := tempOutErr(t)
	code := RunList(&ListParams{Dir: cwd, SortBy: "modified"}, stdout, stderr)
	require.Equal(t, 0, code)

	out := readFileContents(t, stdout.Name())
	assert.NotContains(t, out, "\x1b[31m", "the raw ESC sequence must not reach the terminal")
	assert.Contains(t, out, "danger", "the visible text survives the scrub")
}

// --- conv search ------------------------------------------------------------

func TestRunSearch_MergesCodexAndClaude(t *testing.T) {
	home := setupHarnessTestHome(t)
	cwd := filepath.Join(home, "proj")
	writeClaudeConv(t, cwd, "aaaaaaaa-1111-1111-1111-111111111111", "claude needle here")
	writeCodexRollout(t, home, "bbbbbbbb-2222-2222-2222-222222222222", cwd, "codex needle here")

	stdout, stderr := tempOutErr(t)
	code := RunSearch(&SearchParams{Pattern: "needle", Dir: cwd, SortBy: "modified"}, stdout, stderr)
	require.Equal(t, 0, code)

	out := readFileContents(t, stdout.Name())
	assert.Contains(t, out, "aaaaaaaa", "claude match listed")
	assert.Contains(t, out, "bbbbbbbb", "codex match listed")
	assert.Contains(t, strings.ToLower(out), "harness", "the badge column appears in mixed search results")
	assert.Contains(t, out, "2 conversation(s) with matches")
}

func TestRunSearch_CodexMetadataMatch(t *testing.T) {
	home := setupHarnessTestHome(t)
	cwd := filepath.Join(home, "proj")
	writeCodexRollout(t, home, "cccccccc-3333-3333-3333-333333333333", cwd, "unique-codex-token")

	stdout, stderr := tempOutErr(t)
	code := RunSearch(&SearchParams{Pattern: "unique-codex-token", Dir: cwd, SortBy: "modified"}, stdout, stderr)
	require.Equal(t, 0, code)

	out := readFileContents(t, stdout.Name())
	assert.Contains(t, out, "cccccccc", "codex conv matched on its first-prompt metadata")
}
