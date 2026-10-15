package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestSpendLimits_CrossingAndDurableDedup(t *testing.T) {
	newFlow(t)
	t.Cleanup(agentd.SetHumanMessageNotifierForTest(func(_, _, _, _, _ string) {}))
	t.Cleanup(agentd.WaitForBackgroundForTest)
	factor := 10.0
	cfg := &config.Config{Cost: &config.CostConfig{DailyLimitUSD: 10, MonthlyLimitUSD: 10, EstimateFactor: &factor, ShowOnSubscription: true}}
	require.NoError(t, config.Save(cfg))
	seedAgentCostSession(t, "spend-api", "spend-conv", 7.99)
	seedAgentCostSession(t, "spend-virtual", "virtual-conv", 0)
	require.NoError(t, db.UpdateSessionVirtualCost("spend-virtual", 100))
	mux := agentd.BuildDashboardHandlerForTest()
	check := func(threshold, count int) {
		t.Helper()
		snap := fetchSnapshotOnly(t, mux)
		require.Len(t, snap.Usage.SpendLimits, 2)
		for _, limit := range snap.Usage.SpendLimits {
			assert.Equal(t, threshold, limit.Threshold)
		}
		messages, err := db.ListHumanMessages()
		require.NoError(t, err)
		assert.Len(t, messages, count)
	}
	check(0, 0) // display multipliers and WHAT-IF must not trigger limits
	require.NoError(t, db.UpdateSessionCost("spend-api", 8))
	check(80, 2)
	check(80, 2)
	require.NoError(t, db.UpdateSessionCost("spend-api", 10))
	check(100, 4)
	agentd.WaitForBackgroundForTest()
	db.Close() // reopen the same database, as after a daemon restart
	check(100, 4)
	messages, err := db.ListHumanMessages()
	require.NoError(t, err)
	for _, m := range messages {
		assert.Contains(t, m.Body, "Estimates from the cost ledger")
		_, err = db.DeleteHumanMessage(m.ID)
		require.NoError(t, err)
	}
	check(100, 0) // deleting notifications must not reset dedup
	cfg.Cost.DailyLimitUSD = 5
	require.NoError(t, config.Save(cfg))
	check(100, 0) // editing a limit must not spam the same period/threshold
	cfg.Cost.DailyLimitUSD, cfg.Cost.MonthlyLimitUSD = 0, 0
	require.NoError(t, config.Save(cfg))
	assert.Empty(t, fetchSnapshotOnly(t, mux).Usage.SpendLimits)
}

func TestSpendLimits_JumpPastLimit(t *testing.T) {
	newFlow(t)
	t.Cleanup(agentd.SetHumanMessageNotifierForTest(func(_, _, _, _, _ string) {}))
	t.Cleanup(agentd.WaitForBackgroundForTest)
	require.NoError(t, config.Save(&config.Config{Cost: &config.CostConfig{DailyLimitUSD: 10}}))
	seedAgentCostSession(t, "jump-api", "jump-conv", 12)
	mux := agentd.BuildDashboardHandlerForTest()
	snap := fetchSnapshotOnly(t, mux)
	require.Len(t, snap.Usage.SpendLimits, 1)
	assert.Equal(t, 100, snap.Usage.SpendLimits[0].Threshold)
	messages, err := db.ListHumanMessages()
	require.NoError(t, err)
	assert.Len(t, messages, 2) // one for each crossed threshold
}
