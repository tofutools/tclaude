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

// The per-type notification selector is a friendly view over the
// Transitions list: each canonical NotifyType maps to one wildcard rule
// {from:"*", to:<type>}. NotifyTypeEnabled reads that bit; SetNotifyType
// flips it while preserving every other rule.
func TestNotifyTypeSelector_MapsToWildcardRules(t *testing.T) {
	n := DefaultConfig().Notifications // the five default *→<type> rules

	// Every default type reads as enabled; a never-configured one does not.
	for _, ty := range NotifyTypes {
		assert.True(t, n.NotifyTypeEnabled(ty), "default config notifies on %q", ty)
	}
	assert.False(t, n.NotifyTypeEnabled("working"), "working is not a default notify type")

	// Unchecking "exited" drops only its wildcard rule — the other four
	// types and the rule count both reflect exactly one removal.
	before := len(n.Transitions)
	n.SetNotifyType("exited", false)
	assert.False(t, n.NotifyTypeEnabled("exited"), "exited unchecked")
	assert.Len(t, n.Transitions, before-1, "exactly one rule removed")
	assert.True(t, n.NotifyTypeEnabled("idle"), "siblings untouched")
	assert.True(t, n.NotifyTypeEnabled("error"), "siblings untouched")

	// Re-checking is idempotent: turning it on twice adds a single rule.
	n.SetNotifyType("exited", true)
	n.SetNotifyType("exited", true)
	assert.True(t, n.NotifyTypeEnabled("exited"), "exited re-checked")
	assert.Len(t, n.Transitions, before, "no duplicate wildcard rule")
}

// SetNotifyType must never disturb from-specific or non-canonical rules —
// they belong to the "Advanced" raw editor and round-trip untouched, so
// the checklist and the raw editor can't clobber each other.
func TestSetNotifyType_PreservesAdvancedRules(t *testing.T) {
	n := &NotificationConfig{Transitions: []TransitionRule{
		{From: "working", To: "idle"}, // from-specific: advanced, not the checkbox
		{From: "*", To: "idle"},       // canonical: the "idle" checkbox
		{From: "*", To: "working"},    // non-canonical destination: advanced
	}}

	// The from-specific working→idle rule must NOT light the "idle" box;
	// only the wildcard rule does.
	assert.True(t, n.NotifyTypeEnabled("idle"), "wildcard idle rule present")

	// Uncheck idle: the wildcard idle rule goes, the from-specific and the
	// non-canonical rules stay.
	n.SetNotifyType("idle", false)
	assert.Equal(t, []TransitionRule{
		{From: "working", To: "idle"},
		{From: "*", To: "working"},
	}, n.Transitions, "only the wildcard idle rule removed")
	assert.False(t, n.NotifyTypeEnabled("idle"))
}

// MergeDefaultTypes is additive: it fills in only the missing canonical
// categories, leaves advanced + already-present rules untouched, returns
// what it added in NotifyTypes order, and is idempotent on a full block.
func TestMergeDefaultTypes(t *testing.T) {
	// nil receiver is safe and adds nothing.
	assert.Nil(t, (*NotificationConfig)(nil).MergeDefaultTypes())

	// A full default block has every category already → no-op.
	full := DefaultConfig().Notifications
	assert.Nil(t, full.MergeDefaultTypes(), "default block is already complete")

	// An older/partial block: missing "error" + "exited", plus an advanced
	// rule that must round-trip and must not light a canonical checkbox.
	n := &NotificationConfig{Transitions: []TransitionRule{
		{From: "*", To: "idle"},
		{From: "*", To: "awaiting_permission"},
		{From: "*", To: "awaiting_input"},
		{From: "working", To: "idle"}, // advanced
	}}
	added := n.MergeDefaultTypes()
	assert.Equal(t, []string{"error", "exited"}, added, "added in NotifyTypes order")
	for _, ty := range NotifyTypes {
		assert.True(t, n.NotifyTypeEnabled(ty), "category %q present after merge", ty)
	}
	assert.Contains(t, n.Transitions, TransitionRule{From: "working", To: "idle"},
		"advanced rule preserved")

	// Idempotent: a second merge finds nothing to add.
	assert.Nil(t, n.MergeDefaultTypes(), "second merge is a no-op")
}

// HumanMessagesIntent is the checkbox state (default-on, off only on an
// explicit false) — independent of the master Enabled switch, unlike
// NotifyHumanMessages which ANDs Enabled.
func TestHumanMessagesIntent(t *testing.T) {
	tt := true
	ff := false
	assert.True(t, (&NotificationConfig{}).HumanMessagesIntent(), "unset defaults on")
	assert.True(t, (&NotificationConfig{HumanMessages: &tt}).HumanMessagesIntent())
	assert.False(t, (&NotificationConfig{HumanMessages: &ff}).HumanMessagesIntent())
	// Intent ignores Enabled; NotifyHumanMessages does not.
	disabled := &NotificationConfig{Enabled: false}
	assert.True(t, disabled.HumanMessagesIntent(), "intent is master-independent")
	assert.False(t, disabled.NotifyHumanMessages(), "effective banner needs the master on")
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
		{"clone cooldown unparseable", func(c *Config) { c.Agent = &AgentConfig{CloneCooldown: "soon"} }, "clone_cooldown"},
		{"negative spawn max", func(c *Config) { n := -1; c.Agent = &AgentConfig{SpawnMaxPerHour: &n} }, "spawn_max_per_hour"},
		{"dashboard port too high", func(c *Config) { c.Agent = &AgentConfig{DashboardPort: 70000} }, "dashboard_port"},
		{"dashboard port negative", func(c *Config) { c.Agent = &AgentConfig{DashboardPort: -1} }, "dashboard_port"},
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
		{"bad log rotation size", func(c *Config) {
			c.LogRotation = &LogRotationConfig{MaxSize: "ginormous"}
		}, "log_rotation.max_size"},
		{"negative log rotation keep", func(c *Config) {
			c.LogRotation = &LogRotationConfig{Keep: -3}
		}, "log_rotation.keep"},
		{"remote access enabled without bind", func(c *Config) {
			c.RemoteAccess = &RemoteAccessConfig{Enabled: true}
		}, "remote_access.bind is empty"},
		{"remote access bind without port", func(c *Config) {
			c.RemoteAccess = &RemoteAccessConfig{Enabled: true, Bind: "0.0.0.0"}
		}, "not a valid host:port"},
		{"remote access bind out-of-range port", func(c *Config) {
			c.RemoteAccess = &RemoteAccessConfig{Enabled: true, Bind: "0.0.0.0:99999"}
		}, "numeric port"},
		{"remote access bind zero port", func(c *Config) {
			c.RemoteAccess = &RemoteAccessConfig{Enabled: true, Bind: "0.0.0.0:0"}
		}, "numeric port"},
		{"remote access bind named port", func(c *Config) {
			c.RemoteAccess = &RemoteAccessConfig{Enabled: true, Bind: "0.0.0.0:https"}
		}, "numeric port"},
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

// A fixed dashboard port in range must validate, and 0 (the random-port
// default) must validate too — 0 is "unset", not an out-of-range port.
func TestValidate_DashboardPort(t *testing.T) {
	fixed := DefaultConfig()
	fixed.Agent = &AgentConfig{DashboardPort: 8080}
	assert.Empty(t, Validate(fixed), "a fixed in-range port must validate")

	zero := DefaultConfig()
	zero.Agent = &AgentConfig{DashboardPort: 0}
	assert.Empty(t, Validate(zero), "0 (random port) must validate")
}

// Remote-access bind validation only bites while the listener is ENABLED:
// a complete host:port passes, and a half-typed bind left behind on a
// DISABLED block is tolerated (nothing starts, so it can't block an
// unrelated save) — mirroring the context-nudge "validate-only-when-enabled"
// rule.
func TestValidate_RemoteAccessBind(t *testing.T) {
	valid := DefaultConfig()
	valid.RemoteAccess = &RemoteAccessConfig{Enabled: true, Bind: "0.0.0.0:8443"}
	assert.Empty(t, Validate(valid), "enabled + a valid host:port must validate")

	ipv6 := DefaultConfig()
	ipv6.RemoteAccess = &RemoteAccessConfig{Enabled: true, Bind: "[::]:8443"}
	assert.Empty(t, Validate(ipv6), "an IPv6 host:port must validate")

	disabledMalformed := DefaultConfig()
	disabledMalformed.RemoteAccess = &RemoteAccessConfig{Enabled: false, Bind: "0.0.0.0"}
	assert.Empty(t, Validate(disabledMalformed),
		"a malformed bind while disabled is harmless and must not block a save")
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

// An existing notifications block with an explicitly-empty transitions
// list must stay empty — it is the "notify on no state transition" state
// (every per-type checkbox unchecked, only human-message notifications
// left). Re-seeding the defaults here would make unchecking the last type
// silently revert to all-on. An absent block still gets the defaults.
func TestNormalize_RespectsExplicitEmptyTransitions(t *testing.T) {
	emptied := &Config{Notifications: &NotificationConfig{Enabled: true}}
	Normalize(emptied)
	assert.Empty(t, emptied.Notifications.Transitions,
		"an existing block's empty transitions list is a deliberate 'none', not 'unset'")
	assert.Equal(t, 5, emptied.Notifications.CooldownSeconds, "other defaults still fill in")

	absent := &Config{}
	Normalize(absent)
	assert.NotEmpty(t, absent.Notifications.Transitions,
		"an absent notifications block still seeds the default rules")
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

// A config with no log_rotation block — or a nil config — resolves to
// the built-in defaults (10 MiB, keep 5), so a hand-written config that
// predates the feature keeps working unchanged.
func TestResolvedLogRotation_Defaults(t *testing.T) {
	for _, c := range []*Config{nil, {}, {LogRotation: &LogRotationConfig{}}} {
		maxSize, keep := c.ResolvedLogRotation()
		assert.EqualValues(t, 10*1024*1024, maxSize)
		assert.Equal(t, 5, keep)
	}
}

// Explicit values are parsed: a human-friendly size string via
// common.ParseSize and a positive keep count verbatim.
func TestResolvedLogRotation_ParsesExplicitValues(t *testing.T) {
	c := &Config{LogRotation: &LogRotationConfig{MaxSize: "20MiB", Keep: 8}}
	maxSize, keep := c.ResolvedLogRotation()
	assert.EqualValues(t, 20*1024*1024, maxSize)
	assert.Equal(t, 8, keep)
}

// An explicit max_size of "0" is a valid zero size and disables
// rotation; keep still falls back to its default.
func TestResolvedLogRotation_ExplicitZeroDisables(t *testing.T) {
	c := &Config{LogRotation: &LogRotationConfig{MaxSize: "0"}}
	maxSize, keep := c.ResolvedLogRotation()
	assert.EqualValues(t, 0, maxSize, "max_size 0 disables rotation")
	assert.Equal(t, 5, keep)
}

// An unparseable max_size or a non-positive keep falls back to the
// defaults — Load must never break on a bad value; Validate reports it
// separately for the dashboard editor.
func TestResolvedLogRotation_BadValuesFallBack(t *testing.T) {
	c := &Config{LogRotation: &LogRotationConfig{MaxSize: "not-a-size", Keep: 0}}
	maxSize, keep := c.ResolvedLogRotation()
	assert.EqualValues(t, 10*1024*1024, maxSize, "bad size falls back to the default")
	assert.Equal(t, 5, keep, "keep 0 falls back to the default")
}

// A valid log_rotation block — including the "0" disable form — passes
// validation cleanly.
func TestValidate_AcceptsLogRotation(t *testing.T) {
	for _, lr := range []*LogRotationConfig{
		{MaxSize: "10MiB", Keep: 5},
		{MaxSize: "0"},
		{Keep: 0},
		{},
	} {
		c := DefaultConfig()
		c.LogRotation = lr
		assert.Emptyf(t, Validate(c), "log_rotation %+v should validate", lr)
	}
}

// RaiseOnlyFocus must be nil-safe and default to false (open-on-focus):
// a nil Config, an absent focus block, and an explicit false all mean the
// historical open-on-focus behavior; only focus.raise_only: true flips it.
func TestRaiseOnlyFocus(t *testing.T) {
	var nilCfg *Config
	assert.False(t, nilCfg.RaiseOnlyFocus(), "nil config → false")
	assert.False(t, (&Config{}).RaiseOnlyFocus(), "no focus block → false")
	assert.False(t, (&Config{Focus: &FocusConfig{}}).RaiseOnlyFocus(), "focus block, raise_only unset → false")
	assert.True(t, (&Config{Focus: &FocusConfig{RaiseOnly: true}}).RaiseOnlyFocus(), "raise_only true → true")
}

// focus.raise_only round-trips through the config file, and an absent
// block stays absent (omitempty) so it never shows as a spurious diff.
func TestFocusConfig_RoundTrips(t *testing.T) {
	in := &Config{Focus: &FocusConfig{RaiseOnly: true}}
	data, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"raise_only":true`)

	var out Config
	require.NoError(t, json.Unmarshal(data, &out))
	assert.True(t, out.RaiseOnlyFocus())

	// A default config marshals without a focus key at all.
	none, err := json.Marshal(&Config{})
	require.NoError(t, err)
	assert.NotContains(t, string(none), "focus")
}

// NotifyHumanMessages gates the notify-human OS notification: off unless
// the master switch is on, and within that defaulting ON (nil) but
// suppressible by an explicit false.
func TestNotifyHumanMessages_Gating(t *testing.T) {
	tru, fls := true, false
	cases := []struct {
		name string
		cfg  *NotificationConfig
		want bool
	}{
		{"nil config", nil, false},
		{"disabled, knob unset", &NotificationConfig{Enabled: false}, false},
		{"disabled, knob true", &NotificationConfig{Enabled: false, HumanMessages: &tru}, false},
		{"enabled, knob unset → default on", &NotificationConfig{Enabled: true}, true},
		{"enabled, knob true", &NotificationConfig{Enabled: true, HumanMessages: &tru}, true},
		{"enabled, knob false → suppressed", &NotificationConfig{Enabled: true, HumanMessages: &fls}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.cfg.NotifyHumanMessages())
		})
	}
}

// human_messages round-trips through JSON, and an unset knob stays absent
// (omitempty) so a default config never shows it as a spurious diff.
func TestNotifyHumanMessages_RoundTrips(t *testing.T) {
	fls := false
	data, err := json.Marshal(&Config{Notifications: &NotificationConfig{Enabled: true, HumanMessages: &fls}})
	require.NoError(t, err)
	assert.Contains(t, string(data), `"human_messages":false`)

	var out Config
	require.NoError(t, json.Unmarshal(data, &out))
	assert.False(t, out.Notifications.NotifyHumanMessages())

	// An unset knob marshals without the key at all.
	none, err := json.Marshal(&Config{Notifications: &NotificationConfig{Enabled: true}})
	require.NoError(t, err)
	assert.NotContains(t, string(none), "human_messages")
}

// ResolvedAskProfile applies the built-in default ask profile: a nil
// config / absent block / blank field falls back to the DefaultAsk*
// constants, while a set field is used verbatim — resolved per field, so
// pinning only one keeps the default value for the other (JOH-253).
func TestResolvedAskProfile(t *testing.T) {
	cases := []struct {
		name                  string
		cfg                   *Config
		wantModel, wantEffort string
	}{
		{"nil config", nil, DefaultAskModel, DefaultAskEffort},
		{"absent block", &Config{}, DefaultAskModel, DefaultAskEffort},
		{"empty block", &Config{Ask: &AskConfig{}}, DefaultAskModel, DefaultAskEffort},
		{"both pinned", &Config{Ask: &AskConfig{Model: "opus", Effort: "high"}}, "opus", "high"},
		{"model only → default effort", &Config{Ask: &AskConfig{Model: "haiku"}}, "haiku", DefaultAskEffort},
		{"effort only → default model", &Config{Ask: &AskConfig{Effort: "max"}}, DefaultAskModel, "max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, e := tc.cfg.ResolvedAskProfile()
			assert.Equal(t, tc.wantModel, m, "model")
			assert.Equal(t, tc.wantEffort, e, "effort")
		})
	}
}

// The ask block round-trips through JSON and Save/Load, and an absent
// block stays absent (omitempty) so a default config never shows it as a
// spurious diff in the dashboard's config editor.
func TestAskConfig_RoundTrips(t *testing.T) {
	in := &Config{Ask: &AskConfig{Model: "opus", Effort: "high"}}
	data, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"model":"opus"`)
	assert.Contains(t, string(data), `"effort":"high"`)

	var out Config
	require.NoError(t, json.Unmarshal(data, &out))
	require.NotNil(t, out.Ask)
	m, e := out.ResolvedAskProfile()
	assert.Equal(t, "opus", m)
	assert.Equal(t, "high", e)

	// A default config marshals without an ask key at all.
	none, err := json.Marshal(&Config{})
	require.NoError(t, err)
	assert.NotContains(t, string(none), `"ask"`)

	// Full Save → Load round-trip through ~/.tclaude/config.json.
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, Save(in))
	loaded, err := Load()
	require.NoError(t, err)
	require.NotNil(t, loaded.Ask)
	assert.Equal(t, "opus", loaded.Ask.Model)
	assert.Equal(t, "high", loaded.Ask.Effort)
}

// TestSpawnNameNormalizeEnabled covers the default-on *bool resolver behind
// the auto-normalize-spawn-name feature: nil config, an absent agent block,
// and an absent key all mean ON (any typed name "just works"); only an
// explicit false disables it. The JSON shape is asserted too so a default
// config stays clean (no redundant key) while an explicit-off round-trips.
func TestSpawnNameNormalizeEnabled(t *testing.T) {
	off := false
	on := true

	assert.True(t, (*Config)(nil).SpawnNameNormalizeEnabled(), "nil config → on")
	assert.True(t, (&Config{}).SpawnNameNormalizeEnabled(), "no agent block → on")
	assert.True(t, (&Config{Agent: &AgentConfig{}}).SpawnNameNormalizeEnabled(), "absent key → on")
	assert.True(t, (&Config{Agent: &AgentConfig{SpawnNameNormalize: &on}}).SpawnNameNormalizeEnabled(), "explicit true → on")
	assert.False(t, (&Config{Agent: &AgentConfig{SpawnNameNormalize: &off}}).SpawnNameNormalizeEnabled(), "explicit false → off")

	// A default config omits the key (omitempty + nil pointer).
	clean, err := json.Marshal(&Config{Agent: &AgentConfig{}})
	require.NoError(t, err)
	assert.NotContains(t, string(clean), "spawn_name_normalize")

	// An explicit-off round-trips through Save/Load and stays off.
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, Save(&Config{Agent: &AgentConfig{SpawnNameNormalize: &off}}))
	loaded, err := Load()
	require.NoError(t, err)
	assert.False(t, loaded.SpawnNameNormalizeEnabled(), "explicit false survives round-trip")
}

// TestShowVegasInRegularMode covers the default-OFF *bool resolver behind
// the opt-in that surfaces the Vegas music features outside slop mode: nil
// config, an absent slop block, and an absent key all mean OFF; only an
// explicit true opts in. The JSON shape is asserted too so a default config
// stays clean (no redundant key) while an explicit-on round-trips, and the
// key coexists with the other slop settings (volumes/channel).
func TestShowVegasInRegularMode(t *testing.T) {
	off := false
	on := true

	assert.False(t, (*Config)(nil).ShowVegasInRegularMode(), "nil config → off")
	assert.False(t, (&Config{}).ShowVegasInRegularMode(), "no slop block → off")
	assert.False(t, (&Config{Slop: &SlopConfig{}}).ShowVegasInRegularMode(), "absent key → off")
	assert.False(t, (&Config{Slop: &SlopConfig{VegasInRegularMode: &off}}).ShowVegasInRegularMode(), "explicit false → off")
	assert.True(t, (&Config{Slop: &SlopConfig{VegasInRegularMode: &on}}).ShowVegasInRegularMode(), "explicit true → on")

	// A default (absent) value omits the key (omitempty + nil pointer).
	clean, err := json.Marshal(&Config{Slop: &SlopConfig{}})
	require.NoError(t, err)
	assert.NotContains(t, string(clean), "vegas_in_regular_mode")

	// An explicit-on round-trips through Save/Load alongside a volume so the
	// new key and the existing slop settings coexist.
	t.Setenv("HOME", t.TempDir())
	vol := 80
	require.NoError(t, Save(&Config{Slop: &SlopConfig{VegasInRegularMode: &on, MusicVolume: &vol}}))
	loaded, err := Load()
	require.NoError(t, err)
	assert.True(t, loaded.ShowVegasInRegularMode(), "explicit true survives round-trip")
	music, _ := loaded.ResolvedSlopVolumes()
	assert.Equal(t, 80, music, "the music volume coexists with the new flag")
}

// TestHidePullLever covers the default-OFF *bool resolver behind the opt-out
// that hides the slop-mode side pull-lever: nil config, an absent slop block,
// and an absent key all mean OFF (the lever shows); only an explicit true
// hides it. The JSON shape is asserted too so a default config stays clean
// (no redundant key) while an explicit-on round-trips and coexists with the
// other slop settings.
func TestHidePullLever(t *testing.T) {
	off := false
	on := true

	assert.False(t, (*Config)(nil).HidePullLever(), "nil config → show")
	assert.False(t, (&Config{}).HidePullLever(), "no slop block → show")
	assert.False(t, (&Config{Slop: &SlopConfig{}}).HidePullLever(), "absent key → show")
	assert.False(t, (&Config{Slop: &SlopConfig{HidePullLever: &off}}).HidePullLever(), "explicit false → show")
	assert.True(t, (&Config{Slop: &SlopConfig{HidePullLever: &on}}).HidePullLever(), "explicit true → hide")

	// A default (absent) value omits the key (omitempty + nil pointer).
	clean, err := json.Marshal(&Config{Slop: &SlopConfig{}})
	require.NoError(t, err)
	assert.NotContains(t, string(clean), "hide_pull_lever")

	// An explicit-on round-trips through Save/Load alongside a volume so the
	// new key and the existing slop settings coexist.
	t.Setenv("HOME", t.TempDir())
	vol := 70
	require.NoError(t, Save(&Config{Slop: &SlopConfig{HidePullLever: &on, MusicVolume: &vol}}))
	loaded, err := Load()
	require.NoError(t, err)
	assert.True(t, loaded.HidePullLever(), "explicit true survives round-trip")
	music, _ := loaded.ResolvedSlopVolumes()
	assert.Equal(t, 70, music, "the music volume coexists with the new flag")
}

// TestActivityBotsStyles covers the per-mode style resolvers behind the
// dashboard's activity-bot indicator: regular + wizard default to "emoji",
// slop to "sprites"; nil config / absent block / absent key / an unknown
// value all fall back to those defaults; explicit values win. The JSON shape
// is asserted so a default config stays clean (no block), while a non-default
// trio round-trips through Save/Load.
func TestActivityBotsStyles(t *testing.T) {
	// Defaults: regular + wizard emoji, slop sprites.
	assert.Equal(t, ActivityBotsEmoji, (*Config)(nil).ActivityBotsRegular(), "nil → emoji")
	assert.Equal(t, ActivityBotsSprites, (*Config)(nil).ActivityBotsSlop(), "nil → sprites")
	assert.Equal(t, ActivityBotsEmoji, (*Config)(nil).ActivityBotsWizard(), "nil → emoji")
	assert.Equal(t, ActivityBotsEmoji, (&Config{}).ActivityBotsRegular(), "no block → emoji")
	assert.Equal(t, ActivityBotsSprites, (&Config{}).ActivityBotsSlop(), "no block → sprites")
	assert.Equal(t, ActivityBotsEmoji, (&Config{}).ActivityBotsWizard(), "no block → emoji")
	assert.Equal(t, ActivityBotsEmoji, (&Config{Dashboard: &DashboardConfig{ActivityBots: &ActivityBotsConfig{}}}).ActivityBotsRegular(), "absent key → emoji")
	assert.Equal(t, ActivityBotsEmoji, (&Config{Dashboard: &DashboardConfig{ActivityBots: &ActivityBotsConfig{}}}).ActivityBotsWizard(), "absent key → emoji")

	// An unknown (hand-edited garbage) value degrades to the default.
	garbage := &Config{Dashboard: &DashboardConfig{ActivityBots: &ActivityBotsConfig{Regular: "wat", Slop: "nope", Wizard: "huh"}}}
	assert.Equal(t, ActivityBotsEmoji, garbage.ActivityBotsRegular(), "unknown → emoji")
	assert.Equal(t, ActivityBotsSprites, garbage.ActivityBotsSlop(), "unknown → sprites")
	assert.Equal(t, ActivityBotsEmoji, garbage.ActivityBotsWizard(), "unknown → emoji")

	// Explicit values win, including the cross/off combos. Wizard opts INTO
	// sprites (the whole point of the config knob).
	explicit := &Config{Dashboard: &DashboardConfig{ActivityBots: &ActivityBotsConfig{Regular: ActivityBotsSprites, Slop: ActivityBotsOff, Wizard: ActivityBotsSprites}}}
	assert.Equal(t, ActivityBotsSprites, explicit.ActivityBotsRegular(), "explicit sprites")
	assert.Equal(t, ActivityBotsOff, explicit.ActivityBotsSlop(), "explicit off")
	assert.Equal(t, ActivityBotsSprites, explicit.ActivityBotsWizard(), "explicit wizard sprites")

	// A fresh (all-default) config serializes no dashboard block.
	clean, err := json.Marshal(&Config{})
	require.NoError(t, err)
	assert.NotContains(t, string(clean), "activity_bots")
	assert.NotContains(t, string(clean), "dashboard")

	// A non-default trio survives Save/Load — including wizard opting into the
	// spellcaster sprites.
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, Save(&Config{Dashboard: &DashboardConfig{ActivityBots: &ActivityBotsConfig{Regular: ActivityBotsOff, Slop: ActivityBotsEmoji, Wizard: ActivityBotsSprites}}}))
	loaded, err := Load()
	require.NoError(t, err)
	assert.Equal(t, ActivityBotsOff, loaded.ActivityBotsRegular(), "regular off survives round-trip")
	assert.Equal(t, ActivityBotsEmoji, loaded.ActivityBotsSlop(), "slop emoji survives round-trip")
	assert.Equal(t, ActivityBotsSprites, loaded.ActivityBotsWizard(), "wizard sprites survives round-trip")
}

// TestHScrollFollow covers the dashboard.hscroll_follow resolver: nil config
// / absent block / nil pointer all default to follow (true); only an explicit
// false selects static. A default config marshals no dashboard block, and an
// explicit static survives Save/Load.
func TestHScrollFollow(t *testing.T) {
	bp := func(b bool) *bool { return &b }

	// Default is follow (true) for every "unset" shape.
	assert.True(t, (*Config)(nil).HScrollFollow(), "nil → follow")
	assert.True(t, (&Config{}).HScrollFollow(), "no block → follow")
	assert.True(t, (&Config{Dashboard: &DashboardConfig{}}).HScrollFollow(), "nil pointer → follow")
	assert.True(t, (&Config{Dashboard: &DashboardConfig{HScrollFollow: bp(true)}}).HScrollFollow(), "explicit true → follow")

	// Only an explicit false selects static.
	assert.False(t, (&Config{Dashboard: &DashboardConfig{HScrollFollow: bp(false)}}).HScrollFollow(), "explicit false → static")

	// A fresh (all-default) config serializes no dashboard block / key.
	clean, err := json.Marshal(&Config{})
	require.NoError(t, err)
	assert.NotContains(t, string(clean), "hscroll_follow")
	assert.NotContains(t, string(clean), "dashboard")

	// Explicit static (false) survives Save/Load — the non-default is persisted.
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, Save(&Config{Dashboard: &DashboardConfig{HScrollFollow: bp(false)}}))
	loaded, err := Load()
	require.NoError(t, err)
	assert.False(t, loaded.HScrollFollow(), "static survives round-trip")
}

// TestGroupQuickOptions covers the dashboard.group_quick_options resolver: nil
// config / absent block / absent key / garbage all default to "hover" (fold);
// only an explicit "expanded" opts out. A default config marshals no dashboard
// block, and an explicit "expanded" survives Save/Load.
func TestGroupQuickOptions(t *testing.T) {
	// Default is hover (fold) for every "unset"/garbage shape.
	assert.Equal(t, GroupQuickOptionsHover, (*Config)(nil).GroupQuickOptions(), "nil → hover")
	assert.Equal(t, GroupQuickOptionsHover, (&Config{}).GroupQuickOptions(), "no block → hover")
	assert.Equal(t, GroupQuickOptionsHover, (&Config{Dashboard: &DashboardConfig{}}).GroupQuickOptions(), "absent key → hover")
	assert.Equal(t, GroupQuickOptionsHover, (&Config{Dashboard: &DashboardConfig{GroupQuickOptions: "wat"}}).GroupQuickOptions(), "unknown → hover")
	assert.Equal(t, GroupQuickOptionsHover, (&Config{Dashboard: &DashboardConfig{GroupQuickOptions: GroupQuickOptionsHover}}).GroupQuickOptions(), "explicit hover → hover")

	// Only an explicit "expanded" opts out of folding.
	assert.Equal(t, GroupQuickOptionsExpanded, (&Config{Dashboard: &DashboardConfig{GroupQuickOptions: GroupQuickOptionsExpanded}}).GroupQuickOptions(), "explicit expanded → expanded")

	// A fresh (all-default) config serializes no dashboard block / key.
	clean, err := json.Marshal(&Config{})
	require.NoError(t, err)
	assert.NotContains(t, string(clean), "group_quick_options")

	// Explicit "expanded" survives Save/Load — the non-default is persisted.
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, Save(&Config{Dashboard: &DashboardConfig{GroupQuickOptions: GroupQuickOptionsExpanded}}))
	loaded, err := Load()
	require.NoError(t, err)
	assert.Equal(t, GroupQuickOptionsExpanded, loaded.GroupQuickOptions(), "expanded survives round-trip")
}

// TestMatchSudoOverride_Keying covers the C2 (JOH-324) addition: a sudo
// override may be keyed on the stable `agt_` agent_id (exact or short
// prefix), surviving conv rotation — alongside the pre-existing conv-id
// and title keys, which must keep matching.
func TestMatchSudoOverride_Keying(t *testing.T) {
	const (
		convID  = "abcdef0123456789-conv"
		agentID = "agt_032fdfcfbb0578a5a1cf6493db7264fb" // agt_ + 32 hex
		title   = "my-worker"
	)
	mk := func(overrides map[string]*SudoConfigOverride) *Config {
		return &Config{Agent: &AgentConfig{Sudo: &SudoConfig{Overrides: overrides}}}
	}
	tag := func(s string) *SudoConfigOverride { return &SudoConfigOverride{MaxDuration: s} }

	t.Run("exact agent_id key matches", func(t *testing.T) {
		ov := mk(map[string]*SudoConfigOverride{agentID: tag("hit")}).
			MatchSudoOverride(convID, agentID, title)
		require.NotNil(t, ov)
		assert.Equal(t, "hit", ov.MaxDuration)
	})
	t.Run("short agent_id prefix (12 = agt_ + 8 hex) matches", func(t *testing.T) {
		ov := mk(map[string]*SudoConfigOverride{agentID[:12]: tag("hit")}).
			MatchSudoOverride(convID, agentID, title)
		require.NotNil(t, ov)
		assert.Equal(t, "hit", ov.MaxDuration)
	})
	t.Run("too-short agent_id prefix (<12) does not match", func(t *testing.T) {
		assert.Nil(t, mk(map[string]*SudoConfigOverride{"agt_0123": tag("hit")}).
			MatchSudoOverride(convID, agentID, title))
	})
	t.Run("agent_id key is skipped when caller has no actor", func(t *testing.T) {
		assert.Nil(t, mk(map[string]*SudoConfigOverride{agentID: tag("hit")}).
			MatchSudoOverride(convID, "", title))
	})
	t.Run("well-formed agt_ key for a different agent does not match", func(t *testing.T) {
		// Same shape, different suffix — must not over-match the caller.
		assert.Nil(t, mk(map[string]*SudoConfigOverride{"agt_ffffffff": tag("hit")}).
			MatchSudoOverride(convID, agentID, title))
	})
	t.Run("conv-id key still matches (regression)", func(t *testing.T) {
		ov := mk(map[string]*SudoConfigOverride{convID[:8]: tag("conv")}).
			MatchSudoOverride(convID, agentID, title)
		require.NotNil(t, ov)
		assert.Equal(t, "conv", ov.MaxDuration)
	})
	t.Run("title key still matches (regression)", func(t *testing.T) {
		ov := mk(map[string]*SudoConfigOverride{title: tag("title")}).
			MatchSudoOverride(convID, agentID, title)
		require.NotNil(t, ov)
		assert.Equal(t, "title", ov.MaxDuration)
	})
	t.Run("longest matching key wins", func(t *testing.T) {
		ov := mk(map[string]*SudoConfigOverride{
			agentID[:12]: tag("short"), // 12 chars
			agentID:      tag("full"),  // 36 chars — more specific
		}).MatchSudoOverride(convID, agentID, title)
		require.NotNil(t, ov)
		assert.Equal(t, "full", ov.MaxDuration)
	})
}
