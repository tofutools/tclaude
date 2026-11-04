package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
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
	// The parity assertion: Claude Code, Codex and Copilot all block on a
	// trust dialog and all expose a seedable trust record, so the opt-in must
	// be accepted for each. A regression that re-narrows this to Codex fails
	// here.
	for _, name := range []string{DefaultName, CodexName, CopilotName} {
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
	assert.Equal(t, "~/.claude/.claude.json", DirTrustStore(claude))

	codex, err := Resolve(CodexName)
	require.NoError(t, err)
	assert.Equal(t, "~/.codex/config.toml", DirTrustStore(codex))

	// Copilot's store is named relative to COPILOT_HOME on purpose: a profile
	// can relocate it, so consent copy naming ~/.copilot would be wrong for
	// exactly the operator who moved it.
	copilot, err := Resolve(CopilotName)
	require.NoError(t, err)
	assert.Equal(t, "$COPILOT_HOME/config.json", DirTrustStore(copilot))

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
	assert.NoError(t, EnsureDirTrustedForLaunch(h, "/some/abs/dir", nil, ""))
	assert.NoError(t, EnsureDirTrustedForLaunch(nil, "/some/abs/dir", nil, ""))
}

func TestEnsureDirTrusted_RejectsRelativeDirForBothHarnesses(t *testing.T) {
	// The absolute-path requirement is load-bearing: each harness keys its
	// trust record on the resolved launch cwd, so a relative path would write
	// an entry that never matches. Asserted through the dispatch so neither
	// branch can lose the check.
	for _, name := range []string{DefaultName, CodexName, CopilotName} {
		h, err := Resolve(name)
		require.NoError(t, err)
		assert.Error(t, EnsureDirTrustedForLaunch(h, "relative/dir", claudeConfigDirEnv(t.TempDir()), ""), "%s must reject a relative dir", name)
	}
}

// claudeConfigDirEnv is a launch getenv carrying only CLAUDE_CONFIG_DIR, the
// way every tclaude Claude launch carries it (session.ApplyClaudeConfigDirEnv).
func claudeConfigDirEnv(dir string) func(string) string {
	return func(name string) string {
		if name == "CLAUDE_CONFIG_DIR" {
			return dir
		}
		return ""
	}
}

// EnsureDirTrustedForLaunch dispatches to the COPILOT editor, driven against a temp HOME
// so no real Copilot config is touched.
func TestEnsureDirTrusted_DispatchesToCopilotEditor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(CopilotHomeEnvVar, "")
	const dir = "/work/proj"

	h, err := Resolve(CopilotName)
	require.NoError(t, err)
	require.NoError(t, EnsureDirTrustedForLaunch(h, dir, nil, ""))

	data, err := os.ReadFile(filepath.Join(home, ".copilot", CopilotConfigFileName))
	require.NoError(t, err, "the copilot store was created")
	var root struct {
		TrustedFolders []string `json:"trustedFolders"`
	}
	require.NoError(t, json.Unmarshal(data, &root))
	assert.Equal(t, []string{dir}, root.TrustedFolders)

	// Dispatch, not "write them all".
	_, statErr := os.Stat(filepath.Join(home, ".claude.json"))
	assert.True(t, os.IsNotExist(statErr))
	_, statErr = os.Stat(filepath.Join(home, ".codex", "config.toml"))
	assert.True(t, os.IsNotExist(statErr))
}

// The launch-aware entry point is what a spawn whose profile relocates
// COPILOT_HOME goes through; the two fixed-path harnesses must ignore the
// environment entirely rather than accidentally keying off it.
func TestEnsureDirTrustedForLaunch_UsesTheLaunchCopilotHome(t *testing.T) {
	ambient := t.TempDir()
	t.Setenv("HOME", ambient)
	relocated := filepath.Join(t.TempDir(), "launch-home")
	getenv := func(name string) string {
		if name == CopilotHomeEnvVar {
			return relocated
		}
		return ""
	}

	copilot, err := Resolve(CopilotName)
	require.NoError(t, err)
	require.NoError(t, EnsureDirTrustedForLaunch(copilot, "/work/proj", getenv, ambient))
	_, statErr := os.Stat(filepath.Join(relocated, CopilotConfigFileName))
	assert.NoError(t, statErr, "the launch's COPILOT_HOME must carry the entry")

	// Claude Code's store is the launch's CLAUDE_CONFIG_DIR. A launch
	// environment without it is refused, never resolved to the top-level
	// ~/.claude.json.
	claude, err := Resolve(DefaultName)
	require.NoError(t, err)
	require.Error(t, EnsureDirTrustedForLaunch(claude, "/work/proj", getenv, ambient))
	_, statErr = os.Stat(filepath.Join(ambient, ".claude.json"))
	assert.True(t, os.IsNotExist(statErr), "the top-level ~/.claude.json must not be created")
}

// EnsureDirTrustedForLaunch dispatches to the CLAUDE editor for the Claude
// harness, writing the launch's <state root>/.claude.json — driven against a
// temp HOME so no real config is touched — and never the top-level
// $HOME/.claude.json.
func TestEnsureDirTrusted_DispatchesToClaudeEditor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const dir = "/work/proj"
	stateRoot := filepath.Join(home, ".claude")

	h, err := Resolve(DefaultName)
	require.NoError(t, err)
	require.NoError(t, EnsureDirTrustedForLaunch(h, dir, claudeConfigDirEnv(stateRoot), ""))

	data, err := os.ReadFile(filepath.Join(stateRoot, ".claude.json"))
	require.NoError(t, err, "the claude store in the state root was created, not the codex one")
	var root struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal(data, &root))
	assert.True(t, root.Projects[dir].HasTrustDialogAccepted)
	_, statErr := os.Stat(filepath.Join(home, ".claude.json"))
	assert.True(t, os.IsNotExist(statErr), "the top-level ~/.claude.json must not be created")
	_, statErr = os.Stat(filepath.Join(home, ".claude.json.tclaude.lock"))
	assert.True(t, os.IsNotExist(statErr), "the lock sidecar must not land in $HOME")
	_, statErr = os.Stat(filepath.Join(stateRoot, ".claude.json.tclaude.lock"))
	assert.NoError(t, statErr, "the lock sidecar sits beside the resolved file, in the state root")

	// The Codex store must be untouched — dispatch, not both.
	_, statErr = os.Stat(filepath.Join(home, ".codex", "config.toml"))
	assert.True(t, os.IsNotExist(statErr), "claude dispatch must not write the codex config")
}

// ...and to the CODEX editor for the Codex harness, with the same isolation.
func TestEnsureDirTrusted_DispatchesToCodexEditor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const dir = "/work/proj"

	h, err := Resolve(CodexName)
	require.NoError(t, err)
	require.NoError(t, EnsureDirTrustedForLaunch(h, dir, nil, ""))

	data, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	require.NoError(t, err, "the codex store was created, not the claude one")
	assert.Contains(t, string(data), `[projects."`+dir+`"]`)
	assert.Contains(t, string(data), `trust_level = "trusted"`)

	_, statErr := os.Stat(filepath.Join(home, ".claude.json"))
	assert.True(t, os.IsNotExist(statErr), "codex dispatch must not write the claude config")
}

func TestEnsureDirTrustedForLaunch_SymlinkedCwd(t *testing.T) {
	for _, name := range []string{DefaultName, CodexName, CopilotName, GeminiName} {
		t.Run(name, func(t *testing.T) {
			root := testutil.CanonicalTempDir(t)
			home := filepath.Join(root, "home")
			real := filepath.Join(root, "project")
			alias := filepath.Join(root, "project-link")
			require.NoError(t, os.MkdirAll(home, 0700))
			require.NoError(t, os.MkdirAll(real, 0700))
			require.NoError(t, os.Symlink(real, alias))
			t.Setenv("HOME", home)
			h, err := Resolve(name)
			require.NoError(t, err)
			require.NoError(t, EnsureDirTrustedForLaunch(h, alias, claudeConfigDirEnv(filepath.Join(home, ".claude")), home))
			var path string
			switch name {
			case DefaultName:
				path = filepath.Join(home, ".claude", ".claude.json")
			case CodexName:
				path = filepath.Join(home, ".codex", "config.toml")
			case CopilotName:
				path = filepath.Join(home, ".copilot", "config.json")
			case GeminiName:
				path = filepath.Join(home, ".gemini", "trustedFolders.json")
			}
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			for _, cwd := range []string{real, alias} {
				encoded, err := json.Marshal(cwd)
				require.NoError(t, err)
				assert.Contains(t, string(data), string(encoded), "trust must match both physical and recorded cwd")
			}
		})
	}
}

func TestEnsureDirTrustedForLaunch_SymlinkedGeminiDistrustIsAtomic(t *testing.T) {
	for _, denied := range []string{"project", "project-link"} {
		t.Run(denied, func(t *testing.T) {
			root := testutil.CanonicalTempDir(t)
			real, alias := filepath.Join(root, "project"), filepath.Join(root, "project-link")
			require.NoError(t, os.Mkdir(real, 0700))
			require.NoError(t, os.Symlink(real, alias))
			home := filepath.Join(root, "home")
			path := filepath.Join(home, ".gemini", "trustedFolders.json")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
			before, err := json.Marshal(map[string]string{filepath.Join(root, denied): "DO_NOT_TRUST"})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, before, 0600))
			h, err := Resolve(GeminiName)
			require.NoError(t, err)
			err = EnsureDirTrustedForLaunch(h, alias, func(string) string { return "" }, home)
			require.ErrorContains(t, err, "DO_NOT_TRUST")
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after, "refused trust must not alter either spelling")
		})
	}
}
