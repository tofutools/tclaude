package app_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestUsageSummaryDoesNotSumCumulativeSnapshotsOrInventAttribution(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.db"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	write := func(id, key string, at time.Time, count int64, cost string, cumulative bool) {
		observation := model.UsageObservation{ID: model.UsageObservationID(id), Attribution: model.UsageAttribution{ConversationID: "conversation", Precision: model.UsageAttributionConversation}, Harness: "fixture", Source: "native.fixture", SourceRevision: id, ObservedAt: at, CollectedAt: at, Counters: []model.UsageCounter{{Unit: model.UsageInputTokens, Value: count}}, Cost: &model.UsageCost{Amount: cost, Currency: "USD", Kind: model.UsageCostNativeReported}, Coverage: model.UsageCoverage{Counters: model.UsageCoverageComplete, Cost: model.UsageCoverageComplete}}
		_, _, err := store.RecordUsage(ctx, app.UsageWrite{Observation: observation, SourceKey: key, Cumulative: cumulative})
		require.NoError(t, err)
	}
	write("baseline", "private-cumulative", start.Add(-time.Hour), 9007199254740993, "0.1", true)
	write("first", "private-cumulative", start.Add(time.Hour), 9007199254740998, "0.3", true)
	write("second", "private-cumulative", start.Add(2*time.Hour), 9007199254741005, "0.6", true)
	write("unbased", "private-no-baseline", start.Add(time.Hour), 9007199254740993, "99", true)
	write("event", "private-event", start.Add(time.Hour), 9007199254740993, "0.1", false)
	filter := app.UsageSummaryFilter{After: start, Before: start.Add(24 * time.Hour)}
	result, err := service.SummarizeUsage(ctx, app.UsageSummaryRequest{Principal: model.OperatorPrincipal(), Filter: filter})
	require.NoError(t, err)
	require.Equal(t, 4, result.Observations)
	require.Len(t, result.Rows, 2)
	for _, row := range result.Rows {
		require.Empty(t, row.Attribution.AgentID)
		require.Empty(t, row.Attribution.ExecutionID)
		if row.Cumulative {
			require.Equal(t, "12", row.Counters[0].Value)
			require.Equal(t, "0.5", row.Costs[0].Amount)
			require.Equal(t, 1, row.MissingBaselines)
		} else {
			require.Equal(t, "9007199254740993", row.Counters[0].Value)
			require.Equal(t, "0.1", row.Costs[0].Amount)
		}
	}
	require.Len(t, result.Gaps, 1)
	_, err = service.SummarizeUsage(ctx, app.UsageSummaryRequest{Principal: model.AgentPrincipal("agent"), Filter: filter})
	require.ErrorIs(t, err, app.ErrUnauthorized)
	filter.AgentID = "agent"
	empty, err := service.SummarizeUsage(ctx, app.UsageSummaryRequest{Principal: model.OperatorPrincipal(), Filter: filter})
	require.NoError(t, err)
	require.Empty(t, empty.Rows, "conversation precision must not inherit launching agents")
	// A newer value after the selected period must not alter its prior summary.
	write("later", "private-cumulative", start.Add(48*time.Hour), 9007199254741010, "0.8", true)
	filter.AgentID = ""
	repeat, err := service.SummarizeUsage(ctx, app.UsageSummaryRequest{Principal: model.OperatorPrincipal(), Filter: filter})
	require.NoError(t, err)
	require.Equal(t, result, repeat)
}

func TestUsageSummaryKeepsResetUnknownAndHistoricalLedgersSeparate(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.db"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	start := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	sample := func(id, key string, offset time.Duration, count int64, cost *model.UsageCost, historical bool, coverage model.UsageCoverageState) {
		obs := model.UsageObservation{ID: model.UsageObservationID(id), Harness: "fixture", Source: "recorded", SourceRevision: id, Attribution: model.UsageAttribution{ConversationID: "conversation", Precision: model.UsageAttributionConversation}, ObservedAt: start.Add(offset), CollectedAt: start.Add(offset), Counters: []model.UsageCounter{{Unit: model.UsageOutputTokens, Value: count}}, Cost: cost, Historical: historical, Coverage: model.UsageCoverage{Counters: coverage, Cost: model.UsageCoverageComplete}}
		_, _, err := store.RecordUsage(ctx, app.UsageWrite{Observation: obs, SourceKey: key, Cumulative: !historical})
		require.NoError(t, err)
	}
	usd := func(amount string) *model.UsageCost {
		return &model.UsageCost{Amount: amount, Currency: "USD", Kind: model.UsageCostNativeReported}
	}
	sample("base", "live", -time.Hour, 100, usd("10"), false, model.UsageCoverageComplete)
	sample("reset", "live", time.Hour, 20, usd("2"), false, model.UsageCoverageComplete)
	sample("after-reset", "live", 2*time.Hour, 25, usd("3"), false, model.UsageCoverageComplete)
	sample("unknown", "other", time.Hour, 900, nil, false, model.UsageCoverageUnknown)
	sample("historical", "import", time.Hour, 7, &model.UsageCost{Amount: "4.00", Currency: "EUR", Kind: model.UsageCostHistoricalEstimate}, true, model.UsageCoverageComplete)
	result, err := service.SummarizeUsage(ctx, app.UsageSummaryRequest{Principal: model.OperatorPrincipal(), Filter: app.UsageSummaryFilter{After: start, Before: start.Add(24 * time.Hour)}})
	require.NoError(t, err)
	require.Len(t, result.Rows, 2)
	for _, row := range result.Rows {
		if row.Historical {
			require.Equal(t, "7", row.Counters[0].Value)
			require.Equal(t, "EUR", row.Costs[0].Currency)
			require.Equal(t, model.UsageCostHistoricalEstimate, row.Costs[0].Kind)
		} else {
			require.Equal(t, "5", row.Counters[0].Value)
			require.Equal(t, "1", row.Costs[0].Amount)
			require.Equal(t, 2, row.Resets)
			require.Equal(t, 1, row.MissingBaselines)
		}
	}
	require.Len(t, result.Gaps, 3)
}

func TestUsageSummaryBaselineSurvivesInterveningIncompatibleLedger(t *testing.T) {
	for _, inside := range []bool{false, true} {
		for _, dimension := range []string{"event", "harness", "source", "historical", "attribution"} {
			t.Run(fmt.Sprintf("%s/inside=%t", dimension, inside), func(t *testing.T) {
				ctx := context.Background()
				store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.db"))
				require.NoError(t, err)
				defer store.Close()
				start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
				baseline := model.UsageObservation{ID: "baseline", SourceRevision: "baseline", Harness: "fixture", Source: "native", Attribution: model.UsageAttribution{ConversationID: "conversation", Precision: model.UsageAttributionConversation}, ObservedAt: start.Add(-2 * time.Hour), CollectedAt: start.Add(-2 * time.Hour), Counters: []model.UsageCounter{{Unit: model.UsageInputTokens, Value: 10}}, Coverage: model.UsageCoverage{Counters: model.UsageCoverageComplete}}
				write := func(observation model.UsageObservation, cumulative bool) {
					_, _, err := store.RecordUsage(ctx, app.UsageWrite{SourceKey: "shared-source-key", Cumulative: cumulative, Observation: observation})
					require.NoError(t, err)
				}
				write(baseline, true)
				other := baseline
				other.ID = "other"
				other.SourceRevision = "other"
				other.Counters = []model.UsageCounter{{Unit: model.UsageInputTokens, Value: 2}}
				other.ObservedAt = start.Add(-time.Hour)
				if inside {
					other.ObservedAt = start.Add(30 * time.Minute)
				}
				other.CollectedAt = other.ObservedAt
				switch dimension {
				case "harness":
					other.Harness = "other"
				case "source":
					other.Source = "other"
				case "historical":
					other.Historical = true
				case "attribution":
					other.Attribution.ConversationID = "other"
				}
				write(other, dimension != "event")
				current := baseline
				current.ID = "current"
				current.SourceRevision = "current"
				current.ObservedAt = start.Add(time.Hour)
				current.CollectedAt = current.ObservedAt
				current.Counters = []model.UsageCounter{{Unit: model.UsageInputTokens, Value: 15}}
				write(current, true)
				result, err := app.New(store, providers.NewRegistry()).SummarizeUsage(ctx, app.UsageSummaryRequest{Principal: model.OperatorPrincipal(), Filter: app.UsageSummaryFilter{After: start, Before: start.Add(24 * time.Hour)}})
				require.NoError(t, err)
				found := false
				for _, row := range result.Rows {
					if row.Cumulative && row.Harness == baseline.Harness && row.Source == baseline.Source && !row.Historical && row.Attribution == baseline.Attribution {
						require.Equal(t, []app.ExactUsageCounter{{Unit: model.UsageInputTokens, Value: "5"}}, row.Counters)
						require.Zero(t, row.MissingBaselines)
						found = true
					}
				}
				require.True(t, found)
			})
		}
	}
}
