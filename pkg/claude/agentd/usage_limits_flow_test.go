package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
)

func blockSpawnLimit(t *testing.T, scope string, groupID int64, budget float64) *db.UsageLimit {
	t.Helper()
	r := usagerules.Rule{Name: "cap", Period: usagerules.PeriodMonth, Budget: budget, Steps: []usagerules.Step{
		{At: 100, Action: usagerules.ActionBlockSpawn},
	}}
	r.Normalize()
	require.NoError(t, r.Validate(usagerules.Scope(scope)))
	l := &db.UsageLimit{Scope: scope, GroupID: groupID, Rule: r, Enabled: true}
	require.NoError(t, db.CreateUsageLimit(l))
	return l
}

// Scenario: a group limit with a block_spawn step refuses spawns into that
// group once crossed — the human included — leaves other groups alone, and
// lifts as soon as the operator raises the budget.
func TestUsageLimits_BlockSpawnGatesGroupUntilBudgetRaised(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetHumanMessageNotifierForTest(func(_, _, _, _, _ string) {}))
	f.HaveGroup("alpha")
	f.HaveGroup("beta")
	alpha, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	const spender = "spnd-aaaa-bbbb-cccc-111111111111"
	f.HaveMember("alpha", spender)
	_, _, err = db.EnsureAgentForConv(spender, "spawn")
	require.NoError(t, err)
	seedAgentCostSession(t, "spender-session", spender, 30)
	l := blockSpawnLimit(t, db.UsageScopeGroup, alpha.ID, 25)

	agentd.PollUsageRulesForTest(time.Now())

	resp := f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "worker"})
	require.Equal(t, http.StatusConflict, resp.Code, "body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "usage_limit_reached")
	assert.Contains(t, string(resp.Raw), "cap")

	resp = f.AsHuman().SpawnWith("beta", map[string]any{"alias": "other"})
	require.Equal(t, http.StatusOK, resp.Code, "another group is not covered; body=%s", resp.Raw)

	l.Rule.Budget = 50
	_, err = db.UpdateUsageLimit(l)
	require.NoError(t, err)
	resp = f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "worker"})
	require.Equal(t, http.StatusOK, resp.Code, "a raised budget lifts the block at once; body=%s", resp.Raw)
}

// Scenario: a global block covers every group; a dry run never blocks.
func TestUsageLimits_GlobalBlockAndDryRun(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetHumanMessageNotifierForTest(func(_, _, _, _, _ string) {}))
	f.HaveGroup("alpha")
	seedAgentCostSession(t, "solo-session", "solo-conv", 30)
	l := blockSpawnLimit(t, db.UsageScopeGlobal, 0, 25)
	l.Rule.DryRun = true
	_, err := db.UpdateUsageLimit(l)
	require.NoError(t, err)

	agentd.PollUsageRulesForTest(time.Now())
	resp := f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "w1"})
	require.Equal(t, http.StatusOK, resp.Code, "a dry run never blocks; body=%s", resp.Raw)

	l.Rule.DryRun = false
	_, err = db.UpdateUsageLimit(l)
	require.NoError(t, err)
	agentd.PollUsageRulesForTest(time.Now())
	resp = f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "w2"})
	require.Equal(t, http.StatusConflict, resp.Code, "body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "global usage limit")

	l.Enabled = false
	_, err = db.UpdateUsageLimit(l)
	require.NoError(t, err)
	resp = f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "w3"})
	require.Equal(t, http.StatusOK, resp.Code, "a disabled limit never blocks; body=%s", resp.Raw)
}

// Scenario: the former cost.daily_limit_usd / monthly_limit_usd settings
// become global limits with the same alerts; an alert already sent this
// period under the old mechanism is not repeated, and the snapshot reports
// the limit for the banner.
func TestUsageLimits_LegacySpendLimitsConvert(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	t.Cleanup(agentd.SetHumanMessageNotifierForTest(func(_, _, _, _, _ string) {}))
	require.NoError(t, config.Save(&config.Config{Cost: &config.CostConfig{DailyLimitUSD: 10, MonthlyLimitUSD: 100}}))
	seedAgentCostSession(t, "legacy-api", "legacy-conv", 9)
	now := time.Now()
	inserted, err := db.InsertSpendAlert("daily", now.Format("2006-01-02"), 80, &db.HumanMessage{FromTitle: "Spend limits", Subject: "old", Body: "old"})
	require.NoError(t, err)
	require.True(t, inserted)

	agentd.PollUsageRulesForTest(now)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Zero(t, cfg.Cost.DailyLimitUSD)
	assert.Zero(t, cfg.Cost.MonthlyLimitUSD)
	limits, err := db.ListUsageLimits()
	require.NoError(t, err)
	require.Len(t, limits, 2)
	assert.Equal(t, "Daily API spend", limits[0].Rule.Name)
	assert.Equal(t, usagerules.MetricRealUSD, limits[0].Rule.Metric)
	assert.Equal(t, 10.0, limits[0].Rule.Budget)
	assert.Equal(t, "Monthly API spend", limits[1].Rule.Name)

	messages, err := db.ListHumanMessagesPage(db.HumanMessageFilter{}, 0, db.MaxHumanMessagesPage)
	require.NoError(t, err)
	assert.Len(t, messages, 1, "the daily 80%% alert was already sent; the monthly limit is at 9%%")

	agentd.PollUsageRulesForTest(now)
	limits, err = db.ListUsageLimits()
	require.NoError(t, err)
	assert.Len(t, limits, 2, "conversion runs once")

	snap := fetchSnapshotOnly(t, agentd.BuildDashboardHandlerForTest())
	require.Len(t, snap.Usage.UsageLimits, 2)
	assert.Equal(t, "notify", snap.Usage.UsageLimits[0].Reached)
	assert.InDelta(t, 90, snap.Usage.UsageLimits[0].Pct, 0.01)
}
