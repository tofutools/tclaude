package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ResolvedRetiredCleanup stays OFF for an absent / disabled block, and
// when enabled resolves a non-positive window to the ~1-year default
// rather than a zero/negative window that would reap everything at once.
func TestResolvedRetiredCleanup(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *Config
		wantOn   bool
		wantDays int
	}{
		{"nil config", nil, false, 0},
		{"no agent block", &Config{}, false, 0},
		{"no cleanup block", &Config{Agent: &AgentConfig{}}, false, 0},
		{"disabled keeps off", &Config{Agent: &AgentConfig{RetiredCleanup: &RetiredCleanupConfig{Enabled: false, AfterDays: 30}}}, false, 0},
		{"enabled, explicit window", &Config{Agent: &AgentConfig{RetiredCleanup: &RetiredCleanupConfig{Enabled: true, AfterDays: 90}}}, true, 90},
		{"enabled, zero falls back to default", &Config{Agent: &AgentConfig{RetiredCleanup: &RetiredCleanupConfig{Enabled: true}}}, true, DefaultRetiredCleanupAfterDays},
		{"enabled, negative falls back to default", &Config{Agent: &AgentConfig{RetiredCleanup: &RetiredCleanupConfig{Enabled: true, AfterDays: -5}}}, true, DefaultRetiredCleanupAfterDays},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			on, days := tc.cfg.ResolvedRetiredCleanup()
			assert.Equal(t, tc.wantOn, on)
			assert.Equal(t, tc.wantDays, days)
		})
	}
}

// Validate rejects after_days < 1 while the sweep is enabled (so the
// Config tab tells the human a 0 would silently snap to the default), but
// tolerates 0 when it is off and accepts a real window. An absent block is
// never an error.
func TestValidate_RetiredCleanup(t *testing.T) {
	reject := func(rc *RetiredCleanupConfig) {
		c := DefaultConfig()
		c.Agent = &AgentConfig{RetiredCleanup: rc}
		assert.Contains(t, strings.Join(Validate(c), " | "), "agent.retired_cleanup.after_days")
	}
	reject(&RetiredCleanupConfig{Enabled: true, AfterDays: 0})
	reject(&RetiredCleanupConfig{Enabled: true, AfterDays: -1})

	accept := func(rc *RetiredCleanupConfig) {
		c := DefaultConfig()
		c.Agent = &AgentConfig{RetiredCleanup: rc}
		for _, e := range Validate(c) {
			assert.NotContains(t, e, "agent.retired_cleanup")
		}
	}
	accept(&RetiredCleanupConfig{Enabled: true, AfterDays: 1})
	accept(&RetiredCleanupConfig{Enabled: true, AfterDays: 365})
	accept(&RetiredCleanupConfig{Enabled: false, AfterDays: 0}) // off → 0 tolerated

	// An absent block is never an error.
	for _, e := range Validate(DefaultConfig()) {
		assert.NotContains(t, e, "agent.retired_cleanup")
	}
}

// The block round-trips through JSON with omitempty: an absent block
// marshals to nothing (so the Config-tab diff stays clean), and a set
// value survives a marshal→unmarshal cycle.
func TestRetiredCleanup_JSONRoundTrip(t *testing.T) {
	raw, err := json.Marshal(&Config{})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "retired_cleanup")

	in := &Config{Agent: &AgentConfig{RetiredCleanup: &RetiredCleanupConfig{Enabled: true, AfterDays: 200}}}
	raw, err = json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "retired_cleanup")
	assert.Contains(t, string(raw), "after_days")

	var out Config
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.Agent)
	require.NotNil(t, out.Agent.RetiredCleanup)
	assert.True(t, out.Agent.RetiredCleanup.Enabled)
	assert.Equal(t, 200, out.Agent.RetiredCleanup.AfterDays)
}
