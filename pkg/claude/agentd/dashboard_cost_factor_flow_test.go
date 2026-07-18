package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// setCostFactorConfig writes a cost.estimate_factor into the test's
// isolated config (newFlow points HOME at a temp dir), so the dashboard
// handlers' config.Load picks it up. Display-only — the seeded DB rows
// stay raw.
func setCostFactorConfig(t *testing.T, factor float64) {
	t.Helper()
	require.NoError(t, config.Save(&config.Config{
		Cost: &config.CostConfig{EstimateFactor: &factor},
	}))
}

// Scenario: the human has set a 2× cost display multiplier to track the
// gap between Claude Code's client-side estimate and the actual bill.
// The Costs tab's per-day bars, per-agent breakdown and total must all
// come back scaled — while the underlying session_cost_daily rows are
// untouched (proven by the unscaled control fetch first).
func TestDashboardCosts_DisplayFactorScalesEverything(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)

	const convA = "fcsa-1111-2222-3333-4444"
	const convB = "fcsb-1111-2222-3333-4444"
	seedAgentCostSession(t, "fcs-a1", convA, 1.25)
	seedAgentCostSession(t, "fcs-b1", convB, 0.50)

	mux := agentd.BuildDashboardHandlerForTest()

	// Control: no factor configured → raw figures (the existing contract).
	raw := fetchCosts(t, mux, "")
	assert.InDelta(t, 1.75, raw.TotalUSD, 1e-9, "raw total with no factor")

	// Now configure a 2× display multiplier.
	setCostFactorConfig(t, 2.0)
	scaled := fetchCosts(t, mux, "")

	today := time.Now().Format("2006-01-02")
	assert.InDelta(t, 3.50, scaled.TotalUSD, 1e-9, "total doubled")
	byConv := map[string]costsRespConv{}
	for _, a := range scaled.Agents {
		byConv[a.ConvID] = a
	}
	assert.InDelta(t, 2.50, byConv[convA].CostUSD, 1e-9, "per-agent A doubled")
	assert.InDelta(t, 1.00, byConv[convB].CostUSD, 1e-9, "per-agent B doubled")
	// Today's bar carries all of today's spend, doubled.
	last := scaled.Days[len(scaled.Days)-1]
	assert.Equal(t, today, last.Day)
	assert.InDelta(t, 3.50, last.CostUSD, 1e-9, "today's bar doubled")

	// The stored rows are unchanged: clearing the factor restores the raw
	// figures exactly.
	setCostFactorConfig(t, 1.0)
	restored := fetchCosts(t, mux, "")
	assert.InDelta(t, 1.75, restored.TotalUSD, 1e-9, "factor 1 restores raw — DB never scaled")
}

// Scenario: the same 2× multiplier must also scale the snapshot's
// per-agent cost badge and the top-bar month-to-date / today figures, so
// every cost surface moves in lockstep. A control snapshot with no factor
// proves the scaling is the only difference.
func TestDashboardSnapshot_DisplayFactorScalesCost(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	const conv = "fsnp-1111-2222-3333-4444"
	const label = "spwn-fsnp"
	f.HaveAliveSession(conv, label, "tmux-fsnp", f.TestCwd("fsnp"))
	f.HaveEnrolledAgent(conv)
	seedAgentCostSession(t, label, conv, 1.37)

	mux := agentd.BuildDashboardHandlerForTest()

	// Control: no factor → raw cost on the badge and the top bar.
	raw := fetchDashSnapshot(t, mux)
	rawAgent := findDashAgent(raw, conv)
	require.NotNil(t, rawAgent)
	assert.InDelta(t, 1.37, rawAgent.State.CostUSD, 1e-9, "raw per-agent cost")
	assert.InDelta(t, 1.37, raw.Usage.TotalCostUSD, 1e-9, "raw month-to-date")
	assert.InDelta(t, 1.37, raw.Usage.TodayCostUSD, 1e-9, "raw today")

	// 2× display multiplier.
	setCostFactorConfig(t, 2.0)
	scaled := fetchDashSnapshot(t, mux)
	scaledAgent := findDashAgent(scaled, conv)
	require.NotNil(t, scaledAgent)
	assert.InDelta(t, 2.74, scaledAgent.State.CostUSD, 1e-9, "per-agent badge doubled")
	assert.InDelta(t, 2.74, scaled.Usage.TotalCostUSD, 1e-9, "month-to-date doubled")
	assert.InDelta(t, 2.74, scaled.Usage.TodayCostUSD, 1e-9, "today doubled")

	// The stored cost stays raw — display scaling never wrote back. Read
	// it through the same accessor the handler uses.
	snapRow, err := db.GetContextSnapshot(label)
	require.NoError(t, err)
	assert.InDelta(t, 1.37, snapRow.CostUSD, 1e-9, "DB cost_usd stays raw")
}
