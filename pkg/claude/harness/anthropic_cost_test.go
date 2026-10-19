package harness

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLookupAnthropicModelPricing(t *testing.T) {
	p, ok := LookupAnthropicModelPricing("claude-opus-5-5")
	require.True(t, ok)
	require.Equal(t, AnthropicModelPrice{4, 5, 8, 0.20, 20}, p.Short)
	p, ok = LookupAnthropicModelPricing("claude-haiku-4-5-20251001")
	require.True(t, ok, "a dated ID prices as its model")
	require.Equal(t, 5.0, p.Short.OutputPerMTok)
	p, ok = LookupAnthropicModelPricing("claude-opus-5-5[1m]")
	require.True(t, ok, "the 1M context tag is standard pricing")
	require.Equal(t, 4.0, p.Short.InputPerMTok)
	p, ok = LookupAnthropicModelPricing("claude-haiku-5-5")
	require.True(t, ok)
	require.NotNil(t, p.Long)
	require.EqualValues(t, 100000, p.LongAbove)
	for _, unknown := range []string{"", "claude-opus-9", "claude-opus-5-5-fast", "gpt-6-sol"} {
		_, ok := LookupAnthropicModelPricing(unknown)
		require.False(t, ok, unknown)
	}
}
