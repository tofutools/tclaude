package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unit coverage for the Codex dir-trust config editor (JOH-205 inc4 Part
// B). planCodexDirTrust is pure (bytes in → bytes out), so every case is
// exercised without touching a real ~/.codex/config.toml; the file-level
// ensureDirTrustedInFile is driven against a temp file to pin the atomic +
// idempotent write behaviour the operator asked for.

const trustLine = `trust_level = "trusted"`

// parses-as-trusted helper: the planned output must keep the dir trusted.
func assertTrusted(t *testing.T, out []byte, dir string) {
	t.Helper()
	changed, again, err := planCodexDirTrust(out, dir)
	require.NoError(t, err)
	assert.False(t, changed, "re-planning trusted output must be a no-op (idempotent); got:\n%s", out)
	assert.Equal(t, out, again, "no-op plan returns the input bytes unchanged")
}

func TestPlanCodexDirTrust_AddsTableToEmptyConfig(t *testing.T) {
	changed, out, err := planCodexDirTrust(nil, "/home/me/proj")
	require.NoError(t, err)
	require.True(t, changed, "an empty config must gain the trust table")
	s := string(out)
	assert.Contains(t, s, `[projects."/home/me/proj"]`)
	assert.Contains(t, s, trustLine)
	assert.True(t, strings.HasSuffix(s, "\n"), "file ends in exactly one newline")
	assertTrusted(t, out, "/home/me/proj")
}

func TestPlanCodexDirTrust_AppendsAlongsideExistingConfig(t *testing.T) {
	existing := "" +
		"model = \"gpt-5\"  # my pick\n" +
		"\n" +
		"[tui]\n" +
		"status_line = [\"model-with-reasoning\"]\n"
	changed, out, err := planCodexDirTrust([]byte(existing), "/work/repo")
	require.NoError(t, err)
	require.True(t, changed)
	s := string(out)
	// Existing content is preserved verbatim (comments + ordering).
	assert.Contains(t, s, `model = "gpt-5"  # my pick`)
	assert.Contains(t, s, "[tui]")
	assert.Contains(t, s, `status_line = ["model-with-reasoning"]`)
	// Our block is appended.
	assert.Contains(t, s, `[projects."/work/repo"]`)
	assert.Contains(t, s, trustLine)
	assertTrusted(t, out, "/work/repo")
}

func TestPlanCodexDirTrust_AlreadyTrustedIsNoOp(t *testing.T) {
	existing := "[projects.\"/a/b\"]\ntrust_level = \"trusted\"\n"
	changed, out, err := planCodexDirTrust([]byte(existing), "/a/b")
	require.NoError(t, err)
	assert.False(t, changed, "an already-trusted dir needs no change")
	assert.Equal(t, []byte(existing), out, "the input bytes are returned untouched")
}

// Even with an inline comment and surrounding tables, a trusted entry is
// still recognised as a no-op.
func TestPlanCodexDirTrust_AlreadyTrustedWithCommentIsNoOp(t *testing.T) {
	existing := "" +
		"[projects.\"/a/b\"]\n" +
		"trust_level = \"trusted\" # set by codex\n" +
		"\n" +
		"[other]\n" +
		"k = 1\n"
	changed, _, err := planCodexDirTrust([]byte(existing), "/a/b")
	require.NoError(t, err)
	assert.False(t, changed, "a trailing comment must not defeat idempotency")
}

func TestPlanCodexDirTrust_UpgradesNonTrustedValueInPlace(t *testing.T) {
	existing := "" +
		"[projects.\"/a/b\"]\n" +
		"trust_level = \"untrusted\"\n" +
		"some_other = 1\n"
	changed, out, err := planCodexDirTrust([]byte(existing), "/a/b")
	require.NoError(t, err)
	require.True(t, changed, "a non-trusted value must be upgraded")
	s := string(out)
	assert.Contains(t, s, trustLine)
	assert.NotContains(t, s, `"untrusted"`, "the stale value is replaced, not duplicated")
	assert.Contains(t, s, "some_other = 1", "sibling keys in the table are preserved")
	assert.Equal(t, 1, strings.Count(s, `[projects."/a/b"]`), "the table is not duplicated")
	assertTrusted(t, out, "/a/b")
}

func TestPlanCodexDirTrust_InsertsTrustLevelIntoExistingTable(t *testing.T) {
	// The table exists (e.g. some future per-project key) but has no
	// trust_level yet — add it without disturbing the sibling key.
	existing := "[projects.\"/a/b\"]\nsome_key = \"v\"\n"
	changed, out, err := planCodexDirTrust([]byte(existing), "/a/b")
	require.NoError(t, err)
	require.True(t, changed)
	s := string(out)
	assert.Contains(t, s, trustLine)
	assert.Contains(t, s, `some_key = "v"`)
	assert.Equal(t, 1, strings.Count(s, `[projects."/a/b"]`))
	assertTrusted(t, out, "/a/b")
}

// A different project's table must not be mistaken for ours.
func TestPlanCodexDirTrust_DistinguishesDifferentPaths(t *testing.T) {
	existing := "[projects.\"/other\"]\ntrust_level = \"trusted\"\n"
	changed, out, err := planCodexDirTrust([]byte(existing), "/a/b")
	require.NoError(t, err)
	require.True(t, changed, "trusting /a/b must add a new table, not reuse /other's")
	s := string(out)
	assert.Contains(t, s, `[projects."/other"]`)
	assert.Contains(t, s, `[projects."/a/b"]`)
	assert.Equal(t, 2, strings.Count(s, "[projects."), "both project tables coexist")
}

// A `projects = {...}` inline table conflicts with adding a
// [projects."x"] subtable — the editor refuses rather than corrupt.
func TestPlanCodexDirTrust_RefusesConflictingProjectsBinding(t *testing.T) {
	existing := "projects = { foo = 1 }\n"
	changed, out, err := planCodexDirTrust([]byte(existing), "/a/b")
	require.Error(t, err, "an inline `projects =` binding must be refused")
	assert.False(t, changed)
	assert.Nil(t, out)
}

func TestPlanCodexDirTrust_RefusesProjectsArrayOfTables(t *testing.T) {
	existing := "[[projects]]\nname = \"x\"\n"
	_, _, err := planCodexDirTrust([]byte(existing), "/a/b")
	require.Error(t, err, "a [[projects]] array-of-tables must be refused")
}

// A top-level dotted `projects.foo = …` keeps projects a TABLE, which is
// compatible with adding a [projects."dir"] subtable — it must NOT be refused.
func TestPlanCodexDirTrust_DottedProjectsKeyIsNotAConflict(t *testing.T) {
	existing := "projects.scan = true\n"
	changed, out, err := planCodexDirTrust([]byte(existing), "/a/b")
	require.NoError(t, err, "a dotted projects.x key must not be treated as a conflict")
	require.True(t, changed)
	assert.Contains(t, string(out), `[projects."/a/b"]`)
}

// A `projects` key nested in some OTHER table is unrelated (foo.projects) and
// must not block adding a top-level [projects."dir"] subtable.
func TestPlanCodexDirTrust_NestedProjectsKeyIsNotAConflict(t *testing.T) {
	existing := "[foo]\nprojects = 3\n"
	changed, out, err := planCodexDirTrust([]byte(existing), "/a/b")
	require.NoError(t, err, "a `projects` key inside another table is unrelated")
	require.True(t, changed)
	assert.Contains(t, string(out), `[projects."/a/b"]`)
	assert.Contains(t, string(out), "[foo]", "the unrelated table is preserved")
}

func TestPlanCodexDirTrust_RejectsRelativeDirAtFileLayer(t *testing.T) {
	dir := t.TempDir()
	err := ensureDirTrustedInFile(filepath.Join(dir, "config.toml"), "relative/dir")
	require.Error(t, err, "a non-absolute project dir is rejected")
}

// The high-severity case the cold review found: a dir already keyed under
// `projects` in a NON-header form (Codex writes the header form, but a config
// can be hand-edited). Appending a second [projects."dir"] table would produce
// a duplicate key → invalid TOML, so the editor must REFUSE rather than corrupt
// — and must never emit a duplicate.
func TestPlanCodexDirTrust_RefusesDirKeyedUnderPlainProjectsTable(t *testing.T) {
	// [projects] parent table with a dotted "dir".trust_level body key.
	existing := "[projects]\n\"/a/b\".trust_level = \"trusted\"\n"
	changed, out, err := planCodexDirTrust([]byte(existing), "/a/b")
	require.Error(t, err, "dir keyed under a plain [projects] table must be refused, not duplicated")
	assert.False(t, changed)
	assert.Nil(t, out)
}

func TestPlanCodexDirTrust_RefusesDirInPlainProjectsInlineTable(t *testing.T) {
	existing := "[projects]\n\"/a/b\" = { trust_level = \"trusted\" }\n"
	_, _, err := planCodexDirTrust([]byte(existing), "/a/b")
	require.Error(t, err, "dir as an inline table under [projects] must be refused")
}

func TestPlanCodexDirTrust_RefusesDirAsTopLevelDottedKey(t *testing.T) {
	for _, existing := range []string{
		"projects.\"/a/b\".trust_level = \"trusted\"\n",       // dotted sub-key
		"projects.\"/a/b\" = { trust_level = \"trusted\" }\n", // inline table
	} {
		_, _, err := planCodexDirTrust([]byte(existing), "/a/b")
		require.Errorf(t, err, "top-level dotted projects.\"dir\" must be refused; config=%q", existing)
	}
}

// Not over-broad: a plain [projects] table that keys OTHER dirs must NOT block
// adding ours — [projects] + [projects."/a/b"] is valid TOML (distinct keys).
func TestPlanCodexDirTrust_AppendsAlongsidePlainProjectsTableForOtherDir(t *testing.T) {
	existing := "[projects]\n\"/other\".trust_level = \"trusted\"\n"
	changed, out, err := planCodexDirTrust([]byte(existing), "/a/b")
	require.NoError(t, err, "a [projects] table keyed for a different dir must not block ours")
	require.True(t, changed)
	s := string(out)
	assert.Contains(t, s, `[projects."/a/b"]`)
	assert.Contains(t, s, `"/other".trust_level = "trusted"`, "the other dir's entry is preserved")
}

// ensureDirTrustedInFile preserves the existing file mode rather than widening
// it (the cold review's medium finding): a 0600 config stays 0600.
func TestEnsureDirTrustedInFile_PreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfg, []byte("model = \"gpt-5\"\n"), 0o600))

	require.NoError(t, ensureDirTrustedInFile(cfg, "/proj/x"))

	fi, err := os.Stat(cfg)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(),
		"a 0600 config must not be widened by the trust write")
}

// ensureDirTrustedInFile end-to-end: it creates a missing config, is
// atomic (the target only ever appears complete), and is idempotent (a
// second call writes nothing).
func TestEnsureDirTrustedInFile_CreatesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")

	require.NoError(t, ensureDirTrustedInFile(cfg, "/proj/one"))
	b1, err := os.ReadFile(cfg)
	require.NoError(t, err)
	assert.Contains(t, string(b1), `[projects."/proj/one"]`)
	assert.Contains(t, string(b1), trustLine)

	// Second call: idempotent — byte-identical, and no leftover temp files.
	require.NoError(t, ensureDirTrustedInFile(cfg, "/proj/one"))
	b2, err := os.ReadFile(cfg)
	require.NoError(t, err)
	assert.Equal(t, b1, b2, "re-trusting the same dir rewrites nothing")

	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range ents {
		assert.False(t, strings.HasSuffix(e.Name(), ".tmp"),
			"atomic write must leave no temp file behind: %s", e.Name())
	}
}

// A second dir trusted into an existing config preserves the first.
func TestEnsureDirTrustedInFile_AppendsSecondDir(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfg, []byte("model = \"gpt-5\"\n"), 0o644))

	require.NoError(t, ensureDirTrustedInFile(cfg, "/proj/a"))
	require.NoError(t, ensureDirTrustedInFile(cfg, "/proj/b"))

	b, err := os.ReadFile(cfg)
	require.NoError(t, err)
	s := string(b)
	assert.Contains(t, s, `model = "gpt-5"`, "pre-existing config is preserved")
	assert.Contains(t, s, `[projects."/proj/a"]`)
	assert.Contains(t, s, `[projects."/proj/b"]`)
}
