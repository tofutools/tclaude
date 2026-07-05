package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// ResolvedUsageIdleTimeout defaults to the built-in grace when unconfigured,
// returns a parseable positive duration verbatim, and falls back to the
// default on a blank / unparseable / non-positive value — the resolver never
// leaves the dashboard readout without a bound (Validate is what surfaces a
// bad string to the human).
func TestResolvedUsageIdleTimeout(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want time.Duration
	}{
		{"nil config", nil, DefaultUsageIdleTimeout},
		{"absent block", &Config{}, DefaultUsageIdleTimeout},
		{"empty string", &Config{Usage: &UsageConfig{}}, DefaultUsageIdleTimeout},
		{"configured hours", &Config{Usage: &UsageConfig{IdleTimeout: "12h"}}, 12 * time.Hour},
		{"configured compound", &Config{Usage: &UsageConfig{IdleTimeout: "1h30m"}}, 90 * time.Minute},
		{"three days as hours", &Config{Usage: &UsageConfig{IdleTimeout: "72h"}}, 72 * time.Hour},
		{"unparseable falls back", &Config{Usage: &UsageConfig{IdleTimeout: "3d"}}, DefaultUsageIdleTimeout},
		{"garbage falls back", &Config{Usage: &UsageConfig{IdleTimeout: "soon"}}, DefaultUsageIdleTimeout},
		{"zero falls back", &Config{Usage: &UsageConfig{IdleTimeout: "0s"}}, DefaultUsageIdleTimeout},
		{"negative falls back", &Config{Usage: &UsageConfig{IdleTimeout: "-2h"}}, DefaultUsageIdleTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.cfg.ResolvedUsageIdleTimeout())
		})
	}
}

// Validate reports an unparseable or non-positive usage.idle_timeout (so the
// Config tab tells the human) but accepts a valid one and stays silent when
// the block is absent — the resolver, not Validate, owns the runtime
// fallback.
func TestValidate_UsageIdleTimeout(t *testing.T) {
	reject := func(s string) {
		c := DefaultConfig()
		c.Usage = &UsageConfig{IdleTimeout: s}
		errs := Validate(c)
		assert.Contains(t, strings.Join(errs, " | "), "usage.idle_timeout",
			"idle_timeout %q should be rejected", s)
	}
	reject("3d")   // ParseDuration has no day unit
	reject("soon") // not a duration at all
	reject("0s")   // not positive
	reject("-2h")  // not positive

	accept := func(s string) {
		c := DefaultConfig()
		c.Usage = &UsageConfig{IdleTimeout: s}
		for _, e := range Validate(c) {
			assert.NotContains(t, e, "usage.idle_timeout", "idle_timeout %q should be accepted", s)
		}
	}
	accept("72h")
	accept("30m")
	accept("1h30m")

	// An absent block is never an error.
	for _, e := range Validate(DefaultConfig()) {
		assert.NotContains(t, e, "usage.idle_timeout")
	}
}
