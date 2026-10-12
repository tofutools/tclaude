package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// pi has no status line: the context meter and the cost come from what
// tclaude's extension reports on each event. A pinned auto-compaction window
// reaches the pane, and the meter measures against it, as for Claude Code.
func TestPiTelemetry_ContextMeterAndCost(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, sim := spawnPi(t, f, "crew", map[string]any{"name": "pi-metered", "auto_compact_window": "32k"})
	assert.Equal(t, "32000", sim.Launch().Env[harness.AutoCompactWindowEnvVar], "the pin reaches the pane")

	typeInto(sim, "count the files")
	sim.Measure(16_000, 64_000, 0.42, false)
	require.NoError(t, sim.Reply("There are 12."))

	mux := agentd.BuildDashboardHandlerForTest()
	agentRow := findDashAgent(fetchDashSnapshot(t, mux), resp.ConvID)
	require.NotNil(t, agentRow)
	assert.Equal(t, int64(32_000), agentRow.State.ContextWindowSize, "measured against the pinned window")
	assert.InDelta(t, 50.0, agentRow.State.ContextPct, 1e-9, "16k of a 32k pin, not 25% of the model's 64k")

	costs := fetchCosts(t, mux, "")
	var cost float64
	for _, a := range costs.Agents {
		if a.ConvID == resp.ConvID {
			cost = a.CostUSD
		}
	}
	assert.InDelta(t, 0.42, cost, 1e-9, "an API-key session's spend is real cost")
}

// Under a subscription login pi still prices the tokens; that figure is only
// what they would have cost, so it is recorded as what-if cost.
func TestPiTelemetry_SubscriptionCostIsWhatIf(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, sim := spawnPi(t, f, "crew", map[string]any{"name": "pi-subscribed"})

	typeInto(sim, "count the files")
	sim.Measure(8_000, 64_000, 0.30, true)
	require.NoError(t, sim.Reply("There are 12."))

	mux := agentd.BuildDashboardHandlerForTest()
	for _, a := range fetchCosts(t, mux, "").Agents {
		assert.NotEqual(t, resp.ConvID, a.ConvID, "what-if cost shows only on the operator's opt-in")
	}
	require.NoError(t, config.Save(&config.Config{Cost: &config.CostConfig{ShowOnSubscription: true}}))
	for _, a := range fetchCosts(t, mux, "").Agents {
		if a.ConvID == resp.ConvID {
			assert.Zero(t, a.RealCostUSD, "no real spend")
			assert.InDelta(t, 0.30, a.WhatIfCostUSD, 1e-9)
			return
		}
	}
	t.Fatalf("%s missing from /api/costs", resp.ConvID)
}
