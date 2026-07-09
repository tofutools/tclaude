package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ClaudeCleanupPeriodDaysOverride reports ok only for a positive day count; 0,
// absent, and a negative value all mean "tclaude doesn't manage the key" so
// Claude Code's own cleanupPeriodDays default (30) stands. Nil-safe.
func TestClaudeCleanupPeriodDaysOverride(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *Config
		wantDays int
		wantOK   bool
	}{
		{"nil config", nil, 0, false},
		{"absent / zero", &Config{}, 0, false},
		{"explicit zero", &Config{ClaudeCleanupPeriodDays: 0}, 0, false},
		{"negative is unmanaged", &Config{ClaudeCleanupPeriodDays: -3}, 0, false},
		{"positive override", &Config{ClaudeCleanupPeriodDays: 99999}, 99999, true},
		{"one day", &Config{ClaudeCleanupPeriodDays: 1}, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			days, ok := tc.cfg.ClaudeCleanupPeriodDaysOverride()
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantDays, days)
		})
	}
}

// Validate rejects a negative day count (so the Config tab tells the human) but
// accepts 0 (leave-alone sentinel) and any positive value, and stays silent
// when the field is absent.
func TestValidate_ClaudeCleanupPeriodDays(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ClaudeCleanupPeriodDays = -1
	assert.Contains(t, strings.Join(Validate(cfg), " | "), "claude_cleanup_period_days")

	for _, days := range []int{0, 1, 30, 99999} {
		c := DefaultConfig()
		c.ClaudeCleanupPeriodDays = days
		for _, e := range Validate(c) {
			assert.NotContains(t, e, "claude_cleanup_period_days")
		}
	}
}

// The field round-trips through JSON with omitempty: 0 marshals to nothing (so
// the dashboard diff stays clean), and a set value survives a marshal→unmarshal
// cycle. This is the contract the Config tab editor relies on.
func TestClaudeCleanupPeriodDays_JSONRoundTrip(t *testing.T) {
	// Zero → no key.
	raw, err := json.Marshal(&Config{})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "claude_cleanup_period_days")

	// Set value survives the round-trip.
	raw, err = json.Marshal(&Config{ClaudeCleanupPeriodDays: 99999})
	require.NoError(t, err)
	assert.Contains(t, string(raw), "claude_cleanup_period_days")

	var out Config
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, 99999, out.ClaudeCleanupPeriodDays)
}
