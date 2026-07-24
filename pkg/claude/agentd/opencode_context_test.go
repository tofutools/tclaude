package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func resetOpenCodeLimitCacheForTest() {
	openCodeLimitCache.Lock()
	openCodeLimitCache.byServer = nil
	openCodeLimitCache.Unlock()
}

// openCodeMessageUpdatedEventJSON builds a `message.updated` SSE event carrying
// an assistant message's token usage, matching OpenCode 1.18.4's wire shape.
func openCodeMessageUpdatedEventJSON(id, sessionID, providerID, modelID string, input, output, reasoning, cacheRead, cacheWrite int64) string {
	return fmt.Sprintf(`{"id":%q,"type":"message.updated","properties":{"info":{`+
		`"id":"msg_1","role":"assistant","sessionID":%q,"providerID":%q,"modelID":%q,`+
		`"tokens":{"input":%d,"output":%d,"reasoning":%d,"cache":{"read":%d,"write":%d}}}}}`,
		id, sessionID, providerID, modelID, input, output, reasoning, cacheRead, cacheWrite)
}

func TestParseOpenCodeContextUsage(t *testing.T) {
	const convID = "ses_ctx"
	tests := []struct {
		name  string
		event string
		want  openCodeContextUsage
		ok    bool
	}{
		{
			name:  "assistant usage",
			event: openCodeMessageUpdatedEventJSON("evt_1", convID, "openai", "gpt-5.4", 1000, 200, 50, 300, 10),
			want: openCodeContextUsage{
				ProviderID: "openai", ModelID: "gpt-5.4",
				Input: 1000, Output: 200, Reasoning: 50, CacheRead: 300, CacheWrite: 10,
			},
			ok: true,
		},
		{
			name:  "wrong session ignored",
			event: openCodeMessageUpdatedEventJSON("evt_2", "ses_other", "openai", "gpt-5.4", 1000, 200, 0, 0, 0),
			ok:    false,
		},
		{
			name: "user role ignored",
			event: fmt.Sprintf(`{"id":"evt_3","type":"message.updated","properties":{"info":{`+
				`"role":"user","sessionID":%q,"tokens":{"input":10,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}}}`, convID),
			ok: false,
		},
		{
			name:  "zero total ignored",
			event: openCodeMessageUpdatedEventJSON("evt_4", convID, "openai", "gpt-5.4", 0, 0, 0, 0, 0),
			ok:    false,
		},
		{
			name:  "other event type ignored",
			event: openCodeTestEvent("evt_5", "session.status", convID, `"status":{"type":"busy"}`),
			ok:    false,
		},
		{
			name: "envelope session fallback",
			event: fmt.Sprintf(`{"id":"evt_6","type":"message.updated","properties":{"sessionID":%q,"info":{`+
				`"role":"assistant","providerID":"openai","modelID":"gpt-5.4",`+
				`"tokens":{"input":500,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}}}`, convID),
			want: openCodeContextUsage{ProviderID: "openai", ModelID: "gpt-5.4", Input: 500},
			ok:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			usage, ok := parseOpenCodeContextUsage(json.RawMessage(tc.event), convID)
			require.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.want, usage)
			}
		})
	}
}

func TestOpenCodeContextSnapshot(t *testing.T) {
	usage := openCodeContextUsage{Input: 1000, Output: 200, Reasoning: 50, CacheRead: 300, CacheWrite: 10}
	// total = 1000+200+50+300+10 = 1560; input side = 1000+300+10 = 1310; output side = 200+50 = 250.
	pct, in, out := openCodeContextSnapshot(usage, 200000)
	assert.Equal(t, int64(1310), in)
	assert.Equal(t, int64(250), out)
	assert.InDelta(t, float64(1560)/200000*100, pct, 1e-9)

	// Unknown window -> pct 0 but token counts preserved.
	pct, in, out = openCodeContextSnapshot(usage, 0)
	assert.Zero(t, pct)
	assert.Equal(t, int64(1310), in)
	assert.Equal(t, int64(250), out)
}

// newOpenCodeConfigServer serves /config/providers with the given limits and
// counts how many times it was hit, so tests can assert TTL caching.
func newOpenCodeConfigServer(t *testing.T, password string, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, "opencode", user)
		assert.Equal(t, password, pass)
		switch r.URL.Path {
		case "/config/providers":
			atomic.AddInt32(hits, 1)
			_, _ = w.Write([]byte(`{"providers":[{"id":"openai","models":{` +
				`"gpt-5.4":{"limit":{"context":272000,"output":128000}},` +
				`"gpt-5.4-mini":{"limit":{"context":400000}}}}],"default":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestPersistOpenCodeContextUsage(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)

	const (
		sessionID = "opencode-ctx-session"
		convID    = "ses_ctx_persist"
		password  = "pw-ctx"
	)
	var hits int32
	server := newOpenCodeConfigServer(t, password, &hits)
	defer server.Close()

	sess := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "oc-pane", Status: "idle",
		Harness: harness.OpenCodeName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(sess))

	runtime := db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: convID,
		ServerURL: server.URL, Password: password, PID: os.Getpid(),
		Cwd: t.TempDir(),
	}

	usage := openCodeContextUsage{
		ProviderID: "openai", ModelID: "gpt-5.4",
		Input: 100000, Output: 2000, Reasoning: 500, CacheRead: 30000, CacheWrite: 0,
	}
	persistOpenCodeContextUsage(context.Background(), runtime, usage)

	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(272000), snap.ContextWindowSize)
	assert.Equal(t, int64(130000), snap.TokensInput) // 100000 + 30000 cache read + 0 cache write
	assert.Equal(t, int64(2500), snap.TokensOutput)  // 2000 + 500 reasoning
	assert.InDelta(t, float64(132500)/272000*100, snap.ContextPct, 1e-6)

	// A second persist reuses the cached limits: no extra /config/providers hit.
	persistOpenCodeContextUsage(context.Background(), runtime, usage)
	assert.Equal(t, int32(1), atomic.LoadInt32(&hits), "provider limits are cached within the TTL")
}

// TestPersistOpenCodeContextUsageUnknownModel proves the graceful-degrade path:
// when /config/providers reports no limit for the active model, the snapshot
// still records token counts but leaves the window (and pct) unresolved rather
// than crashing or inventing a figure.
func TestPersistOpenCodeContextUsageUnknownModel(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)

	const (
		sessionID = "opencode-ctx-unknown"
		convID    = "ses_ctx_unknown"
		password  = "pw-unknown"
	)
	var hits int32
	server := newOpenCodeConfigServer(t, password, &hits)
	defer server.Close()

	sess := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "oc-pane3", Status: "idle",
		Harness: harness.OpenCodeName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(sess))

	runtime := db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: convID,
		ServerURL: server.URL, Password: password, PID: os.Getpid(),
		Cwd: t.TempDir(),
	}
	// The mock server knows only openai/gpt-5.4[-mini]; this model has no limit.
	usage := openCodeContextUsage{
		ProviderID: "anthropic", ModelID: "claude-sonnet-5",
		Input: 5000, Output: 1000, Reasoning: 0, CacheRead: 0, CacheWrite: 0,
	}
	persistOpenCodeContextUsage(context.Background(), runtime, usage)

	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Zero(t, snap.ContextWindowSize, "unknown model resolves no window")
	assert.Zero(t, snap.ContextPct, "pct is unresolved without a window")
	assert.Equal(t, int64(5000), snap.TokensInput)
	assert.Equal(t, int64(1000), snap.TokensOutput)
}

// TestConsumeOpenCodeEventPersistsContext drives the production SSE dispatch
// path end to end: a message.updated event flowing through consumeOpenCodeEvent
// must land as a dashboard-readable context snapshot on the session row.
func TestConsumeOpenCodeEventPersistsContext(t *testing.T) {
	setupTestDB(t)
	resetOpenCodeLimitCacheForTest()
	t.Cleanup(resetOpenCodeLimitCacheForTest)

	const (
		sessionID = "opencode-ctx-consume"
		convID    = "ses_ctx_consume"
		password  = "pw-consume"
	)
	var hits int32
	server := newOpenCodeConfigServer(t, password, &hits)
	defer server.Close()

	sess := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "oc-pane2", Status: "idle",
		Harness: harness.OpenCodeName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(sess))

	runtime := db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: convID,
		ServerURL: server.URL, Password: password, PID: os.Getpid(),
		Cwd: t.TempDir(),
	}
	projector := newOpenCodeEventProjector(convID, runtime.Cwd)

	event := openCodeMessageUpdatedEventJSON("evt_consume", convID, "openai", "gpt-5.4-mini", 40000, 1000, 0, 0, 0)
	consumeOpenCodeEvent(context.Background(), runtime, projector, json.RawMessage(event))

	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(400000), snap.ContextWindowSize)
	assert.Equal(t, int64(40000), snap.TokensInput)
	assert.Equal(t, int64(1000), snap.TokensOutput)
	assert.InDelta(t, float64(41000)/400000*100, snap.ContextPct, 1e-6)
}
