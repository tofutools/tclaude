package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unit coverage for the Claude Code dir-trust config editor (JOH-369).
// planClaudeDirTrust is pure (bytes in → bytes out), so every case is
// exercised without touching a real ~/.claude.json; the file-level
// ensureClaudeDirTrustedInFile is driven against a temp file to pin the atomic
// + idempotent write behaviour and the missing-file create path.

// trustedInParsed reports whether the planned output keeps dir trusted, by
// decoding it back into the projects map — the surface Claude Code reads.
func trustedInParsed(t *testing.T, out []byte, dir string) bool {
	t.Helper()
	var root map[string]any
	require.NoError(t, json.Unmarshal(out, &root), "planned output is valid JSON:\n%s", out)
	projects, ok := root["projects"].(map[string]any)
	require.True(t, ok, "projects is an object")
	entry, ok := projects[dir].(map[string]any)
	require.True(t, ok, "dir has a project entry")
	b, _ := entry["hasTrustDialogAccepted"].(bool)
	return b
}

// assertIdempotent: re-planning trusted output must be a clean no-op.
func assertClaudeIdempotent(t *testing.T, out []byte, dir string) {
	t.Helper()
	changed, again, err := planClaudeDirTrust(out, dir)
	require.NoError(t, err)
	assert.False(t, changed, "re-planning trusted output must be a no-op (idempotent); got:\n%s", out)
	assert.Equal(t, out, again, "no-op plan returns the input bytes unchanged")
}

func TestPlanClaudeDirTrust_CreatesFromEmptyConfig(t *testing.T) {
	changed, out, err := planClaudeDirTrust(nil, "/home/me/.tclaude/scribe")
	require.NoError(t, err)
	require.True(t, changed, "an empty config must gain the trust entry")
	assert.True(t, trustedInParsed(t, out, "/home/me/.tclaude/scribe"))
	assert.Equal(t, byte('\n'), out[len(out)-1], "file ends in a newline")
	assertClaudeIdempotent(t, out, "/home/me/.tclaude/scribe")
}

func TestPlanClaudeDirTrust_AddsEntryPreservingOtherState(t *testing.T) {
	// A realistic-ish config: unrelated top-level state (incl. a LARGE integer
	// that must survive the round-trip byte-exact), plus another project entry
	// that must be preserved.
	const bigTimestamp = 1751626800123 // epoch-ms; float64 would mangle this
	existing := map[string]any{
		"numStartups":            1339,
		"hasCompletedOnboarding": true,
		"cachedGrowthBookAt":     bigTimestamp,
		"nullField":              nil,
		"projects": map[string]any{
			"/work/repo": map[string]any{
				"hasTrustDialogAccepted": true,
				"lastCost":               42,
			},
		},
	}
	data, err := json.Marshal(existing)
	require.NoError(t, err)

	changed, out, err := planClaudeDirTrust(data, "/home/me/.tclaude/scribe")
	require.NoError(t, err)
	require.True(t, changed)

	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	require.NoError(t, dec.Decode(&root))

	// New scribe entry is trusted.
	assert.True(t, trustedInParsed(t, out, "/home/me/.tclaude/scribe"))
	// The pre-existing project entry is untouched.
	projects := root["projects"].(map[string]any)
	other := projects["/work/repo"].(map[string]any)
	assert.Equal(t, true, other["hasTrustDialogAccepted"])
	// Unrelated top-level state survives, and the big integer is EXACT (not a
	// lossy float rewrite) thanks to UseNumber.
	assert.Equal(t, true, root["hasCompletedOnboarding"])
	assert.Equal(t, json.Number("1751626800123"), root["cachedGrowthBookAt"])
	assert.Nil(t, root["nullField"])
	assertClaudeIdempotent(t, out, "/home/me/.tclaude/scribe")
}

func TestPlanClaudeDirTrust_FlipsFalseToTrue(t *testing.T) {
	existing := `{"projects":{"/home/me/.tclaude/scribe":{"hasTrustDialogAccepted":false,"lastDuration":7}}}`
	changed, out, err := planClaudeDirTrust([]byte(existing), "/home/me/.tclaude/scribe")
	require.NoError(t, err)
	require.True(t, changed, "a false entry must be flipped to true")
	assert.True(t, trustedInParsed(t, out, "/home/me/.tclaude/scribe"))
	// A sibling key in the same entry is preserved.
	var root map[string]any
	require.NoError(t, json.Unmarshal(out, &root))
	entry := root["projects"].(map[string]any)["/home/me/.tclaude/scribe"].(map[string]any)
	assert.EqualValues(t, 7, entry["lastDuration"])
	assertClaudeIdempotent(t, out, "/home/me/.tclaude/scribe")
}

func TestPlanClaudeDirTrust_AlreadyTrustedIsNoOp(t *testing.T) {
	existing := `{"projects":{"/x":{"hasTrustDialogAccepted":true}}}`
	changed, out, err := planClaudeDirTrust([]byte(existing), "/x")
	require.NoError(t, err)
	assert.False(t, changed, "already-trusted dir is a no-op")
	assert.Equal(t, []byte(existing), out, "no-op returns input bytes unchanged")
}

func TestPlanClaudeDirTrust_RefusesNonObjectProjects(t *testing.T) {
	existing := `{"projects":"oops"}`
	_, _, err := planClaudeDirTrust([]byte(existing), "/x")
	require.Error(t, err, "a non-object projects must be refused, not corrupted")
	assert.Contains(t, err.Error(), "projects")
}

func TestPlanClaudeDirTrust_RefusesNonObjectEntry(t *testing.T) {
	existing := `{"projects":{"/x":"oops"}}`
	_, _, err := planClaudeDirTrust([]byte(existing), "/x")
	require.Error(t, err, "a non-object project entry must be refused")
	assert.Contains(t, err.Error(), "/x")
}

func TestPlanClaudeDirTrust_RejectsRelativeViaFileHelper(t *testing.T) {
	err := ensureClaudeDirTrustedInFile(filepath.Join(t.TempDir(), ".claude.json"), "relative/dir")
	require.Error(t, err, "a relative project dir is rejected")
	assert.Contains(t, err.Error(), "absolute")
}

func TestEnsureClaudeDirTrustedInFile_CreatesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".claude.json")
	const proj = "/home/me/.tclaude/scribe"

	// (1) Missing file → created with the trust entry, 0600.
	require.NoError(t, ensureClaudeDirTrustedInFile(cfg, proj))
	data, err := os.ReadFile(cfg)
	require.NoError(t, err)
	assert.True(t, trustedInParsed(t, data, proj))
	fi, err := os.Stat(cfg)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "fresh config is 0600")

	// (2) Second call is a no-op that leaves the bytes byte-identical.
	require.NoError(t, ensureClaudeDirTrustedInFile(cfg, proj))
	again, err := os.ReadFile(cfg)
	require.NoError(t, err)
	assert.Equal(t, data, again, "idempotent second call does not rewrite the file")
}

func TestEnsureClaudeDirTrustedInFile_PreservesMode(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".claude.json")
	require.NoError(t, os.WriteFile(cfg, []byte(`{"numStartups":3}`), 0o640))
	require.NoError(t, ensureClaudeDirTrustedInFile(cfg, "/abs/scribe"))
	fi, err := os.Stat(cfg)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), fi.Mode().Perm(), "existing file mode is preserved")
}

// A lone surrogate escape cannot survive Go's decode→encode round-trip (it
// becomes U+FFFD), so the editor refuses rather than silently corrupting the
// operator's config — the same fail-safe posture as a wrong-shape `projects`.
// This matters more now that the trust-dir opt-in is no longer Codex-only and
// so this editor runs on ordinary spawns rather than only a rare scribe summon.
func TestPlanClaudeDirTrust_RefusesLoneSurrogateEscape(t *testing.T) {
	for _, tc := range []struct{ name, config string }{
		{"high surrogate alone", `{"tip":"lone \ud83d surrogate","projects":{}}`},
		{"low surrogate alone", `{"tip":"lone \udc4d surrogate","projects":{}}`},
		{"high followed by a plain escape", `{"tip":"\ud83d\n","projects":{}}`},
		{"high in a project entry value", `{"projects":{"/x":{"note":"\udbff"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, _, err := planClaudeDirTrust([]byte(tc.config), "/work/proj")
			require.Error(t, err, "an unpaired surrogate must be refused, not mangled")
			assert.False(t, changed)
			assert.Contains(t, err.Error(), "surrogate")
		})
	}
}

// ...but a properly PAIRED surrogate escape round-trips fine, so refusing it
// would be over-broad — a config with an escaped emoji must still be editable.
func TestPlanClaudeDirTrust_AllowsPairedSurrogateEscape(t *testing.T) {
	const cfg = `{"tip":"thumbs \ud83d\udc4d up","projects":{}}`
	changed, out, err := planClaudeDirTrust([]byte(cfg), "/work/proj")
	require.NoError(t, err)
	require.True(t, changed)
	assert.True(t, trustedInParsed(t, out, "/work/proj"))

	var root map[string]any
	require.NoError(t, json.Unmarshal(out, &root))
	assert.Equal(t, "thumbs \U0001F44D up", root["tip"], "the pair decodes to the real rune, uncorrupted")
}

// A backslash-escaped backslash followed by "u" is NOT a unicode escape; the
// scanner must consume the pair rather than misreading the tail as one.
func TestPlanClaudeDirTrust_LiteralBackslashUIsNotAnEscape(t *testing.T) {
	const cfg = `{"path":"C:\\ud800\\next","projects":{}}`
	changed, out, err := planClaudeDirTrust([]byte(cfg), "/work/proj")
	require.NoError(t, err, "a literal \\ud800 in a string is not a surrogate escape")
	require.True(t, changed)

	var root map[string]any
	require.NoError(t, json.Unmarshal(out, &root))
	assert.Equal(t, `C:\ud800\next`, root["path"], "the literal text survives verbatim")
}

// Encoding must not rewrite <, > and & as \u003c/\u003e/\u0026 across a file
// tclaude did not author — that is gratuitous churn in a diff the operator may
// well read.
func TestPlanClaudeDirTrust_DoesNotEscapeHTMLCharacters(t *testing.T) {
	const cfg = `{"tip":"use <b> & </b>","projects":{}}`
	changed, out, err := planClaudeDirTrust([]byte(cfg), "/work/proj")
	require.NoError(t, err)
	require.True(t, changed)
	assert.Contains(t, string(out), `"use <b> & </b>"`, "HTML characters are left verbatim")
	assert.NotContains(t, string(out), `\u003c`)
	assert.NotContains(t, string(out), `\u0026`)
}

// A launch that relocates the config dir (tclaude's constructed-root sandbox
// launches set CLAUDE_CONFIG_DIR; see session.ApplyClaudeConfigDirEnv) must
// have its trust entry written into the relocated file — seeding the ambient
// ~/.claude.json would leave the pane parked on the dialog.
func TestEnsureClaudeDirTrustedForLaunch_HonorsClaudeConfigDir(t *testing.T) {
	configDir := t.TempDir()
	getenv := func(name string) string {
		if name == "CLAUDE_CONFIG_DIR" {
			return configDir
		}
		return ""
	}
	require.NoError(t, EnsureClaudeDirTrustedForLaunch(getenv, "/work/proj"))

	out, err := os.ReadFile(filepath.Join(configDir, ClaudeConfigJSONName))
	require.NoError(t, err, "trust entry lands in $CLAUDE_CONFIG_DIR/.claude.json")
	assert.True(t, trustedInParsed(t, out, "/work/proj"))
}

func TestEnsureClaudeDirTrustedForLaunch_RefusesRelativeConfigDir(t *testing.T) {
	getenv := func(name string) string {
		if name == "CLAUDE_CONFIG_DIR" {
			return "relative/dir"
		}
		return ""
	}
	require.Error(t, EnsureClaudeDirTrustedForLaunch(getenv, "/work/proj"))
}

// An empty or absent CLAUDE_CONFIG_DIR is refused: tclaude never edits the
// top-level ~/.claude.json, which its panes do not read. A nil getenv is the
// same case.
func TestEnsureClaudeDirTrustedForLaunch_RefusesWithoutClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for name, getenv := range map[string]func(string) string{
		"nil":   nil,
		"empty": func(string) string { return "" },
		"blank": func(string) string { return "  " },
	} {
		err := EnsureClaudeDirTrustedForLaunch(getenv, "/work/proj")
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "CLAUDE_CONFIG_DIR", name)
	}
	_, statErr := os.Stat(filepath.Join(home, ClaudeConfigJSONName))
	assert.True(t, os.IsNotExist(statErr), "the top-level ~/.claude.json must not be created")
}

// A trust write under a set CLAUDE_CONFIG_DIR lands in that dir and never
// creates or modifies $HOME/.claude.json — not even an existing one.
func TestEnsureClaudeDirTrustedForLaunch_NeverTouchesHomeClaudeJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateRoot := filepath.Join(home, ".claude")
	ambient := filepath.Join(home, ClaudeConfigJSONName)
	before := []byte(`{"numStartups":7}`)
	require.NoError(t, os.WriteFile(ambient, before, 0o600))
	beforeStat, err := os.Stat(ambient)
	require.NoError(t, err)

	getenv := func(name string) string {
		if name == "CLAUDE_CONFIG_DIR" {
			return stateRoot
		}
		return ""
	}
	require.NoError(t, EnsureClaudeDirTrustedForLaunch(getenv, "/work/proj"))

	out, err := os.ReadFile(filepath.Join(stateRoot, ClaudeConfigJSONName))
	require.NoError(t, err)
	assert.True(t, trustedInParsed(t, out, "/work/proj"))
	after, err := os.ReadFile(ambient)
	require.NoError(t, err)
	assert.Equal(t, before, after, "$HOME/.claude.json is untouched")
	afterStat, err := os.Stat(ambient)
	require.NoError(t, err)
	assert.Equal(t, beforeStat.ModTime(), afterStat.ModTime(), "$HOME/.claude.json is not rewritten")
	_, statErr := os.Stat(ambient + ".tclaude.lock")
	assert.True(t, os.IsNotExist(statErr), "no lock sidecar in $HOME")
}

// A symlinked .claude.json (a dotfile manager's) is written through, not
// replaced by a plain file — the same as every other harness's dir-trust seed.
func TestEnsureClaudeDirTrustedInFile_PreservesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "managed.json")
	link := filepath.Join(dir, ".claude.json")
	require.NoError(t, os.WriteFile(target, []byte(`{"numStartups":3}`), 0o640))
	require.NoError(t, os.Symlink(target, link))

	require.NoError(t, ensureClaudeDirTrustedInFile(link, "/abs/proj"))

	fi, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, ".claude.json must remain a symlink")
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.True(t, trustedInParsed(t, data, "/abs/proj"), "the symlink's target carries the trust entry")
	tfi, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), tfi.Mode().Perm(), "target mode is preserved")
}

// Claude Code rewrites .claude.json on nearly every turn and takes no lock.
// A write it makes after tclaude read the file but before the commit must be
// re-planned on top of, not reverted by tclaude's rename.
func TestEnsureClaudeDirTrustedInFile_ReplansOverConcurrentClaudeWrite(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	require.NoError(t, os.WriteFile(cfg, []byte(`{"numStartups":3}`), 0o600))

	prepareCalls := 0
	err := editClaudeConfigFile(cfg, func(data []byte) (bool, []byte, error) {
		return planClaudeDirTrust(data, "/abs/proj")
	}, func(target string, data []byte, perm os.FileMode) (*atomicFileReplacement, error) {
		prepareCalls++
		replacement, err := prepareAtomicWriteFile(target, data, perm)
		if err == nil && prepareCalls == 1 {
			// Claude Code writes after our temp file is staged, before the
			// final stale-read check.
			require.NoError(t, os.WriteFile(cfg, []byte(`{"numStartups":4,"oauthAccount":{"id":"x"}}`), 0o600))
		}
		return replacement, err
	})
	require.NoError(t, err)
	assert.Equal(t, 2, prepareCalls, "the stale read is detected and re-planned once")

	data, err := os.ReadFile(cfg)
	require.NoError(t, err)
	assert.True(t, trustedInParsed(t, data, "/abs/proj"))
	var root map[string]any
	require.NoError(t, json.Unmarshal(data, &root))
	assert.EqualValues(t, 4, root["numStartups"], "Claude Code's concurrent write survives")
	assert.Contains(t, root, "oauthAccount", "Claude Code's concurrent write survives")
}

// Concurrent tclaude spawns seeding DIFFERENT dirs all land; none drops
// another's project entry.
func TestEnsureClaudeDirTrustedInFile_ConcurrentSeedsAllSurvive(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	const seeds = 8
	var wg sync.WaitGroup
	errs := make([]error, seeds)
	for i := range seeds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = ensureClaudeDirTrustedInFile(cfg, fmt.Sprintf("/work/proj-%d", i))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "seed %d", i)
	}
	data, err := os.ReadFile(cfg)
	require.NoError(t, err)
	for i := range seeds {
		assert.True(t, trustedInParsed(t, data, fmt.Sprintf("/work/proj-%d", i)), "seed %d survives", i)
	}
}
