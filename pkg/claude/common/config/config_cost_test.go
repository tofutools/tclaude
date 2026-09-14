package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func fptr(f float64) *float64 { return &f }

// ResolvedCostFactor defaults to a no-op 1.0 when unconfigured, returns
// a sane configured value verbatim, treats a non-positive value as the
// no-op default, and clamps an over-range value down so a hand-edited
// absurd factor can't silently inflate the display.
func TestResolvedCostFactor(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want float64
	}{
		{"nil config", nil, 1.0},
		{"absent block", &Config{}, 1.0},
		{"nil pointer", &Config{Cost: &CostConfig{}}, 1.0},
		{"typical compensation", &Config{Cost: &CostConfig{EstimateFactor: fptr(1.1)}}, 1.1},
		{"explicit 1 is a no-op", &Config{Cost: &CostConfig{EstimateFactor: fptr(1)}}, 1.0},
		{"below 1 allowed", &Config{Cost: &CostConfig{EstimateFactor: fptr(0.9)}}, 0.9},
		{"zero falls back to default", &Config{Cost: &CostConfig{EstimateFactor: fptr(0)}}, 1.0},
		{"negative falls back to default", &Config{Cost: &CostConfig{EstimateFactor: fptr(-2)}}, 1.0},
		{"over-range clamps to max", &Config{Cost: &CostConfig{EstimateFactor: fptr(1000)}}, maxCostEstimateFactor},
		{"exactly max is kept", &Config{Cost: &CostConfig{EstimateFactor: fptr(maxCostEstimateFactor)}}, maxCostEstimateFactor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.InDelta(t, tc.want, tc.cfg.ResolvedCostFactor(), 1e-9)
		})
	}
}

// Validate reports an out-of-range cost factor (so the Config tab tells
// the human) but accepts an in-range one and stays silent when the block
// is absent — the resolver, not Validate, owns the runtime no-op.
func TestValidate_CostEstimateFactor(t *testing.T) {
	reject := func(f float64) {
		c := DefaultConfig()
		c.Cost = &CostConfig{EstimateFactor: fptr(f)}
		errs := Validate(c)
		assert.Contains(t, strings.Join(errs, " | "), "cost.estimate_factor",
			"factor %g should be rejected", f)
	}
	reject(0)
	reject(-1)
	reject(maxCostEstimateFactor + 0.01)

	accept := func(f float64) {
		c := DefaultConfig()
		c.Cost = &CostConfig{EstimateFactor: fptr(f)}
		for _, e := range Validate(c) {
			assert.NotContains(t, e, "cost.estimate_factor", "factor %g should be accepted", f)
		}
	}
	accept(1)
	accept(1.1)
	accept(maxCostEstimateFactor)

	// An absent block is never an error.
	for _, e := range Validate(DefaultConfig()) {
		assert.NotContains(t, e, "cost.estimate_factor")
	}
}

func TestCostFactorForHarness(t *testing.T) {
	var absent *Config
	assert.Equal(t, 1.0, absent.CostFactorForHarness("claude"))
	cfg := &Config{Cost: &CostConfig{EstimateFactor: fptr(2), HarnessFactors: map[string]float64{"claude": 1.2, "codex": 1}}}
	assert.Equal(t, 1.2, cfg.CostFactorForHarness("claude"), "override replaces default")
	assert.Equal(t, 1.0, cfg.CostFactorForHarness("codex"), "explicit 1 opts out")
	for _, name := range []string{"opencode", "copilot", "", "unknown"} {
		assert.Equal(t, 2.0, cfg.CostFactorForHarness(name), "missing override inherits default")
	}
	cfg.Cost.HarnessFactors["claude"] = 0
	assert.Equal(t, 2.0, cfg.CostFactorForHarness("claude"), "invalid override inherits")
	cfg.Cost.HarnessFactors["claude"] = 100
	assert.Equal(t, 10.0, cfg.CostFactorForHarness("claude"))
}

func TestValidateCostHarnessFactors(t *testing.T) {
	for _, value := range []float64{0, -1, 11} {
		cfg := DefaultConfig()
		cfg.Cost = &CostConfig{HarnessFactors: map[string]float64{"claude": value}}
		assert.Contains(t, strings.Join(Validate(cfg), " | "), "cost.harness_factors.claude")
	}
	cfg := DefaultConfig()
	cfg.Cost = &CostConfig{HarnessFactors: map[string]float64{"claude": 1, "opencode": 1.2}}
	assert.NotContains(t, strings.Join(Validate(cfg), " | "), "cost.harness_factors")
	cfg.Cost.HarnessFactors["anthropic"] = 1.2
	assert.Contains(t, strings.Join(Validate(cfg), " | "), "unknown harness")
}
