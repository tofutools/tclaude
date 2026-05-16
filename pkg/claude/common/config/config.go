// Package config provides configuration loading for tclaude.
package config

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
)

// Config represents the tclaude configuration file structure.
type Config struct {
	Notifications      *NotificationConfig `json:"notifications,omitempty"`
	AutoCompactPercent *int                `json:"auto_compact_percent,omitempty"`
	LogLevel           string              `json:"log_level,omitempty"`
	RecordHooks        bool                `json:"record_hooks,omitempty"`
	RateLimit          *RateLimitConfig    `json:"ratelimit,omitempty"`
	Agent              *AgentConfig        `json:"agent,omitempty"`

	// Terminal names the terminal emulator the agentd dashboard's
	// spawn auto-focus / shell-attach feature should open — "ghostty",
	// "kitty", "wezterm", "alacritty", "iterm2", "gnome-terminal", … .
	// Empty means auto-detect, which prefers a hand-installed modern
	// terminal over the OS default. This is the middle tier of the
	// terminal-selection priority: the `tclaude agentd serve
	// --terminal` flag overrides it; auto-detect is the fallback.
	Terminal string `json:"terminal,omitempty"`
}

// AgentConfig holds agent-coordination knobs (see agents_todo.md).
//
// DefaultPermissions are granted to every agent — baseline trust the
// human curates by hand. Per-agent overrides used to live here too,
// but moved to SQLite (table agent_permissions) in v9: the daemon
// rewrites them through grant/revoke endpoints, and storing them in
// JSON made round-tripping awkward (config.json is hand-edited for
// log_level etc.). config keeps only what humans naturally write.
//
// Permission slugs are simple dotted strings, e.g. "self.rename",
// "member.redesignate", "agent.spawn". Unknown slugs are ignored
// (forward-compat: a user grants a permission a future build wires up).
//
// Sudo carries the human-curated defaults for `tclaude agent sudo`
// (time-bounded permission elevations). Hand-written; the daemon
// reads but never rewrites it. Empty fields fall back to the agentd
// hardcoded defaults. Per-conv overrides via Sudo.Overrides[] use
// selector-shaped keys (conv-id / title, with prefix match against
// title and conv-id) the historical permission_overrides block did.
//
// AutoLaunchDashboard, when true, makes `tclaude agentd serve` open the
// browser dashboard on startup — the persistent twin of the
// --auto-launch-dashboard serve flag. The flag and this field OR
// together: either one turns it on, so an autostart/service launch can
// opt in without carrying the flag.
//
// CloneCooldown is the minimum cooldown between two clones of the same
// source agent — a Go duration string ("1m", "30s"). It is the
// persistent twin of the `tclaude agentd serve --agent-clone-cooldown`
// flag, which overrides it when set; the built-in default is 1m. "0"
// disables the cooldown. An unparseable value is warned about and
// ignored, falling through to the flag/default. The cooldown applies
// only to agent-initiated clones — human-initiated clones (CLI or
// dashboard) are never rate-limited.
//
// SpawnGroupRestriction / SpawnAllowedGroups / SpawnMaxPerHour are the
// global knobs of the agent-spawn guardrail layer — runaway-prevention
// for the case where the human grants an AGENT the `groups.spawn`
// permission. They only ever affect agent callers; a human (no claude
// ancestor) bypasses every spawn guardrail, as everywhere else.
//
//   - SpawnGroupRestriction toggles the group restriction: when on
//     (the default — a nil pointer means on), an agent may only spawn
//     into a group it is itself a member or owner of. Set it to false
//     to let a spawn-capable agent spawn into any group.
//   - SpawnAllowedGroups widens the restriction with a fixed allowlist
//     of group names an agent may always spawn into, even when it is
//     not a member/owner. Empty (the default) means no extra groups.
//   - SpawnMaxPerHour caps how many agents one caller-agent may spawn
//     per rolling hour. A nil pointer means the built-in default (10);
//     0 disables the rate limit (unlimited). The daemon resolves it
//     into agentd.SpawnMaxPerWindow once at startup.
//
// (CloneCooldown above is a distinct, separately-named knob — the
// clone cooldown — not part of this guardrail layer.)
//
// The per-group member cap is NOT here — it is a hard property of the
// group itself (agent_groups.max_members), set via `groups
// set-max-members` / the dashboard, and applies to every caller.
type AgentConfig struct {
	DefaultPermissions    []string            `json:"default_permissions,omitempty"`
	Sudo                  *SudoConfig         `json:"sudo,omitempty"`
	ContextNudge          *ContextNudgeConfig `json:"context_nudge,omitempty"`
	AutoLaunchDashboard   bool                `json:"auto_launch_dashboard,omitempty"`
	CloneCooldown         string              `json:"clone_cooldown,omitempty"`
	SpawnGroupRestriction *bool               `json:"spawn_group_restriction,omitempty"`
	SpawnAllowedGroups    []string            `json:"spawn_allowed_groups,omitempty"`
	SpawnMaxPerHour       *int                `json:"spawn_max_per_hour,omitempty"`
}

// ContextNudgeConfig controls the opt-in "consider reincarnating"
// nudge that fires as a long-running agent's context fills. Off by
// default — a fresh daemon shouldn't start typing into the agent's
// pane until the human signs up for it.
//
// Threshold ladder: starting at MinPct, every IntervalPct, capped at
// 90. So MinPct=30 + IntervalPct=10 → fires at 30, 40, 50, 60, 70,
// 80, 90. MinPct=50 + IntervalPct=20 → 50, 70, 90.
//
// The daemon tracks per-session "highest threshold already fired"
// in sessions.nudged_pct so flicker around a boundary doesn't
// re-fire. ResetCompact zeroes it so a compacted session can be
// re-nudged on its next climb.
type ContextNudgeConfig struct {
	Enabled     bool `json:"enabled,omitempty"`
	MinPct      int  `json:"min_pct,omitempty"`
	IntervalPct int  `json:"interval_pct,omitempty"`
}

// defaultContextNudgeMinPct / defaultContextNudgeIntervalPct are the
// fallbacks when Enabled is true but the user didn't specify a
// threshold ladder. Picked to fire often enough to be useful (30%
// is the first "we're past the easy zone" moment) without spamming
// (10-point steps give six nudges max over a session).
const (
	defaultContextNudgeMinPct      = 30
	defaultContextNudgeIntervalPct = 10
)

// Resolved returns the effective (MinPct, IntervalPct) for this
// config — caller-supplied values when present, sensible defaults
// otherwise. Enabled callers should use this so they don't have to
// repeat the fallback logic. Returns zeros when Enabled is false
// so the caller can tell "off" apart from "on with defaults".
func (c *ContextNudgeConfig) Resolved() (enabled bool, minPct, intervalPct int) {
	if c == nil || !c.Enabled {
		return false, 0, 0
	}
	minPct = c.MinPct
	if minPct <= 0 {
		minPct = defaultContextNudgeMinPct
	}
	intervalPct = c.IntervalPct
	if intervalPct <= 0 {
		intervalPct = defaultContextNudgeIntervalPct
	}
	return true, minPct, intervalPct
}

// SudoConfig overrides the hardcoded sudo defaults globally. Each
// field is optional: an empty/unset value preserves the agentd
// fallback. Use Overrides to scope overrides to a specific conv /
// title.
//
// Blocklist is a pointer-to-slice so we can distinguish "field
// absent → keep the default block of permissions.grant /
// permissions.revoke" from "field present, value [] → explicitly
// empty blocklist (you really know what you're doing)". Replace
// semantics, not merge — when set, this field is the complete list.
type SudoConfig struct {
	MaxDuration     string                         `json:"max_duration,omitempty"`
	DefaultDuration string                         `json:"default_duration,omitempty"`
	PopupTimeout    string                         `json:"popup_timeout,omitempty"`
	Blocklist       *[]string                      `json:"blocklist,omitempty"`
	Overrides       map[string]*SudoConfigOverride `json:"overrides,omitempty"`
}

// SudoConfigOverride is the per-conv twin of SudoConfig — same fields
// minus Overrides (no recursion). A non-empty override field replaces
// the corresponding global value; unset fields fall through to the
// global SudoConfig (and then to the agentd hardcoded defaults).
type SudoConfigOverride struct {
	MaxDuration     string    `json:"max_duration,omitempty"`
	DefaultDuration string    `json:"default_duration,omitempty"`
	PopupTimeout    string    `json:"popup_timeout,omitempty"`
	Blocklist       *[]string `json:"blocklist,omitempty"`
}

// MatchSudoOverride picks the SudoConfigOverride that applies to the
// caller (convID / title). Match shape mirrors the historical
// permission_overrides[<conv-id|prefix|title>] pattern: a key matches
// if it equals one of the two identifiers OR is a prefix of conv-id
// (≥8 chars) or title. The longest matching key wins so a more
// specific override beats a generic prefix. Returns nil when no key
// matches.
func (c *Config) MatchSudoOverride(convID, title string) *SudoConfigOverride {
	if c == nil || c.Agent == nil || c.Agent.Sudo == nil {
		return nil
	}
	var (
		bestKey string
		best    *SudoConfigOverride
	)
	for k, v := range c.Agent.Sudo.Overrides {
		if !sudoOverrideKeyMatches(k, convID, title) {
			continue
		}
		if len(k) > len(bestKey) {
			bestKey = k
			best = v
		}
	}
	return best
}

func sudoOverrideKeyMatches(key, convID, title string) bool {
	if key == "" {
		return false
	}
	if key == convID || key == title {
		return true
	}
	// Conv-id prefix match: 8 chars is the same threshold ResolveSelector
	// uses for prefix lookups, so config keys can use a stable short form.
	if len(key) >= 8 && convID != "" && len(key) <= len(convID) && convID[:len(key)] == key {
		return true
	}
	if title != "" && len(key) <= len(title) && title[:len(key)] == key {
		return true
	}
	return false
}

// HasDefaultPermission reports whether perm is in the global defaults
// list. Per-agent overrides live in SQLite and are checked separately
// by the daemon's requirePermission — this method only covers the
// defaults half of that lookup.
func (c *Config) HasDefaultPermission(perm string) bool {
	if c == nil || c.Agent == nil {
		return false
	}
	return slices.Contains(c.Agent.DefaultPermissions, perm)
}

// NotificationConfig holds settings for OS notifications.
type NotificationConfig struct {
	Enabled             bool             `json:"enabled"`
	Transitions         []TransitionRule `json:"transitions,omitempty"`
	CooldownSeconds     int              `json:"cooldown_seconds,omitempty"`
	NotificationCommand []string         `json:"notification_command,omitempty"`
}

// RateLimitConfig holds settings for rate limit
type RateLimitConfig struct {
	FiveHourPercentMaxUsed float64 `json:"five_hour_percent_max_used"`
	SevenDayPercentMaxUsed float64 `json:"seven_day_percent_max_used"`
}

// TransitionRule defines a state transition that triggers a notification.
// Use "*" as a wildcard to match any state.
type TransitionRule struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		LogLevel: "info",
		Notifications: &NotificationConfig{
			Enabled: false,
			Transitions: []TransitionRule{
				{From: "*", To: "idle"},
				{From: "*", To: "awaiting_permission"},
				{From: "*", To: "awaiting_input"},
				{From: "*", To: "error"},
				{From: "*", To: "exited"},
			},
			CooldownSeconds: 5,
		},
		RateLimit: nil,
	}
}

// ConfigDir returns the tclaude config directory (~/.tclaude).
func ConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude")
}

// ConfigPath returns the path to the config file (~/.tclaude/config.json).
func ConfigPath() string {
	return filepath.Join(ConfigDir(), "config.json")
}

// Load loads the config from ~/.tclaude/config.json.
// Returns default config if file doesn't exist.
func Load() (*Config, error) {
	path := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		slog.Warn("Unable to load config", "err", err)
		return DefaultConfig(), err
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		slog.Warn("Unable to load config", "err", err)
		return DefaultConfig(), err
	}

	// Apply defaults for missing fields
	if config.LogLevel == "" {
		config.LogLevel = "info"
	}

	// Apply defaults for missing sections
	if config.Notifications == nil {
		config.Notifications = DefaultConfig().Notifications
	} else {
		// Apply defaults for missing notification fields
		if config.Notifications.CooldownSeconds == 0 {
			config.Notifications.CooldownSeconds = 5
		}
		if len(config.Notifications.Transitions) == 0 {
			config.Notifications.Transitions = DefaultConfig().Notifications.Transitions
		}
	}
	if config.RateLimit != nil {
		if v := config.RateLimit.FiveHourPercentMaxUsed; v <= 0 || v > 100 {
			slog.Warn("Invalid ratelimit.five_hour_percent_max_used; using default", "value", v)
			config.RateLimit.FiveHourPercentMaxUsed = 99.0
		}
		if v := config.RateLimit.SevenDayPercentMaxUsed; v <= 0 || v > 100 {
			slog.Warn("Invalid ratelimit.seven_day_percent_max_used; using default", "value", v)
			config.RateLimit.SevenDayPercentMaxUsed = 99.9
		}
	}

	return &config, nil
}

// Save saves the config to ~/.tclaude/config.json.
func Save(config *Config) error {
	dir := ConfigDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(ConfigPath(), data, 0644)
}

// MatchesTransition checks if a state transition matches any configured rule.
func (c *NotificationConfig) MatchesTransition(from, to string) bool {
	if c == nil || !c.Enabled {
		return false
	}

	for _, rule := range c.Transitions {
		fromMatch := rule.From == "*" || rule.From == from
		toMatch := rule.To == "*" || rule.To == to
		if fromMatch && toMatch {
			return true
		}
	}
	return false
}
