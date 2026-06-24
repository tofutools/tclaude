package conv

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// resumeLaunchCmd injects the configured CLAUDE_CODE_RESUME_* overrides so the
// watch-mode resume doesn't trip Claude Code's "Resume from summary" chooser —
// the prompt that hangs a scripted/detached resume. The override lives in
// tclaude's own config.json (never ~/.claude/settings.json) and is
// Claude-Code-specific, so it must ride a Claude resume command and be absent
// from a Codex one.

// withResumeConfig points HOME at a temp dir and writes a config whose
// claude_resume.threshold_minutes is the suppress sentinel, so config.Load
// (HOME-relative) inside resumeLaunchCmd sees a deterministic override.
func withResumeConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // os.UserHomeDir reads this on Windows
	cfg := config.DefaultConfig()
	cfg.ClaudeResume = &config.ClaudeResumeConfig{ThresholdMinutes: new(config.ResumeThresholdMinutesSuppress)}
	require.NoError(t, config.Save(cfg))
}

// A Claude resume carries the configured threshold as an exported env var, so
// the spawned `claude --resume` never shows the chooser.
func TestResumeLaunchCmd_InjectsResumeOverrideForClaude(t *testing.T) {
	withResumeConfig(t)

	cmd, h, err := resumeLaunchCmd("claude", resumeConvClaude[:8], resumeConvClaude, nil)
	require.NoError(t, err)
	require.Equal(t, "claude", h.Name)

	assert.Contains(t, cmd, "CLAUDE_CODE_RESUME_THRESHOLD_MINUTES=525600000",
		"the configured override must be exported onto the Claude resume command")
}

// The override is Claude-Code-specific: a Codex resume must NOT carry it (Codex
// has no such prompt and the env var is meaningless there).
func TestResumeLaunchCmd_NoResumeOverrideForCodex(t *testing.T) {
	withResumeConfig(t)

	cmd, h, err := resumeLaunchCmd("codex", resumeConvCodex[:8], resumeConvCodex, nil)
	require.NoError(t, err)
	require.Equal(t, "codex", h.Name)

	assert.NotContains(t, cmd, "CLAUDE_CODE_RESUME_THRESHOLD_MINUTES=525600000",
		"a Codex resume must not get the Claude-specific resume override")
}

// With no claude_resume block configured, a Claude resume stays on Claude
// Code's own defaults — tclaude injects nothing.
func TestResumeLaunchCmd_NoOverrideWhenUnconfigured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	require.NoError(t, config.Save(config.DefaultConfig())) // no claude_resume block

	cmd, _, err := resumeLaunchCmd("claude", resumeConvClaude[:8], resumeConvClaude, nil)
	require.NoError(t, err)

	assert.NotContains(t, cmd, "CLAUDE_CODE_RESUME_THRESHOLD_MINUTES=525600000",
		"an unconfigured override must not inject the suppress sentinel")
}
