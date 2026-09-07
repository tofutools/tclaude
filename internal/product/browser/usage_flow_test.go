package browser

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

type usageBrowserProvider struct {
	accessBrowserProvider
	revision atomic.Int64
}

func (p *usageBrowserProvider) Usage() ports.UsageReader { return usageBrowserReader{p: p} }

type usageBrowserReader struct{ p *usageBrowserProvider }

func (usageBrowserReader) Capabilities() ports.UsageCapabilities {
	return ports.UsageCapabilities{CollectCounters: true, CollectCost: true, CostKind: model.UsageCostNativeReported}
}
func (r usageBrowserReader) Collect(_ context.Context, req ports.UsageCollectionRequest) (ports.CollectedUsage, error) {
	revision := r.p.revision.Load()
	return ports.CollectedUsage{SourceKey: "fixture:" + string(req.Execution.ConversationID), Source: "fixture cumulative", SourceRevision: fmt.Sprint(revision), ObservedAt: time.Date(2026, 1, 2, 12, 0, int(revision), 0, time.UTC), Counters: []model.UsageCounter{{Unit: model.UsageInputTokens, Value: 9007199254740991 + revision}, {Unit: model.UsageOutputTokens, Value: revision * 20}}, Cost: &model.UsageCost{Amount: "0.123456789", Currency: "USD", Kind: model.UsageCostNativeReported}, Coverage: model.UsageCoverage{Counters: model.UsageCoverageComplete, Cost: model.UsageCoverageComplete}, Cumulative: true, Attribution: model.UsageAttributionConversation}, nil
}

func TestBrowserUsageReadingsFilterChartAndDeduplicate(t *testing.T) {
	p := &usageBrowserProvider{accessBrowserProvider: accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 2)}}
	p.revision.Store(1)
	ctx, page, operator := processEditorBrowserWithSetup(t, func(state string) {
		store, err := sqlite.Open(filepath.Join(state, "backend.sqlite"))
		require.NoError(t, err)
		defer store.Close()
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		_, err = store.CatalogHistory(context.Background(), "claude", "offline", []app.HistoryCatalogWrite{{Entry: model.HistoryCatalogEntry{ConversationID: "historical_only", Harness: "claude", Title: "Imported history", Availability: model.HistoryMetadataOnly}, Native: model.NativeConversationEvidence{Namespace: "fixture", Reference: "historical"}, SourceToken: "fixture", SourceFingerprint: "fixture"}}, model.HistoryCoverage{}, at)
		require.NoError(t, err)
		_, _, err = store.ImportHistoricalUsage(context.Background(), app.HistoricalUsageWrite{SourceKey: "legacy:fixture", Cumulative: true, Observation: model.UsageObservation{ID: "usage_imported", Attribution: model.UsageAttribution{ConversationID: "historical_only", Precision: model.UsageAttributionConversation}, Harness: "claude", Source: "imported fixture", SourceRevision: "snapshot-1", ObservedAt: at, CollectedAt: at, Historical: true, Provenance: "synthetic offline fixture", Counters: []model.UsageCounter{{Unit: model.UsageInputTokens, Value: 9007199254740993}}, Coverage: model.UsageCoverage{Counters: model.UsageCoveragePartial, Cost: model.UsageCoverageUnknown}}})
		require.NoError(t, err)
	}, p)
	desired := model.DesiredConfiguration{Harness: p.Name(), Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "usage_agent", "name": "Usage agent", "desired": desired}, nil))
	var launched app.OperationResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "launch_usage", "target": map[string]any{"agent": map[string]any{"agent_id": "usage_agent", "expected_revision": 1}}}, &launched))
	target := app.UsageTarget{ExecutionID: launched.Execution.ID}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/usage/refresh", map[string]any{"target": target}, nil))
	p.revision.Store(2)
	require.NoError(t, operator.Call(ctx, "POST", "/v2/usage/refresh", map[string]any{"target": target}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElement("[data-tab=usage]").MustClick()
	page.MustElement("[aria-label='Usage target']").MustSelect("Execution: Usage agent · " + string(launched.Execution.ID))
	page.MustElementR("#usage-list [role=status]", "1 recorded readings loaded")
	require.Len(t, page.MustElements("#usage-list [data-usage]"), 1)
	page.MustElementR("#usage-list [data-usage] p", "input tokens: 9007199254740993")
	page.MustElementR("#usage-list [data-usage] strong", "conversation")
	page.MustElement("[aria-label='Usage chart unit']").MustSelect("output tokens")
	require.Equal(t, 40.0, page.MustElement("#usage-list meter").MustProperty("value").Num())
	page.MustElementR("#usage-list p", "0.123456789 USD")
	page.MustElementR("#usage-list button", "^Refresh native usage$").MustClick()
	page.MustElementR("#usage-list [role=status]", "1 recorded readings loaded")
	require.Len(t, page.MustElements("#usage-list [data-usage]"), 1)
	page.MustEval(`() => {window.exported='';const original=URL.createObjectURL;URL.createObjectURL=b=>{b.text().then(t=>window.exported=t);return original(b)};document.addEventListener('click',e=>{if(e.target.download)e.preventDefault()})}`)
	page.MustElementR("#usage-list button", "^Export loaded usage JSON$").MustClick()
	page.MustWait(`() => window.exported.includes('0.123456789')`)
	require.Equal(t, 1, page.MustEval(`() => JSON.parse(window.exported).observations.length`).Int())
	require.Equal(t, "9007199254740993", page.MustEval(`() => JSON.parse(window.exported).observations[0].Counters.find(c=>c.Unit==='input_tokens').Value`).Str())
	page.MustElement("[aria-label='Usage target']").MustSelect("Conversation: historical_only")
	page.MustElementR("#usage-list [data-usage] p", "imported fixture")
	page.MustElementR("#usage-list p", "has no recorded execution")
	require.Equal(t, 0, page.MustEval(`() => [...document.querySelectorAll('#usage-list button')].filter(b=>b.textContent==='Refresh native usage').length`).Int())
	page.MustElementR("#usage-list button", "^Export loaded usage JSON$").MustClick()
	page.MustWait(`() => window.exported.includes('usage_imported')`)
	require.Equal(t, "9007199254740993", page.MustEval(`() => JSON.parse(window.exported).observations[0].Counters[0].Value`).Str())
	// Input dates are browser local time; use the browser to select a range after the fixture observation.
	page.MustEval(`() => {document.querySelector('[aria-label="Usage observed from"]').value='2027-01-01T00:00';document.querySelector('[aria-label="Usage observed before"]').value='2027-02-01T00:00'}`)
	page.MustElementR("#usage-list form button", "^Apply observed-time range$").MustClick()
	page.MustElementR("#usage-list [role=status]", "0 recorded readings loaded")
	page.MustElementR("#usage-list p", "Missing usage is not zero usage")
	page.MustEval(`() => {document.querySelector('[aria-label="Usage observed before"]').value='2026-01-01T00:00'}`)
	page.MustElementR("#usage-list form button", "^Apply observed-time range$").MustClick()
	page.MustElementR("#usage-list [role=status]", "must be later")
}
