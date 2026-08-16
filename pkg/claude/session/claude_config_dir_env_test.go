package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Coverage for the constructed-root Claude Code config relocation: the env
// assignment, the once-only seed from the legacy top-level ~/.claude.json,
// and the guard rails (never overwrite, refuse symlinks, clean no-op when
// there is nothing to seed).

func TestApplyClaudeConfigDirEnv_SeedsAndSetsEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy := filepath.Join(home, harness.ClaudeConfigJSONName)
	require.NoError(t, os.WriteFile(legacy, []byte(`{"oauthAccount":{}}`), 0o600))

	env := map[string]string{}
	require.NoError(t, ApplyClaudeConfigDirEnv(harness.DefaultName, true, env))

	stateRoot := filepath.Join(home, ".claude")
	assert.Equal(t, stateRoot, env["CLAUDE_CONFIG_DIR"])
	seeded, err := os.ReadFile(filepath.Join(stateRoot, harness.ClaudeConfigJSONName))
	require.NoError(t, err, "legacy config is seeded into the state root")
	assert.Equal(t, `{"oauthAccount":{}}`, string(seeded))
	info, err := os.Stat(filepath.Join(stateRoot, harness.ClaudeConfigJSONName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"seed keeps Claude Code's own conservative mode for this account-adjacent file")
	entries, err := os.ReadDir(stateRoot)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no seed temp file is left behind")
}

func TestApplyClaudeConfigDirEnv_NoOpForOtherHarnessesAndRootPostures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for name, apply := range map[string]func(map[string]string) error{
		"host-inherited root": func(env map[string]string) error {
			return ApplyClaudeConfigDirEnv(harness.DefaultName, false, env)
		},
		"non-claude harness": func(env map[string]string) error {
			return ApplyClaudeConfigDirEnv(harness.CodexName, true, env)
		},
	} {
		env := map[string]string{}
		require.NoError(t, apply(env), name)
		assert.Empty(t, env, name)
		_, err := os.Stat(filepath.Join(home, ".claude"))
		assert.True(t, os.IsNotExist(err), "%s: nothing is seeded", name)
	}
}

// After the first copy the relocated config evolves on its own; a later launch
// must never clobber it with the (by then stale) legacy file.
func TestApplyClaudeConfigDirEnv_NeverOverwritesExistingSeed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateRoot := filepath.Join(home, ".claude")
	require.NoError(t, os.MkdirAll(stateRoot, 0o700))
	target := filepath.Join(stateRoot, harness.ClaudeConfigJSONName)
	require.NoError(t, os.WriteFile(target, []byte(`{"evolved":true}`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(home, harness.ClaudeConfigJSONName), []byte(`{"stale":true}`), 0o600))

	env := map[string]string{}
	require.NoError(t, ApplyClaudeConfigDirEnv(harness.DefaultName, true, env))

	assert.Equal(t, stateRoot, env["CLAUDE_CONFIG_DIR"])
	kept, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, `{"evolved":true}`, string(kept))
}

// A machine with no legacy config has no login to carry over: the env is still
// pinned (the state root is where a completed login can then persist), and the
// onboarding wizard the agent sees is the truth rather than a mount bug.
func TestApplyClaudeConfigDirEnv_MissingLegacyIsCleanNoSeed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	env := map[string]string{}
	require.NoError(t, ApplyClaudeConfigDirEnv(harness.DefaultName, true, env))

	stateRoot := filepath.Join(home, ".claude")
	assert.Equal(t, stateRoot, env["CLAUDE_CONFIG_DIR"])
	_, err := os.Stat(filepath.Join(stateRoot, harness.ClaudeConfigJSONName))
	assert.True(t, os.IsNotExist(err))
}

func TestApplyClaudeConfigDirEnv_RefusesSymlinkedLegacyConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	real := filepath.Join(home, "elsewhere.json")
	require.NoError(t, os.WriteFile(real, []byte(`{}`), 0o600))
	require.NoError(t, os.Symlink(real, filepath.Join(home, harness.ClaudeConfigJSONName)))

	env := map[string]string{}
	err := ApplyClaudeConfigDirEnv(harness.DefaultName, true, env)
	require.Error(t, err)
	assert.NotContains(t, env, "CLAUDE_CONFIG_DIR",
		"a refused seed must not still relocate the config dir")
}
