package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A transition into the "error" status must be notify-worthy by
// default — the human wants a desktop notification when an agent's turn
// ends in an API/auth/billing error (Claude Code's StopFailure hook).
func TestDefaultConfig_NotifiesOnErrorTransition(t *testing.T) {
	n := DefaultConfig().Notifications
	n.Enabled = true // MatchesTransition short-circuits on a disabled config

	assert.True(t, n.MatchesTransition("working", "error"),
		"working→error must match a default notification rule")
	assert.True(t, n.MatchesTransition("idle", "error"),
		"the error rule uses a wildcard 'from', so any prior status matches")

	// Sanity: the pre-existing rules still match.
	assert.True(t, n.MatchesTransition("working", "idle"))
	assert.True(t, n.MatchesTransition("working", "exited"))

	// And a transition with no matching rule still does not notify.
	assert.False(t, n.MatchesTransition("idle", "working"),
		"a transition with no matching rule must not notify")
}

// The default config must pass its own validator — the dashboard's
// config editor would otherwise refuse to save a freshly-loaded config.
func TestValidate_AcceptsDefaultConfig(t *testing.T) {
	assert.Empty(t, Validate(DefaultConfig()),
		"DefaultConfig must validate cleanly")
}

// Every nonsensical value the editor can submit must come back as a
// human-readable error mentioning the offending field.
func TestValidate_RejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"bad log level", func(c *Config) { c.LogLevel = "loud" }, "log_level"},
		{"auto-compact too high", func(c *Config) { p := 150; c.AutoCompactPercent = &p }, "auto_compact_percent"},
		{"auto-compact too low", func(c *Config) { p := 0; c.AutoCompactPercent = &p }, "auto_compact_percent"},
		{"clone cooldown unparseable", func(c *Config) { c.Agent = &AgentConfig{CloneCooldown: "soon"} }, "clone_cooldown"},
		{"negative spawn max", func(c *Config) { n := -1; c.Agent = &AgentConfig{SpawnMaxPerHour: &n} }, "spawn_max_per_hour"},
		{"bad sudo duration", func(c *Config) { c.Agent = &AgentConfig{Sudo: &SudoConfig{MaxDuration: "ages"}} }, "sudo.max_duration"},
		{"transition missing to", func(c *Config) {
			c.Notifications = &NotificationConfig{Transitions: []TransitionRule{{From: "idle"}}}
		}, "transitions[0]"},
		{"context nudge out of range", func(c *Config) {
			c.Agent = &AgentConfig{ContextNudge: &ContextNudgeConfig{MinPct: 200}}
		}, "min_pct"},
		{"ratelimit out of range", func(c *Config) {
			c.RateLimit = &RateLimitConfig{FiveHourPercentMaxUsed: 0, SevenDayPercentMaxUsed: 50}
		}, "five_hour_percent_max_used"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			tc.mut(c)
			errs := Validate(c)
			require.NotEmpty(t, errs, "expected a validation error")
			assert.Contains(t, strings.Join(errs, " | "), tc.want)
		})
	}
}

// A clone cooldown of "0" disables the cooldown and must validate — it
// is a legal duration, not a missing value.
func TestValidate_AcceptsZeroCloneCooldown(t *testing.T) {
	c := DefaultConfig()
	c.Agent = &AgentConfig{CloneCooldown: "0"}
	assert.Empty(t, Validate(c))
}

// A 0 in the context-nudge ladder is silently rewritten to a default
// by Resolved(), so Validate must reject it while the nudge is ENABLED
// (the human's 0 would never take effect) but tolerate it while off.
func TestValidate_ContextNudgeZeroLadder(t *testing.T) {
	enabled := DefaultConfig()
	enabled.Agent = &AgentConfig{ContextNudge: &ContextNudgeConfig{Enabled: true, MinPct: 30, IntervalPct: 0}}
	errs := Validate(enabled)
	require.NotEmpty(t, errs, "interval_pct 0 while enabled must be rejected")
	assert.Contains(t, strings.Join(errs, " | "), "interval_pct")

	disabled := DefaultConfig()
	disabled.Agent = &AgentConfig{ContextNudge: &ContextNudgeConfig{Enabled: false, MinPct: 0, IntervalPct: 0}}
	assert.Empty(t, Validate(disabled), "a zero ladder is inert (not an error) while the nudge is off")
}

// Normalize fills the same defaults Load applies, on a bare Config.
func TestNormalize_FillsDefaults(t *testing.T) {
	c := &Config{}
	Normalize(c)
	assert.Equal(t, "info", c.LogLevel)
	require.NotNil(t, c.Notifications, "notifications block must be populated")
	assert.Equal(t, 5, c.Notifications.CooldownSeconds)
	assert.NotEmpty(t, c.Notifications.Transitions)
}

// Normalize must be idempotent — the dashboard editor relies on running
// it once server-side producing the same bytes a later GET re-derives.
func TestNormalize_Idempotent(t *testing.T) {
	c := &Config{LogLevel: "warn", RateLimit: &RateLimitConfig{FiveHourPercentMaxUsed: 150, SevenDayPercentMaxUsed: 80}}
	Normalize(c)
	first, err := json.Marshal(c)
	require.NoError(t, err)
	Normalize(c)
	second, err := json.Marshal(c)
	require.NoError(t, err)
	assert.JSONEq(t, string(first), string(second))
}

// Load must keep normalizing the file it reads — guards the Normalize
// extraction against a regression that drops the defaulting step.
func TestLoad_NormalizesFromFile(t *testing.T) {
	cases := []struct {
		name   string
		file   string
		verify func(*testing.T, *Config)
	}{
		{
			name: "empty object gets log level and notifications",
			file: `{}`,
			verify: func(t *testing.T, c *Config) {
				assert.Equal(t, "info", c.LogLevel)
				require.NotNil(t, c.Notifications)
				assert.Equal(t, 5, c.Notifications.CooldownSeconds)
				assert.NotEmpty(t, c.Notifications.Transitions)
			},
		},
		{
			name: "partial notifications keeps explicit fields, fills the rest",
			file: `{"log_level":"debug","notifications":{"enabled":true}}`,
			verify: func(t *testing.T, c *Config) {
				assert.Equal(t, "debug", c.LogLevel)
				require.NotNil(t, c.Notifications)
				assert.True(t, c.Notifications.Enabled)
				assert.Equal(t, 5, c.Notifications.CooldownSeconds, "zero cooldown defaults to 5")
			},
		},
		{
			name: "out-of-range ratelimit is clamped to defaults",
			file: `{"ratelimit":{"five_hour_percent_max_used":150,"seven_day_percent_max_used":-3}}`,
			verify: func(t *testing.T, c *Config) {
				require.NotNil(t, c.RateLimit)
				assert.Equal(t, 99.0, c.RateLimit.FiveHourPercentMaxUsed)
				assert.Equal(t, 99.9, c.RateLimit.SevenDayPercentMaxUsed)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigHome(t)
			require.NoError(t, os.MkdirAll(ConfigDir(), 0o755))
			require.NoError(t, os.WriteFile(ConfigPath(), []byte(tc.file), 0o644))
			c, err := Load()
			require.NoError(t, err)
			tc.verify(t, c)
		})
	}
}

// isolateConfigHome points the config directory at a fresh temp dir
// for the duration of the test. It sets both HOME and USERPROFILE
// because os.UserHomeDir() — which ConfigDir() relies on — reads
// USERPROFILE on Windows and HOME elsewhere; setting only HOME would
// let a Windows test run touch the real user config.
func isolateConfigHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// Save must write atomically and leave no temp file behind, and a
// second Save over an existing config must succeed (rename-replace).
func TestSave_AtomicAndRepeatable(t *testing.T) {
	isolateConfigHome(t)

	c := DefaultConfig()
	c.LogLevel = "debug"
	require.NoError(t, Save(c))

	got, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "debug", got.LogLevel)

	entries, err := os.ReadDir(ConfigDir())
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp", "Save must not leave a temp file behind")
	}

	c.LogLevel = "warn"
	require.NoError(t, Save(c), "second Save must replace the existing file")
	got, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "warn", got.LogLevel)
}
