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
				`"gpt-5.6-terra":{"limit":{"context":272000,"output":128000}}}}]}`))
		case "/session/" + convID + "/message":
			_, _ = w.Write([]byte(`[` +
				`{"info":{"id":"msg_u","role":"user"}},` +
				// Older assistant turn (smaller context) — must NOT win.
				`{"info":{"id":"msg_a1","role":"assistant","providerID":"openai","modelID":"gpt-5.6-terra",` +
				`"time":{"created":100},"tokens":{"input":10000,"output":200,"reasoning":0,"cache":{"read":0,"write":0}}}},` +
				// Newer assistant turn — this one wins.
				`{"info":{"id":"msg_a2","role":"assistant","providerID":"openai","modelID":"gpt-5.6-terra",` +
				`"time":{"created":200},"tokens":{"input":80000,"output":4000,"reasoning":1000,"cache":{"read":20000,"write":0}}}}` +
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
}
