package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// writeResumeConfig points HOME at a temp dir and saves a config whose
// claude_resume.threshold_minutes is the suppress sentinel, so config.Load
// (HOME-relative) inside ApplyClaudeResumeEnv sees a deterministic override.
func writeResumeConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // os.UserHomeDir reads this on Windows
	cfg := config.DefaultConfig()
	cfg.ClaudeResume = &config.ClaudeResumeConfig{ThresholdMinutes: new(config.ResumeThresholdMinutesSuppress)}
	require.NoError(t, config.Save(cfg))
}

func mustResolve(t *testing.T, name string) *harness.Harness {
	t.Helper()
	h, err := harness.Resolve(name)
	require.NoError(t, err)
	return h
}

// For the Claude harness, the configured override is merged into the env so the
// spawned pane never stops on the resume chooser.
func TestApplyClaudeResumeEnv_Claude(t *testing.T) {
	writeResumeConfig(t)

	env := map[string]string{"TCLAUDE_SESSION_ID": "abc"}
	ApplyClaudeResumeEnv(mustResolve(t, harness.DefaultName), env)

	assert.Equal(t, "525600000", env[config.EnvResumeThresholdMinutes])
	assert.Equal(t, "abc", env["TCLAUDE_SESSION_ID"], "existing keys are preserved")
}

// The override is Claude-Code-specific: a Codex harness must leave the env
// untouched (Codex has no such prompt).
func TestApplyClaudeResumeEnv_CodexIsNoOp(t *testing.T) {
	writeResumeConfig(t)

	env := map[string]string{"TCLAUDE_SESSION_ID": "abc"}
	ApplyClaudeResumeEnv(mustResolve(t, harness.CodexName), env)

	_, ok := env[config.EnvResumeThresholdMinutes]
	assert.False(t, ok, "a Codex spawn must not get the Claude resume override")
	assert.Len(t, env, 1, "only the pre-existing key remains")
}

// With no claude_resume block configured, the Claude harness env is left on
// Claude Code's own defaults — nothing is injected.
func TestApplyClaudeResumeEnv_Unconfigured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	require.NoError(t, config.Save(config.DefaultConfig())) // no claude_resume block

	env := map[string]string{}
	ApplyClaudeResumeEnv(mustResolve(t, harness.DefaultName), env)
	assert.Empty(t, env, "an unconfigured override injects nothing")
}

// Defensive: a nil harness or nil env must not panic.
func TestApplyClaudeResumeEnv_NilSafe(t *testing.T) {
	writeResumeConfig(t)
	assert.NotPanics(t, func() { ApplyClaudeResumeEnv(nil, map[string]string{}) })
	assert.NotPanics(t, func() { ApplyClaudeResumeEnv(mustResolve(t, harness.DefaultName), nil) })
}
