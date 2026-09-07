package browser

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestBrowserUsageSummaryRetainsExactSourceTotalsAndGaps(t *testing.T) {
	ctx, page, operator := processEditorBrowserWithSetup(t, func(state string) {
		store, err := sqlite.Open(filepath.Join(state, "backend.sqlite"))
		require.NoError(t, err)
		defer store.Close()
		at := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
		for _, sample := range []struct {
			id, key    string
			delta      time.Duration
			count      int64
			cumulative bool
		}{{"before", "private-source", -24 * time.Hour, 20, true}, {"after", "private-source", 0, 30, true}, {"event", "private-event", 0, 9007199254740993, false}, {"gap", "private-gap", 0, 99, true}} {
			_, _, err := store.RecordUsage(context.Background(), app.UsageWrite{SourceKey: sample.key, Cumulative: sample.cumulative, Observation: model.UsageObservation{ID: model.UsageObservationID(sample.id), Source: "fixture", SourceRevision: sample.id, Harness: "claude", Attribution: model.UsageAttribution{ConversationID: "retained-conversation", Precision: model.UsageAttributionConversation}, ObservedAt: at.Add(sample.delta), CollectedAt: at.Add(sample.delta), Counters: []model.UsageCounter{{Unit: model.UsageInputTokens, Value: sample.count}}, Cost: &model.UsageCost{Amount: "0.1", Currency: "USD", Kind: model.UsageCostNativeReported}, Coverage: model.UsageCoverage{Counters: model.UsageCoverageComplete, Cost: model.UsageCoverageComplete}}})
			require.NoError(t, err)
		}
	})
	page.MustElement("[data-tab=usage]").MustClick()
	page.MustEval(`()=>{document.querySelector('[aria-label="Observed from (UTC)"]').value='2026-01-02';document.querySelector('[aria-label="Observed before (UTC, exclusive)"]').value='2026-01-03'}`)
	page.MustElement("#usage-summary > summary").MustClick()
	page.MustElementR("#usage-summary [role=status]", "3 source observations")
	page.MustElement("#usage-summary [aria-label='Accounting source']").MustSelect("claude · fixture · native · event readings · conversation")
	page.MustElementR("#usage-summary .usage-summary-total", "9007199254740993")
	require.Contains(t, *page.MustElement("#usage-summary .cost-col").MustAttribute("aria-label"), "9007199254740993")
	page.MustEval(`()=>{window.summaryExport='';const original=URL.createObjectURL;URL.createObjectURL=b=>{b.text().then(v=>window.summaryExport=v);return original(b)};document.addEventListener('click',e=>{if(e.target.download)e.preventDefault()})}`)
	page.MustElementR("#usage-summary button", "^Export summary JSON$").MustClick()
	page.MustWait(`()=>window.summaryExport.includes('9007199254740993')`)
	require.NotContains(t, page.MustEval(`()=>window.summaryExport`).Str(), "private-source")
	page.MustElement("#usage-summary [aria-label='Accounting source']").MustSelect("claude · fixture · native · cumulative changes · conversation")
	page.MustElementR("#usage-summary .usage-summary-total", "Recorded total for this source and filter: 10")
	page.MustElementR("#usage-summary details summary", "Excluded observations").MustClick()
	page.MustElementR("#usage-summary details p", "no compatible prior baseline")
	var response app.UsageSummaryResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/usage/summary", map[string]any{"filter": app.UsageSummaryFilter{After: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Before: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)}}, &response))
	require.Equal(t, 3, response.Observations)
	// Empty calendar days stay on the axis and are unknown, not zero.
	page.MustEval(`()=>{document.querySelector('[aria-label="Observed from (UTC)"]').value='2026-01-02';document.querySelector('[aria-label="Observed before (UTC, exclusive)"]').value='2026-04-02'}`)
	page.MustElementR("#usage-summary button", "^Load summary$").MustClick()
	page.MustElementR("#usage-summary [role=status]", "3 source observations")
	page.MustElement("#usage-summary [aria-label='Accounting source']").MustSelect("claude · fixture · native · event readings · conversation")
	page.MustWait(`()=>document.querySelectorAll('#usage-summary .cost-col').length===90`)
	require.Len(t, page.MustElements("#usage-summary .cost-col.unknown"), 89)
	require.Contains(t, *page.MustElement("#usage-summary [data-day='2026-01-03']").MustAttribute("aria-label"), "Unknown")
	require.Empty(t, page.MustElements("#usage-summary [data-day='2026-01-03'] .cost-seg"))
	require.Contains(t, *page.MustElement("#usage-summary [data-day='2026-01-02']").MustAttribute("aria-label"), "9007199254740993")
	page.MustElement("#logout").MustClick()
	page.MustWait(`()=>document.querySelector('#connection').textContent==='Signed out'`)
	require.NotContains(t, page.MustElement("#usage-summary").MustText(), "9007199254740993")
}
