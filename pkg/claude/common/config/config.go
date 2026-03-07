// Package config provides configuration loading for tofu.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config represents the tofu configuration file structure.
type Config struct {
	Notifications *NotificationConfig `json:"notifications,omitempty"`
}

// NotificationConfig holds settings for OS notifications.
type NotificationConfig struct {
	Enabled         bool             `json:"enabled"`
	Transitions     []TransitionRule `json:"transitions,omitempty"`
	CooldownSeconds int              `json:"cooldown_seconds,omitempty"`
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
	}
}

// ConfigDir returns the tofu config directory (~/.tofu).
func ConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tofu")
}

// ConfigPath returns the path to the config file (~/.tofu/config.json).
func ConfigPath() string {
	return filepath.Join(ConfigDir(), "config.json")
}

// Load loads the config from ~/.tofu/config.json.
// Returns default config if file doesn't exist.
func Load() (*Config, error) {
	path := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return nil, err
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
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

	return &config, nil
}

// Save saves the config to ~/.tofu/config.json.
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
