package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unit coverage for the harness-agnostic dir-trust layer: the ResolveTrustDir
// gate, the EnsureDirTrusted dispatch, and DirTrustStore. The per-harness
// editors are covered exhaustively in codex_dir_trust_test.go /
// claude_dir_trust_test.go; what matters here is that the right editor is
// reached for the right harness, and that a harness with no trust dialog is
// refused rather than silently handed a config file to edit.

func TestResolveTrustDir_UnsetAlwaysPasses(t *testing.T) {
	// An unrequested flag passes for EVERY harness, including ones with no
	// trust dialog — false is the universal default, so a caller that never
	// touched the checkbox must not trip the gate.
	for _, name := range Names() {
		h, err := Resolve(name)
		require.NoError(t, err)
		got, err := ResolveTrustDir(h, false)
		require.NoError(t, err, "unset trust-dir must pass for %s", name)
		assert.False(t, got)
	}
	// Nil (unresolvable harness) is the same story.
	got, err := ResolveTrustDir(nil, false)
	require.NoError(t, err)
	assert.False(t, got)
}

func TestResolveTrustDir_AcceptedForBothTrustDialogHarnesses(t *testing.T) {
	// The parity assertion: Claude Code and Codex both block on a trust dialog
	// and both expose a seedable trust record, so the opt-in must be accepted
	// for both. A regression that re-narrows this to Codex fails here.
	for _, name := range []string{DefaultName, CodexName} {
		h, err := Resolve(name)
		require.NoError(t, err)
		require.True(t, h.SupportsDirTrust(), "%s must declare dir trust", name)

		got, err := ResolveTrustDir(h, true)
		require.NoError(t, err, "trust-dir must be accepted for %s", name)
		assert.True(t, got)
	}
}

func TestResolveTrustDir_RejectedForHarnessWithoutTrustDialog(t *testing.T) {
	// A harness with no trust dialog gets an ERROR, not a silently dropped
	// flag — the operator asked for a side effect that cannot happen, and a
	// quiet no-op would hide the mistake.
	h, err := Resolve(OpenCodeName)
	require.NoError(t, err)
	require.False(t, h.SupportsDirTrust())

	got, err := ResolveTrustDir(h, true)
	require.Error(t, err)
	assert.False(t, got)
	assert.Contains(t, err.Error(), OpenCodeName, "the error names the offending harness")
}

func TestResolveTrustDir_RejectedForNilHarness(t *testing.T) {
	got, err := ResolveTrustDir(nil, true)
	require.Error(t, err)
	assert.False(t, got)
}

func TestDirTrustStore_NamesEachHarnessStore(t *testing.T) {
	// The UI copy ("edits ~/.claude.json") derives from this, so pin the exact
	// spellings — a wrong path here would ask the operator to consent to an
	// edit of a file tclaude is not about to touch.
	claude, err := Resolve(DefaultName)
	require.NoError(t, err)
	assert.Equal(t, "~/.claude.json", DirTrustStore(claude))

	codex, err := Resolve(CodexName)
	require.NoError(t, err)
	assert.Equal(t, "~/.codex/config.toml", DirTrustStore(codex))

	opencode, err := Resolve(OpenCodeName)
	require.NoError(t, err)
	assert.Empty(t, DirTrustStore(opencode), "a harness with no dir trust names no store")
	assert.Empty(t, DirTrustStore(nil))
}

// DirTrustStore must be defined for exactly the harnesses that declare
// DirTrust: a store with no editor (or an editor with no store) is the drift
// EnsureDirTrusted's default branch would turn into a runtime error.
func TestDirTrustStore_DefinedExactlyWhenDirTrustIsDeclared(t *testing.T) {
	for _, name := range Names() {
		h, err := Resolve(name)
		require.NoError(t, err)
		assert.Equal(t, h.SupportsDirTrust(), DirTrustStore(h) != "",
			"%s: DirTrust=%v but store=%q", name, h.SupportsDirTrust(), DirTrustStore(h))
	}
}

func TestEnsureDirTrusted_NoOpForHarnessWithoutTrustDialog(t *testing.T) {
	// A caller that skipped ResolveTrustDir must not be able to write a config
	// file for a harness that has none. Nil is the same case.
	h, err := Resolve(OpenCodeName)
	require.NoError(t, err)
	assert.NoError(t, EnsureDirTrusted(h, "/some/abs/dir"))
	assert.NoError(t, EnsureDirTrusted(nil, "/some/abs/dir"))
}

func TestEnsureDirTrusted_RejectsRelativeDirForBothHarnesses(t *testing.T) {
	// The absolute-path requirement is load-bearing: each harness keys its
	// trust record on the resolved launch cwd, so a relative path would write
	// an entry that never matches. Asserted through the dispatch so neither
	// branch can lose the check.
	for _, name := range []string{DefaultName, CodexName} {
		h, err := Resolve(name)
		require.NoError(t, err)
		assert.Error(t, EnsureDirTrusted(h, "relative/dir"), "%s must reject a relative dir", name)
	}
}

// EnsureDirTrusted dispatches to the CLAUDE editor for the Claude harness —
// driven against a temp HOME so the real ~/.claude.json is never touched.
func TestEnsureDirTrusted_DispatchesToClaudeEditor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const dir = "/work/proj"

	h, err := Resolve(DefaultName)
	require.NoError(t, err)
	require.NoError(t, EnsureDirTrusted(h, dir))

	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	require.NoError(t, err, "the claude store was created, not the codex one")
	var root struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal(data, &root))
	assert.True(t, root.Projects[dir].HasTrustDialogAccepted)

	// The Codex store must be untouched — dispatch, not both.
	_, statErr := os.Stat(filepath.Join(home, ".codex", "config.toml"))
	assert.True(t, os.IsNotExist(statErr), "claude dispatch must not write the codex config")
}

// ...and to the CODEX editor for the Codex harness, with the same isolation.
func TestEnsureDirTrusted_DispatchesToCodexEditor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const dir = "/work/proj"

	h, err := Resolve(CodexName)
	require.NoError(t, err)
	require.NoError(t, EnsureDirTrusted(h, dir))

	data, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	require.NoError(t, err, "the codex store was created, not the claude one")
	assert.Contains(t, string(data), `[projects."`+dir+`"]`)
	assert.Contains(t, string(data), `trust_level = "trusted"`)

	_, statErr := os.Stat(filepath.Join(home, ".claude.json"))
	assert.True(t, os.IsNotExist(statErr), "codex dispatch must not write the claude config")
}
