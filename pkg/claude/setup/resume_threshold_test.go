package setup

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// resumeMinutes returns the configured claude_resume.threshold_minutes, or
// (0,false) when the block / field is absent — the assertion surface for the
// install tests.
func resumeMinutes(t *testing.T) (int, bool) {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	if cfg.ClaudeResume == nil || cfg.ClaudeResume.ThresholdMinutes == nil {
		return 0, false
	}
	return *cfg.ClaudeResume.ThresholdMinutes, true
}

// On a fresh config the override is written with the suppress sentinel, so a
// tclaude-spawned resume never trips Claude Code's "Resume from summary" prompt.
func TestInstallResumeThresholdOverride_WritesDefault(t *testing.T) {
	tempHome(t)

	out := captureStdout(t, func() {
		require.NoError(t, installResumeThresholdOverride())
	})
	assert.Contains(t, out, "✓ Set claude_resume.threshold_minutes")

	got, ok := resumeMinutes(t)
	require.True(t, ok, "threshold_minutes must be written")
	assert.Equal(t, config.ResumeThresholdMinutesSuppress, got)
}

// A value the operator already configured is never overwritten — the install
// is skip-if-set, so a hand-tuned threshold (or an explicit 0 = always show)
// survives untouched.
func TestInstallResumeThresholdOverride_SkipIfSet(t *testing.T) {
	tempHome(t)

	// Pre-seed a deliberate, non-default value.
	cfg := config.DefaultConfig()
	cfg.ClaudeResume = &config.ClaudeResumeConfig{ThresholdMinutes: new(0)}
	require.NoError(t, config.Save(cfg))

	out := captureStdout(t, func() {
		require.NoError(t, installResumeThresholdOverride())
	})
	assert.Contains(t, out, "already set")
	assert.NotContains(t, out, "✓ Set")

	got, ok := resumeMinutes(t)
	require.True(t, ok)
	assert.Equal(t, 0, got, "a configured value must be left unchanged")
}

// Running it twice is idempotent: the second run sees the value the first run
// wrote and leaves it in place.
func TestInstallResumeThresholdOverride_Idempotent(t *testing.T) {
	tempHome(t)

	require.NoError(t, installResumeThresholdOverride())
	require.NoError(t, installResumeThresholdOverride())

	got, ok := resumeMinutes(t)
	require.True(t, ok)
	assert.Equal(t, config.ResumeThresholdMinutesSuppress, got)
}

// A corrupt config file is never clobbered: the installer skips with a warning
// rather than overwriting the operator's unparseable config with defaults.
func TestInstallResumeThresholdOverride_CorruptConfigNotClobbered(t *testing.T) {
	tempHome(t)
	require.NoError(t, os.MkdirAll(config.ConfigDir(), 0o755))
	const garbage = "{ this is not valid json"
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(garbage), 0o644))

	out := captureStdout(t, func() {
		require.NoError(t, installResumeThresholdOverride())
	})
	assert.Contains(t, out, "Skipping")
	assert.NotContains(t, out, "✓ Set")

	// The corrupt file is left exactly as-is — not replaced with a default config.
	data, err := os.ReadFile(config.ConfigPath())
	require.NoError(t, err)
	assert.Equal(t, garbage, string(data), "a corrupt config must not be overwritten")
}

// installExtras wires --install-resume-threshold-override (and --install-all)
// to the installer, and does not run it without a flag.
func TestInstallExtras_ResumeThresholdOverride(t *testing.T) {
	t.Run("no flag is a no-op", func(t *testing.T) {
		tempHome(t)
		require.NoError(t, installExtras(&Params{}))
		_, ok := resumeMinutes(t)
		assert.False(t, ok, "no flag must not write the override")
	})
	t.Run("flag installs it", func(t *testing.T) {
		tempHome(t)
		require.NoError(t, installExtras(&Params{InstallResumeThreshold: true}))
		got, ok := resumeMinutes(t)
		require.True(t, ok)
		assert.Equal(t, config.ResumeThresholdMinutesSuppress, got)
	})
	t.Run("install-all includes it", func(t *testing.T) {
		tempHome(t)
		require.NoError(t, installExtras(&Params{InstallAll: true}))
		got, ok := resumeMinutes(t)
		require.True(t, ok)
		assert.Equal(t, config.ResumeThresholdMinutesSuppress, got)
	})
}
