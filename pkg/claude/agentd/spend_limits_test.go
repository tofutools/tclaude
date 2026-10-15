package agentd

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestSpendLimits_LocalPeriodRollover(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(SetHumanMessageNotifierForTest(func(_, _, _, _, _ string) {}))
	t.Cleanup(WaitForBackgroundForTest)
	cfg := &config.Config{Cost: &config.CostConfig{DailyLimitUSD: 10, MonthlyLimitUSD: 10}}
	now := time.Date(2026, 10, 31, 23, 59, 0, 0, time.FixedZone("local", 7200))
	evaluateSpendLimits(cfg, 10, 10, now)
	evaluateSpendLimits(cfg, 10, 10, now)
	messages, err := db.ListHumanMessages()
	require.NoError(t, err)
	assert.Len(t, messages, 4)
	evaluateSpendLimits(cfg, 10, 10, now.Add(2*time.Minute))
	messages, err = db.ListHumanMessages()
	require.NoError(t, err)
	assert.Len(t, messages, 8)
	evaluateSpendLimits(cfg, 11, 11, now.Add(24*time.Hour))
	messages, err = db.ListHumanMessages()
	require.NoError(t, err)
	assert.Len(t, messages, 10) // new day, same month
}

func TestDashboardConfig_SpendLimits(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	w, resp := postConfig(t, "/api/config", wrapBody(`{"cost":{"daily_limit_usd":10,"monthly_limit_usd":200}}`, ""))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, resp.Raw, `"daily_limit_usd": 10`)
	assert.Contains(t, resp.Raw, `"monthly_limit_usd": 200`)
	w, resp = postConfig(t, "/api/config", wrapBody(`{"cost":{"daily_limit_usd":-1}}`, ""))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assertErrorContains(t, resp.Errors, "cost.daily_limit_usd")
}
