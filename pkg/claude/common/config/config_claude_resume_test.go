package config

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ClaudeResumeEnv emits a CLAUDE_CODE_RESUME_* var for each set field and
// omits the ones left nil, so an unconfigured threshold falls through to
// Claude Code's own default. It is nil-safe on every empty shape.
func TestClaudeResumeEnv(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want map[string]string
	}{
		{"nil config", nil, nil},
		{"absent block", &Config{}, nil},
		{"empty block omits both vars", &Config{ClaudeResume: &ClaudeResumeConfig{}}, map[string]string{}},
		{
			"minutes only",
			&Config{ClaudeResume: &ClaudeResumeConfig{ThresholdMinutes: new(70)}},
			map[string]string{EnvResumeThresholdMinutes: "70"},
		},
		{
			"tokens only",
			&Config{ClaudeResume: &ClaudeResumeConfig{TokenThreshold: new(100000)}},
			map[string]string{EnvResumeTokenThreshold: "100000"},
		},
		{
			"both",
			&Config{ClaudeResume: &ClaudeResumeConfig{ThresholdMinutes: new(70), TokenThreshold: new(100000)}},
			map[string]string{EnvResumeThresholdMinutes: "70", EnvResumeTokenThreshold: "100000"},
		},
		{
			"explicit zero is emitted (always-show), distinct from absent",
			&Config{ClaudeResume: &ClaudeResumeConfig{ThresholdMinutes: new(0)}},
			map[string]string{EnvResumeThresholdMinutes: "0"},
		},
		{
			"suppress sentinel",
			&Config{ClaudeResume: &ClaudeResumeConfig{ThresholdMinutes: new(ResumeThresholdMinutesSuppress)}},
			map[string]string{EnvResumeThresholdMinutes: strconv.Itoa(ResumeThresholdMinutesSuppress)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.cfg.ClaudeResumeEnv())
		})
	}
}

// The env var names are the exact, undocumented Claude Code knobs the
// investigation pinned (CC 2.1.187). A typo here silently stops suppressing
// the prompt, so pin them.
func TestClaudeResumeEnvVarNames(t *testing.T) {
	assert.Equal(t, "CLAUDE_CODE_RESUME_THRESHOLD_MINUTES", EnvResumeThresholdMinutes)
	assert.Equal(t, "CLAUDE_CODE_RESUME_TOKEN_THRESHOLD", EnvResumeTokenThreshold)
}

// Validate rejects a negative threshold (so the Config tab tells the human)
// but accepts 0 (always-show) and a large suppress value, and stays silent
// when the block is absent.
func TestValidate_ClaudeResume(t *testing.T) {
	rejects := func(field string, c *ClaudeResumeConfig) {
		cfg := DefaultConfig()
		cfg.ClaudeResume = c
		assert.Contains(t, strings.Join(Validate(cfg), " | "), field)
	}
	rejects("claude_resume.threshold_minutes", &ClaudeResumeConfig{ThresholdMinutes: new(-1)})
	rejects("claude_resume.token_threshold", &ClaudeResumeConfig{TokenThreshold: new(-5)})

	accepts := func(c *ClaudeResumeConfig) {
		cfg := DefaultConfig()
		cfg.ClaudeResume = c
		for _, e := range Validate(cfg) {
			assert.NotContains(t, e, "claude_resume")
		}
	}
	accepts(&ClaudeResumeConfig{ThresholdMinutes: new(0)})
	accepts(&ClaudeResumeConfig{ThresholdMinutes: new(ResumeThresholdMinutesSuppress)})
	accepts(&ClaudeResumeConfig{ThresholdMinutes: new(70), TokenThreshold: new(100000)})

	// An absent block is never an error.
	for _, e := range Validate(DefaultConfig()) {
		assert.NotContains(t, e, "claude_resume")
	}
}

// The block round-trips through JSON with omitempty: an absent block marshals
// to nothing (so the dashboard diff stays clean), and a set value survives a
// marshal→unmarshal cycle. This is the contract the Config tab editor relies on.
func TestClaudeResume_JSONRoundTrip(t *testing.T) {
	// Absent block → no key.
	raw, err := json.Marshal(&Config{})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "claude_resume")

	// Set value survives the round-trip.
	in := &Config{ClaudeResume: &ClaudeResumeConfig{ThresholdMinutes: new(ResumeThresholdMinutesSuppress)}}
	raw, err = json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "claude_resume")
	assert.Contains(t, string(raw), "threshold_minutes")

	var out Config
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.ClaudeResume)
	require.NotNil(t, out.ClaudeResume.ThresholdMinutes)
	assert.Equal(t, ResumeThresholdMinutesSuppress, *out.ClaudeResume.ThresholdMinutes)
	assert.Nil(t, out.ClaudeResume.TokenThreshold)
}
