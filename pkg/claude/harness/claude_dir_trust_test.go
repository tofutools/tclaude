package harness

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
