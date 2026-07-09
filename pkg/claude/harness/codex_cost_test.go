package harness_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestCodexVirtualCostFromRollout_PricesLatestTotalUsage(t *testing.T) {
	home := codexTestHome(t)
	const id = "019ec004-4250-79b1-9ade-ebaea41354c1"
	cx := testharness.NewCodexSimWithID(t, home, id, "/home/u/proj")
	cx.Model = "gpt-5.3-codex"
	require.NoError(t, cx.Start())

	require.NoError(t, cx.WriteUserInput("small"))
	require.NoError(t, cx.WriteTokenCount(
		testharness.CodexTokenUsage{InputTokens: 1000, CachedInputTokens: 100, OutputTokens: 25, TotalTokens: 1025},
		testharness.CodexTokenUsage{InputTokens: 1000, CachedInputTokens: 100, OutputTokens: 25, TotalTokens: 1025}))

	require.NoError(t, cx.WriteUserInput("larger"))
	require.NoError(t, cx.WriteTokenCount(
		testharness.CodexTokenUsage{InputTokens: 2000, CachedInputTokens: 400, OutputTokens: 100, TotalTokens: 2100},
		testharness.CodexTokenUsage{InputTokens: 1000, CachedInputTokens: 300, OutputTokens: 75, TotalTokens: 1075}))

	cost, ok, err := harness.CodexVirtualCostFromRollout(cx.RolloutPath, "")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "gpt-5.3-codex", cost.Model)
	assert.InDelta(t, 0.00427, cost.CostUSD, 1e-9,
		"(1600 input * $1.75 + 400 cached * $0.175 + 100 output * $14) / 1M")
}

func TestCodexVirtualCostFromRollout_PricesFlagshipModels(t *testing.T) {
	cases := []struct {
		name          string
		model         string
		contextWindow int
		want          float64
	}{
		{name: "gpt-5.6-sol", model: "gpt-5.6-sol", contextWindow: 372000, want: 0.0000355},
		{name: "gpt-5.6-terra", model: "gpt-5.6-terra", contextWindow: 372000, want: 0.00001775},
		{name: "gpt-5.6-luna", model: "gpt-5.6-luna", contextWindow: 372000, want: 0.0000071},
		{name: "gpt-5.5 short", model: "gpt-5.5", contextWindow: 272000, want: 0.0000355},
		{name: "gpt-5.5 long", model: "gpt-5.5", contextWindow: 1000000, want: 0.0000560},
		{name: "gpt-5.5-pro short", model: "gpt-5.5-pro", contextWindow: 200000, want: 0.0002400},
		{name: "gpt-5.5-pro long", model: "gpt-5.5-pro", contextWindow: 1000000, want: 0.0003900},
		{name: "gpt-5.4 sampled codex window stays short", model: "gpt-5.4", contextWindow: 258400, want: 0.00001775},
		{name: "gpt-5.4 short", model: "gpt-5.4", contextWindow: 200000, want: 0.00001775},
		{name: "gpt-5.4 long", model: "gpt-5.4", contextWindow: 1000000, want: 0.0000280},
		{name: "gpt-5.4-mini", model: "gpt-5.4-mini", contextWindow: 1000000, want: 0.000005325},
		{name: "gpt-5.4-nano", model: "gpt-5.4-nano", contextWindow: 1000000, want: 0.00000147},
		{name: "gpt-5.4-pro short", model: "gpt-5.4-pro", contextWindow: 200000, want: 0.0002400},
		{name: "gpt-5.4-pro long", model: "gpt-5.4-pro", contextWindow: 1000000, want: 0.0003900},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := codexTestHome(t)
			cx := testharness.NewCodexSim(t, home, "/home/u/proj")
			cx.Model = tc.model
			cx.ContextWindow = tc.contextWindow
			require.NoError(t, cx.Start())
			require.NoError(t, cx.WriteUserInput("price it"))
			usage := testharness.CodexTokenUsage{InputTokens: 2, CachedInputTokens: 1, OutputTokens: 1, TotalTokens: 3}
			require.NoError(t, cx.WriteTokenCount(usage, usage))

			cost, ok, err := harness.CodexVirtualCostFromRollout(cx.RolloutPath, "")
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, tc.model, cost.Model)
			assert.InDelta(t, tc.want, cost.CostUSD, 1e-12)
		})
	}
}

func TestCodexVirtualCostFromRollout_ResearchPreviewWithoutFinalRate(t *testing.T) {
	home := codexTestHome(t)
	cx := testharness.NewCodexSim(t, home, "/home/u/proj")
	cx.Model = "gpt-5.3-codex-spark"
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteUserInput("price it"))
	usage := testharness.CodexTokenUsage{InputTokens: 1000, CachedInputTokens: 100, OutputTokens: 100, TotalTokens: 1100}
	require.NoError(t, cx.WriteTokenCount(usage, usage))

	_, ok, err := harness.CodexVirtualCostFromRollout(cx.RolloutPath, "")
	require.NoError(t, err)
	assert.False(t, ok, "research-preview model without a final rate must stay unestimated")
}

func TestCodexVirtualCostFromRollout_UnknownModelNoEstimate(t *testing.T) {
	home := codexTestHome(t)
	const id = "019ec004-4250-79b1-9ade-ebaea41354c2"
	cx := testharness.NewCodexSimWithID(t, home, id, "/home/u/proj")
	cx.Model = "gpt-unknown-codex"
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteUserInput("hello"))
	require.NoError(t, cx.WriteTokenCount(
		testharness.CodexTokenUsage{InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100},
		testharness.CodexTokenUsage{InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100}))

	_, ok, err := harness.CodexVirtualCostFromRollout(cx.RolloutPath, "")
	require.NoError(t, err)
	assert.False(t, ok, "unknown models must not invent a price")
}
