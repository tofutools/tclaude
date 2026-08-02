package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func float64ptr(value float64) *float64 { return &value }

func resetOpenCodeVirtualCostStateForTest() {
	openCodeVirtualCostState.Lock()
	openCodeVirtualCostState.bySession = nil
	openCodeVirtualCostState.usageSession = nil
	openCodeVirtualCostState.hydratedSession = nil
	openCodeVirtualCostState.pendingRemovals = nil
	openCodeVirtualCostState.knownSteps = nil
	openCodeVirtualCostState.snapshotSteps = nil
	openCodeVirtualCostState.removalRetries = nil
	openCodeVirtualCostState.trackedSessions = nil
	openCodeVirtualCostState.nativeCosts = nil
	openCodeVirtualCostState.retiredNativeCost = nil
	openCodeVirtualCostState.Unlock()
}

func TestOpenCodeVirtualCostForUsageUsesNativeTiersAndCachePricing(t *testing.T) {
	tier := openCodePriceTier{
		Input: 4, Output: 20, Cache: openCodeCachePrice{Read: 0.4, Write: 8},
	}
	tier.Tier.Type, tier.Tier.Size = "context", 200_000
	base := openCodeModelPrice{
		Input: 2, Output: 10, Cache: openCodeCachePrice{Read: 0.2, Write: 0.5},
		Tiers: []openCodePriceTier{tier},
		ExperimentalOver200K: &struct {
			Input  float64            `json:"input"`
			Output float64            `json:"output"`
			Cache  openCodeCachePrice `json:"cache"`
		}{Input: 3, Output: 12, Cache: openCodeCachePrice{Read: 0.3, Write: 0.6}},
	}
	usage := openCodeContextUsage{
		Input: 100_000, Output: 10_000, Reasoning: 5_000, CacheRead: 120_000, CacheWrite: 1_000,
	}
	got, ok := openCodeVirtualCostForUsage(
		usage, base, config.DefaultOpenCodeLegacyLongContextPricingCutoff)
	require.True(t, ok)
	assert.InDelta(t, 0.756, got, 1e-12,
		"explicit >200k context tier wins and prices reasoning as output plus both cache buckets")

	base.Tiers = nil
	got, ok = openCodeVirtualCostForUsage(
		usage, base, config.DefaultOpenCodeLegacyLongContextPricingCutoff)
	require.True(t, ok)
	assert.InDelta(t, 0.3745, got, 1e-12,
		"the 221k call remains base-priced below the default 272k cutoff")

	got, ok = openCodeVirtualCostForUsage(usage, base, 200_000)
	require.True(t, ok)
	assert.InDelta(t, 0.5166, got, 1e-12,
		"a configured cutoff selects legacy experimentalOver200K pricing")

	base.ExperimentalOver200K = nil
	got, ok = openCodeVirtualCostForUsage(
		usage, base, config.DefaultOpenCodeLegacyLongContextPricingCutoff)
	require.True(t, ok)
	assert.InDelta(t, 0.3745, got, 1e-12, "base pricing applies without a matching tier")

	got, ok = openCodeVirtualCostForUsage(
		usage, openCodeModelPrice{}, config.DefaultOpenCodeLegacyLongContextPricingCutoff)
	require.True(t, ok, "an explicitly cataloged free model is valid")
	assert.Zero(t, got)
	missing := projectOpenCodeMessageCost(openCodeContextUsage{
		MessageID: "missing", ProviderID: "openai", ModelID: "not-cataloged",
		ReportedCost: float64ptr(0), Input: 1,
	}, map[string]openCodeModelPrice{}, config.DefaultOpenCodeLegacyLongContextPricingCutoff)
	assert.False(t, missing.eligible, "an absent catalog entry still degrades without inventing a price")
}

func TestOpenCodeLegacyLongContextPricingCutoffBoundaries(t *testing.T) {
	legacy := &struct {
		Input  float64            `json:"input"`
		Output float64            `json:"output"`
		Cache  openCodeCachePrice `json:"cache"`
	}{Input: 2}
	base := openCodeModelPrice{Input: 1, ExperimentalOver200K: legacy}

	for _, tc := range []struct {
		name       string
		cutoff     int64
		input      int64
		wantPerTok float64
	}{
		{"default exactly at boundary", config.DefaultOpenCodeLegacyLongContextPricingCutoff, 272_000, 1},
		{"default just over boundary", config.DefaultOpenCodeLegacyLongContextPricingCutoff, 272_001, 2},
		{"override exactly at boundary", 180_000, 180_000, 1},
		{"override just over boundary", 180_000, 180_001, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := openCodeVirtualCostForUsage(
				openCodeContextUsage{Input: tc.input}, base, tc.cutoff)
			require.True(t, ok)
			assert.InDelta(t, float64(tc.input)*tc.wantPerTok/1_000_000, got, 1e-12)
		})
	}
}

func TestOpenCodeLegacyLongContextPricingCutoffReadsConfig(t *testing.T) {
	setupTestDB(t)
	assert.Equal(t, config.DefaultOpenCodeLegacyLongContextPricingCutoff,
		openCodeLegacyLongContextPricingCutoff())

	configured := int64(180_000)
	cfg := config.DefaultConfig()
	cfg.OpenCode = &config.OpenCodeConfig{LegacyLongContextPricingCutoff: &configured}
	require.NoError(t, config.Save(cfg))
	assert.Equal(t, configured, openCodeLegacyLongContextPricingCutoff())

	invalid := int64(-1)
	cfg.OpenCode.LegacyLongContextPricingCutoff = &invalid
	require.NoError(t, config.Save(cfg), "hand-edited invalid config can bypass dashboard validation")
	assert.Equal(t, config.DefaultOpenCodeLegacyLongContextPricingCutoff,
		openCodeLegacyLongContextPricingCutoff())
}

func TestOpenCodeExplicitContextTierWinsOverConfiguredLegacyCutoff(t *testing.T) {
	tier := openCodePriceTier{Input: 4}
	tier.Tier.Type, tier.Tier.Size = "context", 200_000
	base := openCodeModelPrice{
		Input: 1,
		Tiers: []openCodePriceTier{tier},
		ExperimentalOver200K: &struct {
			Input  float64            `json:"input"`
			Output float64            `json:"output"`
			Cache  openCodeCachePrice `json:"cache"`
		}{Input: 2},
	}

	got, ok := openCodeVirtualCostForUsage(
		openCodeContextUsage{Input: 250_000}, base, 100_000)
	require.True(t, ok)
	assert.InDelta(t, 1.0, got, 1e-12,
		"the matching explicit tier wins even when the configured legacy cutoff also matches")
}

func TestOpenCodeVirtualCostPricesEachStepBeforeApplyingContextTier(t *testing.T) {
	zero := float64(0)
	state := openCodeMessageCostUsage{
		message: openCodeContextUsage{
			MessageID: "msg-tiered", ProviderID: "openai", ModelID: "gpt-tiered",
			ReportedCost: &zero,
		},
		hadSteps: true,
		steps: map[string]openCodeContextUsage{
			"part-1": {MessageID: "msg-tiered", ReportedCost: &zero, Input: 200_000},
			"part-2": {MessageID: "msg-tiered", ReportedCost: &zero, Input: 200_000},
		},
	}
	projected := projectOpenCodeMessageCostUsage(state, map[string]openCodeModelPrice{
		"openai/gpt-tiered": {
			Input: 1,
			ExperimentalOver200K: &struct {
				Input  float64            `json:"input"`
				Output float64            `json:"output"`
				Cache  openCodeCachePrice `json:"cache"`
			}{Input: 2},
		},
	}, config.DefaultOpenCodeLegacyLongContextPricingCutoff)
	require.True(t, projected.eligible)
	assert.InDelta(t, 0.4, projected.usd, 1e-12,
		"two sub-threshold model calls remain base-priced instead of becoming one legacy-tier 400k call")
}

func TestApplyOpenCodeVirtualCostUsageIsReplaySafeAndHandlesModelChanges(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/config/providers", r.URL.Path)
		_, _ = w.Write([]byte(`{"providers":[{"id":"openai","models":{` +
			`"gpt-a":{"cost":{"input":1,"output":2,"cache":{"read":0.1,"write":0.2}},"limit":{"context":200000}},` +
			`"gpt-b":{"cost":{"input":2,"output":4,"cache":{"read":0.2,"write":0.4}},"limit":{"context":200000}},` +
			`"free":{"cost":{"input":0,"output":0,"cache":{"read":0,"write":0}},"limit":{"context":200000}}}}]}`))
	}))
	t.Cleanup(server.Close)
	const sessionID, convID = "oc-virtual", "ses-virtual"
	seedOpenCodeUsageSession(t, sessionID, convID)
	runtime := db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: convID, ServerURL: server.URL, Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
	}
	_, directPrices, fetchErr := fetchOpenCodeModelCatalog(context.Background(), runtime)
	require.NoError(t, fetchErr)
	require.Contains(t, directPrices, "openai/gpt-a")
	prices, loaded := openCodeModelPrices(context.Background(), runtime)
	require.True(t, loaded)
	require.Contains(t, prices, "openai/gpt-a")
	subscription := float64ptr(0)
	first := openCodeContextUsage{
		MessageID: "msg-1", ProviderID: "openai", ModelID: "gpt-a",
		ReportedCost: subscription, Input: 1_000_000, CreatedAt: time.Now().Add(-time.Minute),
	}
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, first)
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, first)
	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 1, snap.VirtualCostUSD, 1e-12, "replayed update replaces, never increments")

	second := openCodeContextUsage{
		MessageID: "msg-2", ProviderID: "openai", ModelID: "gpt-b",
		ReportedCost: subscription, Input: 500_000, CreatedAt: time.Now(),
	}
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, second)
	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 2, snap.VirtualCostUSD, 1e-12, "messages on different models use their own prices")

	first.Input = 2_000_000
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, first)
	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 3, snap.VirtualCostUSD, 1e-12, "a corrected repeated message replaces its prior contribution")

	first.Input = 250_000
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, first)
	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 1.25, snap.VirtualCostUSD, 1e-12,
		"an authoritative lower replay clears the earlier overestimate")

	first.ModelID = "missing-price"
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, first)
	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 1.25, snap.VirtualCostUSD, 1e-12,
		"missing pricing retains the last complete estimate instead of publishing a partial total")

	first.ModelID = "gpt-a"
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, first)
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, openCodeContextUsage{
		MessageID: "msg-free", ProviderID: "openai", ModelID: "free",
		ReportedCost: subscription, Input: 1_000_000, CreatedAt: time.Now(),
	})
	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 1.25, snap.VirtualCostUSD, 1e-12,
		"a valid free-model message contributes zero without erasing earlier priced usage")

	rows, err := db.OpenCodeUsageActivityBetween(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Len(t, rows, 3, "replays are also idempotent in provider activity history")
}

func TestOpenCodeModelCatalogFallsBackForZeroPricedOpenAISubscription(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/config/providers", r.URL.Path)
		_, _ = w.Write([]byte(`{"providers":[` +
			`{"id":"openai","models":{"gpt-5.6-sol":{"cost":{"input":0,"output":0,"cache":{"read":0,"write":0}},"limit":{"context":1050000}}}},` +
			`{"id":"local","models":{"free":{"cost":{"input":0,"output":0,"cache":{"read":0,"write":0}},"limit":{"context":200000}}}}]}`))
	}))
	t.Cleanup(server.Close)
	runtime := db.OpenCodeRuntime{
		SessionID: "oc-zero-price", ConvID: "ses-zero-price", ServerURL: server.URL,
		Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
	}

	_, prices, err := fetchOpenCodeModelCatalog(context.Background(), runtime)
	require.NoError(t, err)
	price := prices["openai/gpt-5.6-sol"]
	assert.Equal(t, 5.0, price.Input)
	assert.Equal(t, 30.0, price.Output)
	assert.Equal(t, 0.5, price.Cache.Read)
	assert.Equal(t, 6.25, price.Cache.Write)
	require.Len(t, price.Tiers, 1)
	assert.Equal(t, float64(harness.OpenAIShortContextInputMax), price.Tiers[0].Tier.Size)
	assert.Equal(t, openCodeModelPrice{}, prices["local/free"],
		"zero-priced non-OpenAI models remain genuinely free")

	zero := 0.0
	projected := projectOpenCodeMessageCost(openCodeContextUsage{
		MessageID: "msg", ProviderID: "openai", ModelID: "gpt-5.6-sol", ReportedCost: &zero,
		Input: 1_000, Output: 100, Reasoning: 50, CacheRead: 200, CacheWrite: 10,
	}, prices, config.DefaultOpenCodeLegacyLongContextPricingCutoff)
	require.True(t, projected.eligible)
	assert.InDelta(t, 0.0096625, projected.usd, 1e-12)
}

func TestOpenCodeVirtualCostRetainsHistoryAcrossTransientCatalogFailure(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)
	const sessionID, convID = "oc-transient", "ses-transient"
	seedOpenCodeUsageSession(t, sessionID, convID)
	require.NoError(t, db.UpdateSessionVirtualCost(sessionID, 2), "seed retained projection")

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(failing.Close)
	runtime := db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: convID, ServerURL: failing.URL,
		Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
	}
	yesterdayAt := time.Now().AddDate(0, 0, -1)
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, openCodeContextUsage{
		MessageID: "msg-recovered", ProviderID: "openai", ModelID: "gpt-a",
		ReportedCost: float64ptr(0), Input: 1_000_000, CreatedAt: yesterdayAt,
	})
	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 2, snap.VirtualCostUSD, 1e-12,
		"a failed catalog fetch is not treated as authoritative missing pricing")

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"providers":[{"id":"openai","models":{"gpt-a":{` +
			`"cost":{"input":1,"output":2,"cache":{"read":0.1,"write":0.2}},"limit":{"context":200000}}}}]}`))
	}))
	t.Cleanup(healthy.Close)
	runtime.ServerURL = healthy.URL
	projectAndPersistOpenCodeCostState(context.Background(), runtime)

	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 1, snap.VirtualCostUSD, 1e-12)
	rows, err := db.AllCostDailyRows()
	require.NoError(t, err)
	yesterday := yesterdayAt.In(time.Local).Format("2006-01-02")
	var yesterdayCost, todayCost float64
	for _, row := range rows {
		if row.SessionID != sessionID {
			continue
		}
		switch row.Day {
		case yesterday:
			yesterdayCost = row.VirtualCostUSD
		case time.Now().Format("2006-01-02"):
			todayCost = row.VirtualCostUSD
		}
	}
	assert.InDelta(t, 1, yesterdayCost, 1e-12,
		"successful recovery rebuilds the original day from the message timestamp")
	assert.Zero(t, todayCost, "recovery does not move prior spend into today")
}

func TestOpenCodeVirtualCostWaitsForResumeSessionRow(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"providers":[{"id":"openai","models":{"gpt-a":{` +
			`"cost":{"input":1,"output":2,"cache":{"read":0.1,"write":0.2}},"limit":{"context":200000}}}}]}`))
	}))
	t.Cleanup(server.Close)
	const sessionID, convID = "oc-resume-race", "ses-resume-race"
	runtime := db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: convID, ServerURL: server.URL,
		Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
	}
	openCodeVirtualCostState.Lock()
	openCodeVirtualCostState.hydratedSession = map[string]bool{sessionID: true}
	openCodeVirtualCostState.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		applyOpenCodeVirtualCostUsage(ctx, runtime, openCodeContextUsage{
			MessageID: "msg-recovered", ProviderID: "openai", ModelID: "gpt-a",
			ReportedCost: float64ptr(0), Input: 1_000_000, CreatedAt: time.Now(),
		})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("cost projection returned before the resume session row was inserted")
	case <-time.After(2 * openCodeHookRowRetryDelay):
	}
	seedOpenCodeUsageSession(t, sessionID, convID)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("cost projection did not retry after the resume session row was inserted")
	}

	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 1, snap.VirtualCostUSD, 1e-12,
		"the authoritative resume backfill persists without waiting for a later message")
}

func TestOpenCodeTelemetryTargetsLaunchEnrollmentConversationRow(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"providers":[{"id":"openai","models":{"gpt-5.6-sol":{` +
			`"cost":{"input":5,"output":30,"cache":{"read":0.5,"write":6.25}},` +
			`"limit":{"context":1050000}}}}]}`))
	}))
	t.Cleanup(server.Close)

	const runtimeLabel, convID = "spwn-opencode", "ses_opencode"
	seedOpenCodeUsageSession(t, convID, convID)
	runtime := db.OpenCodeRuntime{
		SessionID: runtimeLabel, ConvID: convID, ServerURL: server.URL,
		Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
	}
	openCodeVirtualCostState.Lock()
	openCodeVirtualCostState.hydratedSession = map[string]bool{runtimeLabel: true}
	openCodeVirtualCostState.Unlock()

	usage := openCodeContextUsage{
		MessageID: "msg-sol", ProviderID: "openai", ModelID: "gpt-5.6-sol",
		ReportedCost: float64ptr(0), Input: 10_000, Output: 100, CreatedAt: time.Now(),
	}
	persistOpenCodeContextUsage(context.Background(), runtime, usage)
	persistOpenCodeRuntimeMetadata(runtime, usage)
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, usage)

	snap, err := db.GetContextSnapshot(convID)
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-5.6-sol", snap.Model)
	assert.Equal(t, int64(10_000), snap.TokensInput)
	assert.Equal(t, int64(100), snap.TokensOutput)
	assert.InDelta(t, 0.053, snap.VirtualCostUSD, 1e-12)
	exists, err := db.SessionExists(runtimeLabel)
	require.NoError(t, err)
	assert.False(t, exists, "the managed-server label is not a sessions row after launch enrollment")
}

func openCodeStepUpdatedEventJSON(convID, messageID, partID string, input int64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":"evt-%s","type":"message.part.updated","properties":{`+
		`"sessionID":%q,"part":{"id":%q,"messageID":%q,"sessionID":%q,"type":"step-finish",`+
		`"cost":0,"tokens":{"input":%d,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}}}`,
		partID, convID, partID, messageID, convID, input))
}

func openCodeRemovedEventJSON(eventType, convID, messageID, partID string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"id":"evt-rm","type":%q,"properties":{"sessionID":%q,"messageID":%q,"partID":%q}}`,
		eventType, convID, messageID, partID,
	))
}

func TestOpenCodeVirtualCostAggregatesStepFinishParts(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config/providers":
			_, _ = w.Write([]byte(`{"providers":[{"id":"openai","models":{"gpt-a":{` +
				`"cost":{"input":1,"output":2,"cache":{"read":0.1,"write":0.2}},"limit":{"context":200000}}}}]}`))
		case "/session/ses-steps/message":
			// OpenCode can retain the final step's stale top-level tokens while
			// correctly returning no surviving step-finish parts.
			_, _ = w.Write([]byte(`[{"info":{"id":"msg-tools","role":"assistant",` +
				`"providerID":"openai","modelID":"gpt-a","time":{"created":100},` +
				`"cost":0,"tokens":{"input":1000000,"output":0}},"parts":[]}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	runtime := db.OpenCodeRuntime{
		SessionID: "oc-steps", ConvID: "ses-steps", ServerURL: server.URL,
		Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
	}
	seedOpenCodeUsageSession(t, runtime.SessionID, runtime.ConvID)

	first, ok := parseOpenCodeStepCostUsage(
		openCodeStepUpdatedEventJSON(runtime.ConvID, "msg-tools", "part-1", 1_000_000),
		runtime.ConvID,
	)
	require.True(t, ok)
	applyOpenCodeVirtualCostStep(context.Background(), runtime, first)
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, openCodeContextUsage{
		MessageID: "msg-tools", ProviderID: "openai", ModelID: "gpt-a",
		ReportedCost: float64ptr(0), Input: 1_000_000,
	})

	second, ok := parseOpenCodeStepCostUsage(
		openCodeStepUpdatedEventJSON(runtime.ConvID, "msg-tools", "part-2", 2_000_000),
		runtime.ConvID,
	)
	require.True(t, ok)
	applyOpenCodeVirtualCostStep(context.Background(), runtime, second)
	applyOpenCodeVirtualCostStep(context.Background(), runtime, second)
	// OpenCode overwrites top-level message tokens with the latest step. The
	// stable parts must remain authoritative when that message update arrives.
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, openCodeContextUsage{
		MessageID: "msg-tools", ProviderID: "openai", ModelID: "gpt-a",
		ReportedCost: float64ptr(0), Input: 2_000_000,
	})

	snap, err := db.GetContextSnapshot(runtime.SessionID)
	require.NoError(t, err)
	assert.InDelta(t, 3, snap.VirtualCostUSD, 1e-12,
		"both model calls are priced once; the latest-step message field does not undercount")

	removal, ok := parseOpenCodeCostRemoval(
		openCodeRemovedEventJSON("message.part.removed", runtime.ConvID, "msg-tools", "part-2"),
		runtime.ConvID,
	)
	require.True(t, ok)
	applyOpenCodeVirtualCostRemoval(context.Background(), runtime, removal)
	snap, err = db.GetContextSnapshot(runtime.SessionID)
	require.NoError(t, err)
	assert.InDelta(t, 1, snap.VirtualCostUSD, 1e-12,
		"removing a live step rebuilds the message from its retained parts")

	removal, ok = parseOpenCodeCostRemoval(
		openCodeRemovedEventJSON("message.part.removed", runtime.ConvID, "msg-tools", "part-text"),
		runtime.ConvID,
	)
	require.True(t, ok)
	applyOpenCodeVirtualCostRemoval(context.Background(), runtime, removal)
	snap, err = db.GetContextSnapshot(runtime.SessionID)
	require.NoError(t, err)
	assert.InDelta(t, 1, snap.VirtualCostUSD, 1e-12,
		"removing an unrelated non-step part leaves projected cost unchanged")
	activity, err := db.OpenCodeUsageActivityBetween(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, activity, 1, "unrelated part removal leaves Usage coverage unchanged")

	removal, ok = parseOpenCodeCostRemoval(
		openCodeRemovedEventJSON("message.part.removed", runtime.ConvID, "msg-tools", "part-1"),
		runtime.ConvID,
	)
	require.True(t, ok)
	originalMark := markOpenCodePricingStepsRemoved
	t.Cleanup(func() { markOpenCodePricingStepsRemoved = originalMark })
	var markAttempts atomic.Int32
	markOpenCodePricingStepsRemoved = func(string, string, string, time.Time) error {
		if markAttempts.Add(1) == 1 {
			return errors.New("database busy")
		}
		return originalMark(runtime.ConvID, runtime.SessionID, "msg-tools", time.Now())
	}
	applyOpenCodeVirtualCostRemoval(context.Background(), runtime, removal)
	snap, err = db.GetContextSnapshot(runtime.SessionID)
	require.NoError(t, err)
	assert.InDelta(t, 1, snap.VirtualCostUSD, 1e-12,
		"a failed durable marker keeps the final step eligible so replay can retry")
	activity, err = db.OpenCodeUsageActivityBetween(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, activity, 1,
		"a failed durable marker does not clear Usage coverage only to resurrect it later")

	require.Eventually(t, func() bool {
		snapshot, snapshotErr := db.GetContextSnapshot(runtime.SessionID)
		return snapshotErr == nil && snapshot.VirtualCostUSD == 0
	}, 2*openCodeSSERetryDelay, 25*time.Millisecond,
		"the scheduled retry applies a one-shot final-step removal without SSE replay")
	snap, err = db.GetContextSnapshot(runtime.SessionID)
	require.NoError(t, err)
	assert.Zero(t, snap.VirtualCostUSD, "removing the final pricing step clears projected history")
	activity, err = db.OpenCodeUsageActivityBetween(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, activity, "final-step removal also clears the Usage coverage index")

	// Simulate an agentd restart plus local-session resume. The history endpoint
	// still exposes stale top-level tokens, but the conversation-scoped removal
	// marker must keep both projection and activity cleared.
	resetOpenCodeVirtualCostStateForTest()
	const resumedSessionID = "oc-steps-resumed"
	seedOpenCodeUsageSession(t, resumedSessionID, runtime.ConvID)
	resumed := runtime
	resumed.SessionID = resumedSessionID
	require.True(t, backfillOpenCodeContextUsage(context.Background(), resumed))
	snap, err = db.GetContextSnapshot(resumedSessionID)
	require.NoError(t, err)
	assert.Zero(t, snap.VirtualCostUSD,
		"reconnect and resume do not resurrect stale top-level usage after final-step removal")
	activity, err = db.OpenCodeUsageActivityBetween(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, activity, "reconnect and resume keep Usage coverage cleared")
}

func TestStaleOpenCodeProjectorGenerationCannotMutateUsageAfterBlockingCall(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)
	const sessionID, convID = "oc-stale-generation", "ses-stale-generation"
	seedOpenCodeUsageSession(t, sessionID, convID)

	oldGeneration := &openCodeProcess{}
	replacement := &openCodeProcess{}
	openCodeProcesses.Lock()
	openCodeProcesses.bySession[sessionID] = oldGeneration
	openCodeProcesses.Unlock()
	t.Cleanup(func() {
		openCodeProcesses.Lock()
		delete(openCodeProcesses.bySession, sessionID)
		openCodeProcesses.Unlock()
	})
	ctx := context.WithValue(context.Background(), openCodeSSEGenerationKey{}, oldGeneration)
	originalHook := afterOpenCodeStepMarkerClearTest
	t.Cleanup(func() { afterOpenCodeStepMarkerClearTest = originalHook })
	afterOpenCodeStepMarkerClearTest = func() {
		// Model a stop timeout followed by replacement while the old projector
		// was blocked in its durable marker call.
		openCodeProcesses.Lock()
		oldGeneration.stopping = true
		openCodeProcesses.bySession[sessionID] = replacement
		openCodeProcesses.Unlock()
	}

	applyOpenCodeVirtualCostStep(ctx, db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: convID,
	}, openCodeStepCostUsage{
		PartID: "part-late",
		Usage: openCodeContextUsage{
			MessageID: "msg-late", ReportedCost: float64ptr(0), Input: 1_000,
		},
	})

	openCodeVirtualCostState.Lock()
	state := openCodeVirtualCostState.usageSession[sessionID]["msg-late"]
	openCodeVirtualCostState.Unlock()
	assert.Empty(t, state.steps,
		"the timed-out generation must not repopulate usage after the replacement owns the registry")
}

func TestLaterOpenCodeStepCancelsPendingFinalRemoval(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)
	const sessionID, convID, messageID = "oc-pending-later", "ses-pending-later", "msg-pending"
	seedOpenCodeUsageSession(t, sessionID, convID)
	openCodeVirtualCostState.Lock()
	_, usages := ensureOpenCodeVirtualCostStateLocked(sessionID)
	usages[messageID] = openCodeMessageCostUsage{
		hadSteps: true,
		steps: map[string]openCodeContextUsage{
			"part-removed": {MessageID: messageID, ReportedCost: float64ptr(0), Input: 1_000},
		},
	}
	openCodeVirtualCostState.Unlock()

	originalMark := markOpenCodePricingStepsRemoved
	t.Cleanup(func() { markOpenCodePricingStepsRemoved = originalMark })
	markOpenCodePricingStepsRemoved = func(string, string, string, time.Time) error {
		return errors.New("database busy")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runtime := db.OpenCodeRuntime{SessionID: sessionID, ConvID: convID}
	applyOpenCodeVirtualCostRemoval(ctx, runtime, openCodeCostRemoval{
		MessageID: messageID, PartID: "part-removed",
	})
	require.True(t, hasOpenCodePendingRemoval(convID, messageID))

	applyOpenCodeVirtualCostStep(ctx, runtime, openCodeStepCostUsage{
		PartID: "part-new",
		Usage: openCodeContextUsage{
			MessageID: messageID, ReportedCost: float64ptr(0), Input: 2_000,
		},
	})
	assert.False(t, hasOpenCodePendingRemoval(convID, messageID),
		"a later eligible step cancels the pending final-removal tombstone")
	openCodeVirtualCostState.Lock()
	state := openCodeVirtualCostState.usageSession[sessionID][messageID]
	openCodeVirtualCostState.Unlock()
	assert.NotContains(t, state.steps, "part-removed")
	assert.Contains(t, state.steps, "part-new")
}

func TestReplacementProjectorReschedulesInheritedPendingRemoval(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(int32) bool
	}{
		{name: "all immediate writes fail", fail: func(attempt int32) bool {
			return attempt <= 2
		}},
		{name: "mixed immediate success and failure", fail: func(attempt int32) bool {
			return attempt == 2
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			resetOpenCodeVirtualCostStateForTest()
			t.Cleanup(resetOpenCodeVirtualCostStateForTest)
			const sessionID, convID = "oc-pending-replacement", "ses-pending-replacement"
			seedOpenCodeUsageSession(t, sessionID, convID)
			removals := []openCodeCostRemoval{
				{MessageID: "msg-pending-1", PartID: "part-removed-1"},
				{MessageID: "msg-pending-2", PartID: "part-removed-2"},
			}
			openCodeVirtualCostState.Lock()
			_, usages := ensureOpenCodeVirtualCostStateLocked(sessionID)
			for _, removal := range removals {
				usages[removal.MessageID] = openCodeMessageCostUsage{
					hadSteps: true,
					steps: map[string]openCodeContextUsage{
						removal.PartID: {
							MessageID: removal.MessageID, ReportedCost: float64ptr(0), Input: 1_000,
						},
					},
				}
			}
			openCodeVirtualCostState.Unlock()
			for _, removal := range removals {
				rememberOpenCodePendingRemoval(convID, removal, time.Now())
			}

			originalMark := markOpenCodePricingStepsRemoved
			t.Cleanup(func() { markOpenCodePricingStepsRemoved = originalMark })
			var markAttempts atomic.Int32
			markOpenCodePricingStepsRemoved = func(
				convID, sessionID, messageID string,
				removedAt time.Time,
			) error {
				if tc.fail(markAttempts.Add(1)) {
					return errors.New("database still busy")
				}
				return originalMark(convID, sessionID, messageID, removedAt)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/config/providers":
					_, _ = w.Write([]byte(`{"providers":[]}`))
				case "/session/" + convID + "/message":
					_, _ = w.Write([]byte(`[` +
						`{"info":{"id":"msg-pending-1","role":"assistant","cost":0,` +
						`"tokens":{"input":1000,"output":0}}},` +
						`{"info":{"id":"msg-pending-2","role":"assistant","cost":0,` +
						`"tokens":{"input":1000,"output":0}}}` +
						`]`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			runtime := db.OpenCodeRuntime{
				SessionID: sessionID, ConvID: convID, ServerURL: server.URL,
				Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
			}
			require.False(t, backfillOpenCodeContextUsage(context.Background(), runtime),
				"at least one replacement marker write still fails and transfers batch ownership")

			require.Eventually(t, func() bool {
				return markAttempts.Load() >= int32(2*len(removals)) &&
					!hasOpenCodePendingRemoval(convID, removals[0].MessageID) &&
					!hasOpenCodePendingRemoval(convID, removals[1].MessageID)
			}, 2*openCodeSSERetryDelay, 25*time.Millisecond,
				"the current replacement generation takes over every inherited retry")
		})
	}
}

func TestPendingRemovalRetryDeduplicatesPerProjectorGeneration(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)
	const sessionID, convID = "oc-pending-dedupe", "ses-pending-dedupe"
	seedOpenCodeUsageSession(t, sessionID, convID)
	for i := 1; i <= 2; i++ {
		removal := openCodeCostRemoval{
			MessageID: fmt.Sprintf("msg-pending-%d", i),
			PartID:    fmt.Sprintf("part-pending-%d", i),
		}
		rememberOpenCodePendingRemoval(convID, removal, time.Now())
	}
	originalMark := markOpenCodePricingStepsRemoved
	originalDelay := openCodeRemovalRetryDelay
	t.Cleanup(func() {
		markOpenCodePricingStepsRemoved = originalMark
		openCodeRemovalRetryDelay = originalDelay
	})
	var markAttempts atomic.Int32
	markOpenCodePricingStepsRemoved = func(string, string, string, time.Time) error {
		markAttempts.Add(1)
		return errors.New("database remains busy")
	}
	openCodeRemovalRetryDelay = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	runtime := db.OpenCodeRuntime{SessionID: sessionID, ConvID: convID}
	require.False(t, backfillOpenCodeContextUsage(ctx, runtime))
	for range 10 {
		scheduleOpenCodeRemovalRetry(ctx, runtime)
	}
	openCodeVirtualCostState.Lock()
	scheduled := len(openCodeVirtualCostState.removalRetries)
	openCodeVirtualCostState.Unlock()
	require.Equal(t, 1, scheduled,
		"one generation owns one batch retry regardless of pending-message count")
	require.Eventually(t, func() bool {
		return markAttempts.Load() >= 6
	}, time.Second, 5*time.Millisecond)
	cancel()
	require.Eventually(t, func() bool {
		openCodeVirtualCostState.Lock()
		defer openCodeVirtualCostState.Unlock()
		return len(openCodeVirtualCostState.removalRetries) == 0
	}, time.Second, 5*time.Millisecond)
}

func TestApplyOpenCodeVirtualCostUsageSkipsRealAndAmbiguousCost(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"providers":[{"id":"openai","models":{"gpt-a":{` +
			`"cost":{"input":1,"output":2,"cache":{"read":0.1,"write":0.2}},"limit":{"context":200000}}}}]}`))
	}))
	t.Cleanup(server.Close)
	runtime := db.OpenCodeRuntime{
		SessionID: "oc-real", ConvID: "ses-real", ServerURL: server.URL, Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
	}
	seedOpenCodeUsageSession(t, runtime.SessionID, runtime.ConvID)
	usage := openCodeContextUsage{
		MessageID: "msg-real", ProviderID: "openai", ModelID: "gpt-a",
		ReportedCost: float64ptr(0.5), Input: 1_000_000,
	}
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, usage)
	snap, err := db.GetContextSnapshot(runtime.SessionID)
	require.NoError(t, err)
	assert.Zero(t, snap.VirtualCostUSD, "native real cost makes the session ineligible for WHAT-IF")
	assert.InDelta(t, 0.5, snap.CostUSD, 1e-12,
		"positive live message cost is persisted without waiting for session.updated")

	applyOpenCodeVirtualCostStep(context.Background(), runtime, openCodeStepCostUsage{
		PartID: "part-later",
		Usage: openCodeContextUsage{
			MessageID: "msg-real", ReportedCost: float64ptr(0.7), Input: 1_000_000,
		},
	})
	snap, err = db.GetContextSnapshot(runtime.SessionID)
	require.NoError(t, err)
	assert.InDelta(t, 0.7, snap.CostUSD, 1e-12,
		"a persisted later step wins over a stale lower top-level cumulative cost")

	usage.MessageID, usage.ReportedCost = "msg-ambiguous", nil
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, usage)
	snap, err = db.GetContextSnapshot(runtime.SessionID)
	require.NoError(t, err)
	assert.Zero(t, snap.VirtualCostUSD, "missing reported-cost metadata is not guessed to mean subscription")
	activity, err := db.OpenCodeUsageActivityBetween(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, activity, "real and ambiguous traffic do not create subscription coverage")
}

// seedOpenCodeUsageSession inserts a minimal OpenCode session row so the usage
// writers have a target to UPDATE.
func seedOpenCodeUsageSession(t *testing.T, sessionID, convID string) {
	t.Helper()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "oc-usage",
		Status: "idle", Harness: harness.OpenCodeName, CreatedAt: time.Now(),
	}))
	openCodeVirtualCostState.Lock()
	if openCodeVirtualCostState.hydratedSession == nil {
		openCodeVirtualCostState.hydratedSession = map[string]bool{}
	}
	openCodeVirtualCostState.hydratedSession[sessionID] = true
	openCodeVirtualCostState.Unlock()
}

func openCodeSessionUpdatedEventJSON(envelopeSessionID, infoID string, cost float64) string {
	return fmt.Sprintf(`{"id":"evt_s","type":"session.updated","properties":{`+
		`"sessionID":%q,"info":{"id":%q,"cost":%v}}}`, envelopeSessionID, infoID, cost)
}

func TestApplyOpenCodeCost(t *testing.T) {
	setupTestDB(t)
	const sessionID, convID = "oc-cost-session", "ses_cost"
	seedOpenCodeUsageSession(t, sessionID, convID)
	runtime := db.OpenCodeRuntime{SessionID: sessionID, ConvID: convID}

	// Subscription: OpenCode reports cost 0 -> nothing written (honest N/A).
	applyOpenCodeCost(runtime, json.RawMessage(openCodeSessionUpdatedEventJSON(convID, convID, 0)))
	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Zero(t, snap.CostUSD, "zero cost must not be recorded")

	// Pay-per-token key: real cumulative cost is recorded.
	applyOpenCodeCost(runtime, json.RawMessage(openCodeSessionUpdatedEventJSON(convID, convID, 0.4213)))
	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 0.4213, snap.CostUSD, 1e-9)
}

// The conversation is matched from the envelope's sessionID when the session
// info carries no id, mirroring the context path's robustness.
func TestApplyOpenCodeCost_EnvelopeFallback(t *testing.T) {
	setupTestDB(t)
	const sessionID, convID = "oc-cost-env", "ses_cost_env"
	seedOpenCodeUsageSession(t, sessionID, convID)
	runtime := db.OpenCodeRuntime{SessionID: sessionID, ConvID: convID}

	applyOpenCodeCost(runtime, json.RawMessage(openCodeSessionUpdatedEventJSON(convID, "", 1.25)))
	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 1.25, snap.CostUSD, 1e-9)
}

// /event is directory-scoped: a session.updated for another conversation must
// not touch this session's cost.
func TestApplyOpenCodeCost_IgnoresForeignConversation(t *testing.T) {
	setupTestDB(t)
	const sessionID, convID = "oc-cost-own", "ses_cost_own"
	seedOpenCodeUsageSession(t, sessionID, convID)
	runtime := db.OpenCodeRuntime{SessionID: sessionID, ConvID: convID}

	applyOpenCodeCost(runtime, json.RawMessage(openCodeSessionUpdatedEventJSON("ses_other", "ses_other", 9.99)))
	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Zero(t, snap.CostUSD, "foreign conversation must not write cost")
}

func TestPersistOpenCodeRuntimeMetadata(t *testing.T) {
	setupTestDB(t)
	const sessionID, convID = "oc-model-session", "ses_model"
	seedOpenCodeUsageSession(t, sessionID, convID)
	runtime := db.OpenCodeRuntime{SessionID: sessionID, ConvID: convID}

	// Missing halves are a no-op.
	persistOpenCodeRuntimeMetadata(runtime, openCodeContextUsage{ProviderID: "openai"})
	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Empty(t, snap.Model, "incomplete model identity must not be written")

	persistOpenCodeRuntimeMetadata(runtime, openCodeContextUsage{
		ProviderID: "openai", ModelID: "gpt-5.6-terra", Variant: "high",
	})
	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-5.6-terra", snap.Model)
	assert.Equal(t, "openai/gpt-5.6-terra", snap.ModelID)
	assert.Equal(t, "high", snap.EffortLevel)
}

// TestBackfillOpenCodeContextUsage drives the reconnect/resume path against a
// stub server: it fetches the conversation's message history, selects the newest
// assistant turn by time.created (regardless of slice order), resolves the model
// limit through the shared context path, and lands a context snapshot + model
// slug on the row.
func TestBackfillOpenCodeContextUsage(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)

	const (
		sessionID = "oc-backfill-session"
		convID    = "ses_backfill"
		password  = "pw-backfill"
	)
	seedOpenCodeUsageSession(t, sessionID, convID)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, "opencode", user)
		assert.Equal(t, password, pass)
		switch r.URL.Path {
		case "/config/providers":
			_, _ = w.Write([]byte(`{"providers":[{"id":"openai","models":{` +
				`"gpt-5.6-terra":{"cost":{"input":2,"output":10,"cache":{"read":0.2,"write":0.5}},` +
				`"limit":{"context":272000,"output":128000}}}}]}`))
		case "/session/" + convID + "/message":
			_, _ = w.Write([]byte(`[` +
				`{"info":{"id":"msg_u","role":"user"}},` +
				// Persisted step preceding its interrupted top-level update.
				`{"info":{"id":"msg_step_only","role":"assistant","providerID":"openai","modelID":"gpt-5.6-terra",` +
				`"time":{"created":50},"cost":0,"tokens":{"input":0,"output":0}},` +
				`"parts":[{"id":"part-only","messageID":"msg_step_only","type":"step-finish","cost":0,` +
				`"tokens":{"input":1000000,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}]},` +
				// Older assistant turn (smaller context) — must NOT win.
				`{"info":{"id":"msg_a1","role":"assistant","providerID":"openai","modelID":"gpt-5.6-terra",` +
				`"time":{"created":100},"cost":0,"tokens":{"input":10000,"output":200,"reasoning":0,"cache":{"read":0,"write":0}}}},` +
				// Newer assistant turn — this one wins.
				`{"info":{"id":"msg_a2","role":"assistant","providerID":"openai","modelID":"gpt-5.6-terra","variant":"xhigh",` +
				`"time":{"created":200},"cost":0,"tokens":{"input":80000,"output":4000,"reasoning":1000,"cache":{"read":20000,"write":0}}},` +
				`"parts":[` +
				`{"id":"part-a","messageID":"msg_a2","type":"step-finish","cost":0,` +
				`"tokens":{"input":10000,"output":100,"reasoning":0,"cache":{"read":0,"write":0}}},` +
				`{"id":"part-b","messageID":"msg_a2","type":"step-finish","cost":0,` +
				`"tokens":{"input":80000,"output":4000,"reasoning":1000,"cache":{"read":20000,"write":0}}}]}` +
				`]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	runtime := db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: convID,
		ServerURL: server.URL, Password: password, PID: os.Getpid(),
		Cwd: t.TempDir(),
	}
	backfillOpenCodeContextUsage(context.Background(), runtime)

	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(100000), snap.TokensInput) // 80000 + 20000 cache read
	assert.Equal(t, int64(5000), snap.TokensOutput)  // 4000 + 1000 reasoning
	assert.Equal(t, int64(272000), snap.ContextWindowSize)
	assert.InDelta(t, float64(105000)/272000*100, snap.ContextPct, 1e-6)
	assert.Equal(t, "openai/gpt-5.6-terra", snap.Model)
	assert.Equal(t, "xhigh", snap.EffortLevel)
	assert.InDelta(t, 2.257, snap.VirtualCostUSD, 1e-12,
		"recovery prices every persisted step, including one whose top-level update was interrupted")
}

func TestOpenCodeCostBackfillAndLiveEventsAggregateNestedSessionTree(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)
	const (
		sessionID = "oc-tree"
		rootID    = "ses-root"
		childID   = "ses-child"
		grandID   = "ses-grand"
		lateID    = "ses-late"
	)
	seedOpenCodeUsageSession(t, sessionID, rootID)
	message := func(id string, input int64) string {
		return fmt.Sprintf(`[{"info":{"id":%q,"role":"assistant","providerID":"openai",`+
			`"modelID":"gpt-a","time":{"created":100},"cost":0,`+
			`"tokens":{"input":%d,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}}]`,
			id, input)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config/providers":
			_, _ = w.Write([]byte(`{"providers":[{"id":"openai","models":{"gpt-a":{` +
				`"cost":{"input":1,"output":2},"limit":{"context":10000000}}}}]}`))
		case "/session/" + rootID + "/message":
			_, _ = w.Write([]byte(message("msg-root", 1_000_000)))
		case "/session/" + childID + "/message":
			_, _ = w.Write([]byte(message("msg-child", 2_000_000)))
		case "/session/" + grandID + "/message":
			_, _ = w.Write([]byte(message("msg-grand", 3_000_000)))
		case "/session/" + rootID + "/children":
			_, _ = w.Write([]byte(`[{"id":"` + childID + `","parentID":"` + rootID + `","cost":0}]`))
		case "/session/" + childID + "/children":
			_, _ = w.Write([]byte(`[{"id":"` + grandID + `","parentID":"` + childID + `","cost":0}]`))
		case "/session/" + grandID + "/children":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	runtime := db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: rootID, ServerURL: server.URL,
		Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
	}
	require.True(t, backfillOpenCodeContextUsage(context.Background(), runtime))

	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 6, snap.VirtualCostUSD, 1e-12)
	assert.Equal(t, int64(1_000_000), snap.TokensInput,
		"child usage contributes cost without replacing root context occupancy")
	openCodeVirtualCostState.Lock()
	tracked := openCodeVirtualCostState.trackedSessions[sessionID]
	assert.Equal(t, rootID, tracked[childID])
	assert.Equal(t, childID, tracked[grandID])
	openCodeVirtualCostState.Unlock()

	projector := newOpenCodeEventProjector(rootID, runtime.Cwd)
	childUpdate := openCodeMessageUpdatedEventJSON(
		"evt-child", childID, "openai", "gpt-a", 4_000_000, 0, 0, 0, 0,
	)
	childUpdate = strings.Replace(childUpdate, `"msg_1"`, `"msg-child"`, 1)
	childUpdate = strings.Replace(childUpdate, `"tokens"`, `"cost":0,"tokens"`, 1)
	consumeOpenCodeEvent(context.Background(), runtime, projector, json.RawMessage(childUpdate))
	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 8, snap.VirtualCostUSD, 1e-12,
		"a replayed child message replaces its prior contribution exactly once")
	assert.Equal(t, int64(1_000_000), snap.TokensInput)

	created := json.RawMessage(fmt.Sprintf(
		`{"type":"session.created","properties":{"info":{"id":%q,"parentID":%q}}}`,
		lateID, grandID,
	))
	consumeOpenCodeEvent(context.Background(), runtime, projector, created)
	lateUpdate := openCodeMessageUpdatedEventJSON(
		"evt-late", lateID, "openai", "gpt-a", 1_000_000, 0, 0, 0, 0,
	)
	lateUpdate = strings.Replace(lateUpdate, `"msg_1"`, `"msg-late"`, 1)
	lateUpdate = strings.Replace(lateUpdate, `"tokens"`, `"cost":0,"tokens"`, 1)
	consumeOpenCodeEvent(context.Background(), runtime, projector, json.RawMessage(lateUpdate))
	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 9, snap.VirtualCostUSD, 1e-12,
		"a nested child discovered live contributes without reconnecting")

	deleted := json.RawMessage(fmt.Sprintf(
		`{"type":"session.deleted","properties":{"info":{"id":%q}}}`, childID,
	))
	consumeOpenCodeEvent(context.Background(), runtime, projector, deleted)
	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 1, snap.VirtualCostUSD, 1e-12,
		"deleting a child removes its complete descendant subtree projection")
}

func TestApplyOpenCodeCostAggregatesTrackedChildSessions(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)
	const sessionID, rootID, childID, grandID = "oc-real-tree", "ses-r", "ses-c", "ses-g"
	seedOpenCodeUsageSession(t, sessionID, rootID)
	runtime := db.OpenCodeRuntime{SessionID: sessionID, ConvID: rootID}
	for _, edge := range [][2]string{{childID, rootID}, {grandID, childID}} {
		observeOpenCodeSessionTree(runtime, json.RawMessage(fmt.Sprintf(
			`{"type":"session.created","properties":{"info":{"id":%q,"parentID":%q}}}`,
			edge[0], edge[1],
		)))
	}
	applyOpenCodeCost(runtime, json.RawMessage(openCodeSessionUpdatedEventJSON(rootID, rootID, 1)))
	applyOpenCodeCost(runtime, json.RawMessage(openCodeSessionUpdatedEventJSON(childID, childID, 2)))
	applyOpenCodeCost(runtime, json.RawMessage(openCodeSessionUpdatedEventJSON(grandID, grandID, 3)))
	applyOpenCodeCost(runtime, json.RawMessage(openCodeSessionUpdatedEventJSON("foreign", "foreign", 99)))
	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 6, snap.CostUSD, 1e-12)

	openCodeVirtualCostState.Lock()
	openCodeVirtualCostState.hydratedSession = map[string]bool{sessionID: true}
	openCodeVirtualCostState.Unlock()
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, openCodeContextUsage{
		SessionID: rootID, MessageID: "msg-real-root", ReportedCost: float64ptr(1), Input: 1,
	})
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, openCodeContextUsage{
		SessionID: childID, MessageID: "msg-real-child", ReportedCost: float64ptr(2), Input: 1,
	})
	removed := observeOpenCodeSessionTree(runtime, json.RawMessage(fmt.Sprintf(
		`{"type":"session.deleted","properties":{"info":{"id":%q}}}`, childID,
	)))
	applyOpenCodeSessionDeletion(context.Background(), runtime, removed)
	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 6, snap.CostUSD, 1e-12,
		"deleting conversation data cannot unspend cumulative API cost")
	openCodeVirtualCostState.Lock()
	assert.NotContains(t, openCodeVirtualCostState.nativeCosts[sessionID], childID)
	assert.NotContains(t, openCodeVirtualCostState.nativeCosts[sessionID], grandID)
	assert.InDelta(t, 5, openCodeVirtualCostState.retiredNativeCost[sessionID], 1e-12)
	openCodeVirtualCostState.Unlock()

	applyOpenCodeCost(runtime, json.RawMessage(openCodeSessionUpdatedEventJSON(rootID, rootID, 2)))
	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 7, snap.CostUSD, 1e-12,
		"new root spend advances on top of the compacted deleted-child spend")
}

func TestObserveOpenCodeSessionTreeBoundsLiveDescendants(t *testing.T) {
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)
	runtime := db.OpenCodeRuntime{SessionID: "oc-bound", ConvID: "ses-bound"}
	for i := 0; i < maxOpenCodeTrackedSessions+50; i++ {
		observeOpenCodeSessionTree(runtime, json.RawMessage(fmt.Sprintf(
			`{"type":"session.created","properties":{"info":{"id":"child-%d","parentID":%q}}}`,
			i, runtime.ConvID,
		)))
	}
	openCodeVirtualCostState.Lock()
	tracked := len(openCodeVirtualCostState.trackedSessions[runtime.SessionID])
	openCodeVirtualCostState.Unlock()
	assert.Equal(t, maxOpenCodeTrackedSessions, tracked)
}

func TestBackfillOpenCodeCostReconstructsDeletedChildSpendAfterRestart(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)
	const sessionID, rootID = "oc-retired-restart", "ses-retired-restart"
	seedOpenCodeUsageSession(t, sessionID, rootID)
	require.NoError(t, db.UpdateSessionCost(sessionID, 7),
		"persist the pre-restart root plus deleted-child cumulative spend")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/" + rootID + "/message":
			_, _ = w.Write([]byte(`[{"info":{"id":"msg-root","role":"assistant",` +
				`"sessionID":"` + rootID + `","providerID":"openai","modelID":"gpt-a",` +
				`"cost":2,"tokens":{"input":1,"output":0}}}]`))
		case "/session/" + rootID + "/children":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	runtime := db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: rootID, ServerURL: server.URL,
		Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
	}
	require.True(t, backfillOpenCodeContextUsage(context.Background(), runtime))
	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 7, snap.CostUSD, 1e-12)
	openCodeVirtualCostState.Lock()
	assert.InDelta(t, 5, openCodeVirtualCostState.retiredNativeCost[sessionID], 1e-12)
	assert.InDelta(t, 2, openCodeVirtualCostState.nativeCosts[sessionID][rootID], 1e-12)
	openCodeVirtualCostState.Unlock()
}

func TestBufferedFinalStepRemovalSurvivesReconnectBackfill(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)

	const sessionID, convID, messageID = "oc-buffered-removal", "ses_buffered_removal", "msg-buffered"
	seedOpenCodeUsageSession(t, sessionID, convID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config/providers":
			_, _ = w.Write([]byte(`{"providers":[{"id":"openai","models":{"gpt-a":{` +
				`"cost":{"input":1,"output":2},"limit":{"context":200000}}}}]}`))
		case "/session/" + convID + "/message":
			// The live stream was opened first and has the corresponding
			// removal buffered, while this snapshot already omits the part and
			// still exposes stale top-level tokens.
			_, _ = w.Write([]byte(`[{"info":{"id":"` + messageID + `","role":"assistant",` +
				`"providerID":"openai","modelID":"gpt-a","time":{"created":100},` +
				`"cost":0,"tokens":{"input":1000000,"output":0}}}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	runtime := db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: convID, ServerURL: server.URL,
		Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
	}
	usage := openCodeContextUsage{
		MessageID: messageID, ProviderID: "openai", ModelID: "gpt-a",
		ReportedCost: float64ptr(0), Input: 1_000_000,
	}
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, usage)
	applyOpenCodeVirtualCostStep(context.Background(), runtime, openCodeStepCostUsage{
		PartID: "part-buffered", Usage: usage,
	})
	require.True(t, backfillOpenCodeContextUsage(context.Background(), runtime))
	openCodeVirtualCostState.Lock()
	_, stepStillInSnapshot := openCodeVirtualCostState.
		usageSession[sessionID][messageID].steps["part-buffered"]
	openCodeVirtualCostState.Unlock()
	require.False(t, stepStillInSnapshot,
		"the reconnect snapshot already reflects the absent part")

	applyOpenCodeVirtualCostRemoval(context.Background(), runtime, openCodeCostRemoval{
		MessageID: messageID, PartID: "part-buffered",
	})

	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Zero(t, snap.VirtualCostUSD,
		"the buffered removal remains classifiable after backfill replaces step state")
	activity, err := db.OpenCodeUsageActivityBetween(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, activity)
	removed, err := db.OpenCodePricingStepsRemoved(convID, time.Now())
	require.NoError(t, err)
	assert.True(t, removed[messageID], "the buffered final removal becomes durable")
}

func TestReconnectPrunesDisconnectedStepBeforeLiveFinalRemoval(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)

	const sessionID, convID, messageID = "oc-disconnected-removal", "ses_disconnected_removal", "msg-disconnected"
	seedOpenCodeUsageSession(t, sessionID, convID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config/providers":
			_, _ = w.Write([]byte(`{"providers":[{"id":"openai","models":{"gpt-a":{` +
				`"cost":{"input":1,"output":2},"limit":{"context":200000}}}}]}`))
		case "/session/" + convID + "/message":
			// part-a disappeared before this SSE stream opened. Only part-b is
			// authoritative in the reconnect snapshot.
			_, _ = w.Write([]byte(`[{"info":{"id":"` + messageID + `","role":"assistant",` +
				`"providerID":"openai","modelID":"gpt-a","time":{"created":100},` +
				`"cost":0,"tokens":{"input":1000000,"output":0}},` +
				`"parts":[{"id":"part-b","messageID":"` + messageID + `","type":"step-finish",` +
				`"cost":0,"tokens":{"input":1000000,"output":0}}]}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	runtime := db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: convID, ServerURL: server.URL,
		Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
	}
	usage := openCodeContextUsage{
		MessageID: messageID, ProviderID: "openai", ModelID: "gpt-a",
		ReportedCost: float64ptr(0), Input: 1_000_000,
	}
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, usage)
	for _, partID := range []string{"part-a", "part-b"} {
		applyOpenCodeVirtualCostStep(context.Background(), runtime, openCodeStepCostUsage{
			PartID: partID, Usage: usage,
		})
	}
	require.True(t, backfillOpenCodeContextUsage(context.Background(), runtime))

	applyOpenCodeVirtualCostRemoval(context.Background(), runtime, openCodeCostRemoval{
		MessageID: messageID, PartID: "part-b",
	})

	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Zero(t, snap.VirtualCostUSD,
		"the reconnect snapshot prunes part-a before classifying part-b as final")
	activity, err := db.OpenCodeUsageActivityBetween(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, activity)
	removed, err := db.OpenCodePricingStepsRemoved(convID, time.Now())
	require.NoError(t, err)
	assert.True(t, removed[messageID], "the true live final removal becomes durable")
}

func TestBackfillOpenCodeContextUsageRecoversMissedRealCost(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)

	const sessionID, convID = "oc-real-backfill", "ses_real_backfill"
	seedOpenCodeUsageSession(t, sessionID, convID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config/providers":
			_, _ = w.Write([]byte(`{"providers":[{"id":"openai","models":{"gpt-a":{` +
				`"cost":{"input":1,"output":2},"limit":{"context":200000}}}}]}`))
		case "/session/" + convID + "/message":
			_, _ = w.Write([]byte(`[` +
				`{"info":{"id":"msg-1","role":"assistant","providerID":"openai","modelID":"gpt-a",` +
				`"time":{"created":100},"cost":0.1,"tokens":{"input":1000,"output":100}},` +
				`"parts":[` +
				`{"id":"p1","messageID":"msg-1","type":"step-finish","cost":0.1,"tokens":{"input":500,"output":50}},` +
				`{"id":"p2","messageID":"msg-1","type":"step-finish","cost":0.2,"tokens":{"input":1000,"output":100}}]},` +
				`{"info":{"id":"msg-2","role":"assistant","providerID":"openai","modelID":"gpt-a",` +
				`"time":{"created":200},"cost":0.3,"tokens":{"input":2000,"output":200}}},` +
				`{"info":{"id":"msg-ambiguous","role":"assistant","providerID":"openai","modelID":"gpt-a",` +
				`"time":{"created":300},"tokens":{"input":1000,"output":100}}}` +
				`]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	backfillOpenCodeContextUsage(context.Background(), db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: convID, ServerURL: server.URL,
		Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
	})
	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 0.6, snap.CostUSD, 1e-12,
		"recovery uses persisted step totals when the top-level cumulative cost is stale")
	assert.Zero(t, snap.VirtualCostUSD, "real history remains authoritative over WHAT-IF cost")
	activity, err := db.OpenCodeUsageActivityBetween(time.Unix(0, 0), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, activity, "real and ambiguous backfill traffic do not create subscription coverage")
}

func TestBackfillOpenCodeRealCostClearsWhatIfDuringCatalogOutage(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)

	const sessionID, convID = "oc-real-outage", "ses_real_outage"
	seedOpenCodeUsageSession(t, sessionID, convID)
	require.NoError(t, db.UpdateSessionVirtualCost(sessionID, 2))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config/providers":
			http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
		case "/session/" + convID + "/message":
			_, _ = w.Write([]byte(`[{"info":{"id":"msg-paid","role":"assistant",` +
				`"providerID":"openai","modelID":"gpt-a","time":{"created":100},` +
				`"cost":0.5,"tokens":{"input":1000,"output":100}}}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	require.True(t, backfillOpenCodeContextUsage(context.Background(), db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: convID, ServerURL: server.URL,
		Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
	}))
	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 0.5, snap.CostUSD, 1e-12)
	assert.Zero(t, snap.VirtualCostUSD,
		"positive native cost clears WHAT-IF state without requiring model pricing")
	rows, err := db.AllCostDailyRows()
	require.NoError(t, err)
	for _, row := range rows {
		if row.ConvID == convID {
			assert.Zero(t, row.VirtualCostUSD,
				"historical WHAT-IF prefixes are cleared when real spend becomes authoritative")
		}
	}
}

func TestOpenCodeLiveCostWaitsForAuthoritativeHydration(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)

	const sessionID, convID = "oc-hydration", "ses_hydration"
	seedOpenCodeUsageSession(t, sessionID, convID)
	require.NoError(t, db.UpdateSessionVirtualCost(sessionID, 2))
	openCodeVirtualCostState.Lock()
	delete(openCodeVirtualCostState.hydratedSession, sessionID)
	openCodeVirtualCostState.Unlock()

	var historyReady atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config/providers":
			_, _ = w.Write([]byte(`{"providers":[{"id":"openai","models":{"gpt-a":{` +
				`"cost":{"input":1,"output":2},"limit":{"context":200000}}}}]}`))
		case "/session/" + convID + "/message":
			if !historyReady.Load() {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`[{"info":{"id":"msg-old","role":"assistant",` +
				`"providerID":"openai","modelID":"gpt-a","time":{"created":100},` +
				`"cost":0,"tokens":{"input":2000000,"output":0}}}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	runtime := db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: convID, ServerURL: server.URL,
		Password: "pw", PID: os.Getpid(), Cwd: t.TempDir(),
	}
	require.False(t, backfillOpenCodeContextUsage(context.Background(), runtime))
	live := openCodeContextUsage{
		MessageID: "msg-live", ProviderID: "openai", ModelID: "gpt-a",
		ReportedCost: float64ptr(0), Input: 1_000_000, CreatedAt: time.Now(),
	}
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, live)
	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 2, snap.VirtualCostUSD, 1e-12,
		"a failed restart hydration cannot replace retained history with one live message")

	historyReady.Store(true)
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, live)
	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 3, snap.VirtualCostUSD, 1e-12,
		"the next live event retries hydration before adding its own contribution")
}
