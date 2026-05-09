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
type AgentConfig struct {
	DefaultPermissions []string `json:"default_permissions,omitempty"`
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
