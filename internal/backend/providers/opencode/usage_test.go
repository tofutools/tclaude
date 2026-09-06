package opencode

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestCollectOpenCodeUsagePreservesNativeDecimalCost(t *testing.T) {
	raw := []byte(`{"info":{"time":{"updated":1788743045000}},"messages":[
{"info":{"role":"user"}},
{"info":{"role":"assistant","cost":0.0105,"tokens":{"input":100,"output":20,"reasoning":3,"cache":{"read":40,"write":2}}}},
{"info":{"role":"assistant","cost":0.00225,"tokens":{"input":50,"output":10,"reasoning":1,"cache":{"read":5,"write":1}}}}
]}`)
	counters, cost, observed, partial, err := collectOpenCodeUsage(raw)
	require.NoError(t, err)
	require.False(t, partial)
	require.Equal(t, "0.01275", cost.Amount)
	require.Equal(t, "USD", cost.Currency)
	require.Equal(t, model.UsageCostNativeReported, cost.Kind)
	require.Equal(t, int64(150), counterValue(counters, model.UsageInputTokens))
	require.Equal(t, int64(30), counterValue(counters, model.UsageOutputTokens))
	require.Equal(t, int64(45), counterValue(counters, model.UsageCacheReadTokens))
	require.Equal(t, "2026-09-07T01:04:05Z", observed.Format("2006-01-02T15:04:05Z"))
}

func counterValue(counters []model.UsageCounter, unit model.UsageUnit) int64 {
	for _, counter := range counters {
		if counter.Unit == unit {
			return counter.Value
		}
	}
	return -1
}
