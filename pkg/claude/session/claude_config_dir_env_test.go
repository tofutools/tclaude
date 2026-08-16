package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Coverage for the tclaude-wide Claude Code config relocation: the env
// assignment, the once-only seed from the ambient config, and the guard rails
// (never overwrite, symlinks followed but non-regular sources refused, clean
// no-op when there is nothing to seed).

func TestApplyClaudeConfigDirEnv_SeedsAndSetsEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	legacy := filepath.Join(home, harness.ClaudeConfigJSONName)
	require.NoError(t, os.WriteFile(legacy, []byte(`{"oauthAccount":{}}`), 0o600))

	env := map[string]string{}
	require.NoError(t, ApplyClaudeConfigDirEnv(harness.DefaultName, env))

	stateRoot := filepath.Join(home, ".claude")
	assert.Equal(t, stateRoot, env["CLAUDE_CONFIG_DIR"])
	seeded, err := os.ReadFile(filepath.Join(stateRoot, harness.ClaudeConfigJSONName))
	require.NoError(t, err, "legacy config is seeded into the state root")
	assert.Equal(t, `{"oauthAccount":{}}`, string(seeded))
	info, err := os.Stat(filepath.Join(stateRoot, harness.ClaudeConfigJSONName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"seed keeps Claude Code's own conservative mode for this account-adjacent file")
	rootInfo, err := os.Stat(stateRoot)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), rootInfo.Mode().Perm(),
		"a freshly created state root is private, matching layer state preparation")
	entries, err := os.ReadDir(stateRoot)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no seed temp file is left behind")
}

func TestApplyClaudeConfigDirEnv_NoOpForOtherHarnesses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	env := map[string]string{}
	require.NoError(t, ApplyClaudeConfigDirEnv(harness.CodexName, env))
	assert.Empty(t, env)
	_, err := os.Stat(filepath.Join(home, ".claude"))
	assert.True(t, os.IsNotExist(err), "nothing is seeded for a non-Claude harness")
}

// After the first copy the relocated config evolves on its own; a later launch
// must never clobber it with the (by then stale) ambient file.
func TestApplyClaudeConfigDirEnv_NeverOverwritesExistingSeed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	stateRoot := filepath.Join(home, ".claude")
	require.NoError(t, os.MkdirAll(stateRoot, 0o700))
	target := filepath.Join(stateRoot, harness.ClaudeConfigJSONName)
	require.NoError(t, os.WriteFile(target, []byte(`{"evolved":true}`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(home, harness.ClaudeConfigJSONName), []byte(`{"stale":true}`), 0o600))

	env := map[string]string{}
	require.NoError(t, ApplyClaudeConfigDirEnv(harness.DefaultName, env))

	assert.Equal(t, stateRoot, env["CLAUDE_CONFIG_DIR"])
	kept, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, `{"evolved":true}`, string(kept))
}

// A machine with no ambient config has no login to carry over: the env is
// still pinned (the state root is where a completed login can then persist),
// and the onboarding wizard the agent sees is the truth rather than a mount
// bug.
func TestApplyClaudeConfigDirEnv_MissingAmbientIsCleanNoSeed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	env := map[string]string{}
	require.NoError(t, ApplyClaudeConfigDirEnv(harness.DefaultName, env))

	stateRoot := filepath.Join(home, ".claude")
	assert.Equal(t, stateRoot, env["CLAUDE_CONFIG_DIR"])
	_, err := os.Stat(filepath.Join(stateRoot, harness.ClaudeConfigJSONName))
	assert.True(t, os.IsNotExist(err))
	info, err := os.Stat(stateRoot)
	require.NoError(t, err, "the state root itself is still prepared for the login to land in")
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// An operator who already runs ambient claude with their own CLAUDE_CONFIG_DIR
// keeps their real config there — the seed must read from it, not from the
// legacy top-level location that setup never used.
func TestApplyClaudeConfigDirEnv_SeedsFromAmbientConfigDir(t *testing.T) {
	home := t.TempDir()
	ambient := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", ambient)
	require.NoError(t, os.WriteFile(
		filepath.Join(ambient, harness.ClaudeConfigJSONName), []byte(`{"ambient":true}`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(home, harness.ClaudeConfigJSONName), []byte(`{"legacy":true}`), 0o600))

	env := map[string]string{}
	require.NoError(t, ApplyClaudeConfigDirEnv(harness.DefaultName, env))

	stateRoot := filepath.Join(home, ".claude")
	assert.Equal(t, stateRoot, env["CLAUDE_CONFIG_DIR"],
		"the launch is still pinned to the state root, not the operator's ambient dir")
	seeded, err := os.ReadFile(filepath.Join(stateRoot, harness.ClaudeConfigJSONName))
	require.NoError(t, err)
	assert.Equal(t, `{"ambient":true}`, string(seeded))
}

// Dotfile managers commonly keep ~/.claude.json as a symlink; the seed follows
// it as long as it resolves to a regular file.
func TestApplyClaudeConfigDirEnv_FollowsSymlinkedAmbientConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	real := filepath.Join(home, "dotfiles.json")
	require.NoError(t, os.WriteFile(real, []byte(`{"linked":true}`), 0o600))
	require.NoError(t, os.Symlink(real, filepath.Join(home, harness.ClaudeConfigJSONName)))

	env := map[string]string{}
	require.NoError(t, ApplyClaudeConfigDirEnv(harness.DefaultName, env))

	seeded, err := os.ReadFile(
		filepath.Join(home, ".claude", harness.ClaudeConfigJSONName))
	require.NoError(t, err)
	assert.Equal(t, `{"linked":true}`, string(seeded))
}

func TestApplyClaudeConfigDirEnv_RefusesNonRegularAmbientConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	require.NoError(t, os.Mkdir(filepath.Join(home, harness.ClaudeConfigJSONName), 0o700))

	env := map[string]string{}
	err := ApplyClaudeConfigDirEnv(harness.DefaultName, env)
	require.Error(t, err)
	assert.NotContains(t, env, "CLAUDE_CONFIG_DIR",
		"a refused seed must not still relocate the config dir")
}

// The daemon-side scribe pre-trust must land its entry in the relocated file —
// and must seed the login state first, so the trust write can never be the
// creator of a blank config that would then block the seed forever.
func TestPretrustClaudeLaunchDir_SeedsThenTrusts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	require.NoError(t, os.WriteFile(
		filepath.Join(home, harness.ClaudeConfigJSONName),
		[]byte(`{"hasCompletedOnboarding":true}`), 0o600))

	require.NoError(t, PretrustClaudeLaunchDir("/work/proj"))

	out, err := os.ReadFile(filepath.Join(home, ".claude", harness.ClaudeConfigJSONName))
	require.NoError(t, err)
	assert.Contains(t, string(out), `"hasCompletedOnboarding": true`,
		"seeded login state survives the trust write")
	assert.Contains(t, string(out), `"hasTrustDialogAccepted": true`)
	assert.Contains(t, string(out), `"/work/proj"`)
}
