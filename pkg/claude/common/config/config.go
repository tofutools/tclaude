// Package config provides configuration loading for tclaude.
package config

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
)

// Config represents the tclaude configuration file structure.
type Config struct {
	Notifications      *NotificationConfig `json:"notifications,omitempty"`
	AutoCompactPercent *int                `json:"auto_compact_percent,omitempty"`
	LogLevel           string              `json:"log_level,omitempty"`
	RecordHooks        bool                `json:"record_hooks,omitempty"`
	Tasks              *TasksConfig        `json:"tasks,omitempty"`
}

// NotificationConfig holds settings for OS notifications.
type NotificationConfig struct {
	Enabled             bool             `json:"enabled"`
	Transitions         []TransitionRule `json:"transitions,omitempty"`
	CooldownSeconds     int              `json:"cooldown_seconds,omitempty"`
	NotificationCommand []string         `json:"notification_command,omitempty"`
}

// TasksConfig holds settings for task runner
type TasksConfig struct {
	FiveHourRateLimitPercentMaxUsed float64 `json:"five_hour_rate_limit_percent_max_used"`
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
		Tasks: &TasksConfig{
			FiveHourRateLimitPercentMaxUsed: 99.0,
		},
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
	if config.Tasks == nil {
		config.Tasks = DefaultConfig().Tasks
	} else {
		if config.Tasks.FiveHourRateLimitPercentMaxUsed == 0.0 {
			config.Tasks.FiveHourRateLimitPercentMaxUsed = DefaultConfig().Tasks.FiveHourRateLimitPercentMaxUsed
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
