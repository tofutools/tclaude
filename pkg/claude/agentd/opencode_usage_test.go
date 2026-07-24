package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func float64ptr(value float64) *float64 { return &value }

func resetOpenCodeVirtualCostStateForTest() {
	openCodeVirtualCostState.Lock()
	openCodeVirtualCostState.bySession = nil
	openCodeVirtualCostState.usageSession = nil
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
	got, ok := openCodeVirtualCostForUsage(usage, base)
	require.True(t, ok)
	assert.InDelta(t, 0.756, got, 1e-12,
		"explicit >200k context tier wins and prices reasoning as output plus both cache buckets")

	base.Tiers = nil
	got, ok = openCodeVirtualCostForUsage(usage, base)
	require.True(t, ok)
	assert.InDelta(t, 0.5166, got, 1e-12, "legacy experimentalOver200K is the fallback")

	base.ExperimentalOver200K = nil
	got, ok = openCodeVirtualCostForUsage(usage, base)
	require.True(t, ok)
	assert.InDelta(t, 0.3745, got, 1e-12, "base pricing applies without a matching tier")

	_, ok = openCodeVirtualCostForUsage(usage, openCodeModelPrice{})
	assert.False(t, ok, "missing/zero pricing degrades without inventing a value")
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
			`"gpt-b":{"cost":{"input":2,"output":4,"cache":{"read":0.2,"write":0.4}},"limit":{"context":200000}}}}]}`))
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
	prices := openCodeModelPrices(context.Background(), runtime)
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
	assert.Zero(t, snap.VirtualCostUSD,
		"an unpriceable authoritative replay clears rather than retaining stale cost")

	rows, err := db.OpenCodeUsageActivityBetween(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Len(t, rows, 2, "replays are also idempotent in provider activity history")
}

func openCodeStepUpdatedEventJSON(convID, messageID, partID string, input int64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":"evt-%s","type":"message.part.updated","properties":{`+
		`"sessionID":%q,"part":{"id":%q,"messageID":%q,"sessionID":%q,"type":"step-finish",`+
		`"cost":0,"tokens":{"input":%d,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}}}`,
		partID, convID, partID, messageID, convID, input))
}

func TestOpenCodeVirtualCostAggregatesStepFinishParts(t *testing.T) {
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

	usage.MessageID, usage.ReportedCost = "msg-ambiguous", nil
	applyOpenCodeVirtualCostUsage(context.Background(), runtime, usage)
	snap, err = db.GetContextSnapshot(runtime.SessionID)
	require.NoError(t, err)
	assert.Zero(t, snap.VirtualCostUSD, "missing reported-cost metadata is not guessed to mean subscription")
}

// seedOpenCodeUsageSession inserts a minimal OpenCode session row so the usage
// writers have a target to UPDATE.
func seedOpenCodeUsageSession(t *testing.T, sessionID, convID string) {
	t.Helper()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "oc-usage",
		Status: "idle", Harness: harness.OpenCodeName, CreatedAt: time.Now(),
	}))
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

func TestPersistOpenCodeModelSlug(t *testing.T) {
	setupTestDB(t)
	const sessionID, convID = "oc-model-session", "ses_model"
	seedOpenCodeUsageSession(t, sessionID, convID)
	runtime := db.OpenCodeRuntime{SessionID: sessionID, ConvID: convID}

	// Missing halves are a no-op.
	persistOpenCodeModelSlug(runtime, openCodeContextUsage{ProviderID: "openai"})
	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Empty(t, snap.Model, "incomplete model identity must not be written")

	persistOpenCodeModelSlug(runtime, openCodeContextUsage{ProviderID: "openai", ModelID: "gpt-5.6-terra"})
	snap, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-5.6-terra", snap.Model)
	assert.Equal(t, "openai/gpt-5.6-terra", snap.ModelID)
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
				// Older assistant turn (smaller context) — must NOT win.
				`{"info":{"id":"msg_a1","role":"assistant","providerID":"openai","modelID":"gpt-5.6-terra",` +
				`"time":{"created":100},"cost":0,"tokens":{"input":10000,"output":200,"reasoning":0,"cache":{"read":0,"write":0}}}},` +
				// Newer assistant turn — this one wins.
				`{"info":{"id":"msg_a2","role":"assistant","providerID":"openai","modelID":"gpt-5.6-terra",` +
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
	assert.InDelta(t, 0.257, snap.VirtualCostUSD, 1e-12,
		"recovery prices every step-finish part without double-counting")
}
