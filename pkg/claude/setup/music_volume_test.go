package setup

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// musicVolume returns the configured slop.music_volume, or (0,false) when the
// block / field is absent — the assertion surface for the install tests.
func musicVolume(t *testing.T) (int, bool) {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	if cfg.Slop == nil || cfg.Slop.MusicVolume == nil {
		return 0, false
	}
	return *cfg.Slop.MusicVolume, true
}

// On a fresh config the music volume is written to the half-volume default, so
// a new install doesn't blast the Vegas/slop + wizard-mode soundtrack at full
// volume on first entry.
func TestInstallDefaultMusicVolume_WritesDefault(t *testing.T) {
	tempHome(t)

	out := captureStdout(t, func() {
		require.NoError(t, installDefaultMusicVolume())
	})
	assert.Contains(t, out, "✓ Set slop.music_volume")

	got, ok := musicVolume(t)
	require.True(t, ok, "music_volume must be written")
	assert.Equal(t, config.DefaultMusicVolume, got)
}

// A value the operator already configured is never overwritten — the install
// is skip-if-set, so a hand-tuned volume (or an explicit 0 = silent) survives
// untouched.
func TestInstallDefaultMusicVolume_SkipIfSet(t *testing.T) {
	tempHome(t)

	// Pre-seed a deliberate, non-default value.
	cfg := config.DefaultConfig()
	cfg.Slop = &config.SlopConfig{MusicVolume: new(90)}
	require.NoError(t, config.Save(cfg))

	out := captureStdout(t, func() {
		require.NoError(t, installDefaultMusicVolume())
	})
	assert.Contains(t, out, "already set")
	assert.NotContains(t, out, "✓ Set")

	got, ok := musicVolume(t)
	require.True(t, ok)
	assert.Equal(t, 90, got, "a configured value must be left unchanged")
}

// An explicit 0 (deliberately silent, distinct from absent) counts as "set"
// and is not bumped up to the default — the pointer, not the value, is the
// skip signal.
func TestInstallDefaultMusicVolume_ExplicitZeroSurvives(t *testing.T) {
	tempHome(t)

	cfg := config.DefaultConfig()
	cfg.Slop = &config.SlopConfig{MusicVolume: new(0)}
	require.NoError(t, config.Save(cfg))

	require.NoError(t, installDefaultMusicVolume())

	got, ok := musicVolume(t)
	require.True(t, ok)
	assert.Equal(t, 0, got, "an explicit silent volume must not be raised to the default")
}

// Running it twice is idempotent: the second run sees the value the first run
// wrote and leaves it in place.
func TestInstallDefaultMusicVolume_Idempotent(t *testing.T) {
	tempHome(t)

	require.NoError(t, installDefaultMusicVolume())
	require.NoError(t, installDefaultMusicVolume())

	got, ok := musicVolume(t)
	require.True(t, ok)
	assert.Equal(t, config.DefaultMusicVolume, got)
}

// A sibling slop setting (e.g. the radio channel) is preserved when the volume
// is written — the installer merges into the existing block rather than
// replacing it.
func TestInstallDefaultMusicVolume_PreservesSiblingSlopSettings(t *testing.T) {
	tempHome(t)

	channel := config.DefaultSlopChannel
	cfg := config.DefaultConfig()
	cfg.Slop = &config.SlopConfig{Channel: &channel} // no MusicVolume yet
	require.NoError(t, config.Save(cfg))

	require.NoError(t, installDefaultMusicVolume())

	loaded, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, loaded.Slop)
	require.NotNil(t, loaded.Slop.MusicVolume)
	assert.Equal(t, config.DefaultMusicVolume, *loaded.Slop.MusicVolume)
	require.NotNil(t, loaded.Slop.Channel, "the sibling channel must survive the volume write")
	assert.Equal(t, channel, *loaded.Slop.Channel)
}

// A corrupt config file is never clobbered: the installer skips with a warning
// rather than overwriting the operator's unparseable config with defaults.
func TestInstallDefaultMusicVolume_CorruptConfigNotClobbered(t *testing.T) {
	tempHome(t)
	require.NoError(t, os.MkdirAll(config.DataDir(), 0o700))
	const garbage = "{ this is not valid json"
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(garbage), 0o644))

	out := captureStdout(t, func() {
		require.NoError(t, installDefaultMusicVolume())
	})
	assert.Contains(t, out, "Skipping")
	assert.NotContains(t, out, "✓ Set")

	// The corrupt file is left exactly as-is — not replaced with a default config.
	data, err := os.ReadFile(config.ConfigPath())
	require.NoError(t, err)
	assert.Equal(t, garbage, string(data), "a corrupt config must not be overwritten")
}
