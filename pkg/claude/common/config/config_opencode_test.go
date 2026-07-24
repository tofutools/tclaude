package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func int64ptr(value int64) *int64 { return &value }

func TestResolvedOpenCodeLegacyLongContextPricingCutoff(t *testing.T) {
	const configured int64 = 180_000
	cases := []struct {
		name string
		cfg  *Config
		want int64
	}{
		{"nil config", nil, DefaultOpenCodeLegacyLongContextPricingCutoff},
		{"absent block", &Config{}, DefaultOpenCodeLegacyLongContextPricingCutoff},
		{"absent value", &Config{OpenCode: &OpenCodeConfig{}}, DefaultOpenCodeLegacyLongContextPricingCutoff},
		{"zero", &Config{OpenCode: &OpenCodeConfig{LegacyLongContextPricingCutoff: int64ptr(0)}}, DefaultOpenCodeLegacyLongContextPricingCutoff},
		{"negative", &Config{OpenCode: &OpenCodeConfig{LegacyLongContextPricingCutoff: int64ptr(-1)}}, DefaultOpenCodeLegacyLongContextPricingCutoff},
		{"configured", &Config{OpenCode: &OpenCodeConfig{LegacyLongContextPricingCutoff: int64ptr(configured)}}, configured},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.cfg.ResolvedOpenCodeLegacyLongContextPricingCutoff())
		})
	}
}

func TestValidate_OpenCodeLegacyLongContextPricingCutoff(t *testing.T) {
	for _, value := range []int64{0, -1} {
		cfg := DefaultConfig()
		cfg.OpenCode = &OpenCodeConfig{LegacyLongContextPricingCutoff: int64ptr(value)}
		assert.Contains(t, strings.Join(Validate(cfg), " | "), "opencode.legacy_long_context_pricing_cutoff")
	}

	cfg := DefaultConfig()
	cfg.OpenCode = &OpenCodeConfig{LegacyLongContextPricingCutoff: int64ptr(180_000)}
	for _, err := range Validate(cfg) {
		assert.NotContains(t, err, "opencode.legacy_long_context_pricing_cutoff")
	}
}

func TestLoad_MalformedOpenCodePricingCutoffFallsBackToDefault(t *testing.T) {
	isolateConfigHome(t)
	require.NoError(t, os.MkdirAll(DataDir(), 0o700))
	require.NoError(t, os.WriteFile(ConfigPath(), []byte(
		`{"opencode":{"legacy_long_context_pricing_cutoff":"many"}}`,
	), 0o644))

	cfg, err := Load()
	require.Error(t, err)
	assert.Equal(t, DefaultOpenCodeLegacyLongContextPricingCutoff,
		cfg.ResolvedOpenCodeLegacyLongContextPricingCutoff())
}
