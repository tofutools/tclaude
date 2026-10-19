package agentd_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Usage sharing: a models.proxy grant with a dollar cap prices each request
// from the provider's reported usage and stops the peer once the cap is
// spent. Provider refusals cost nothing; an unpriced model is refused.
func TestFederation_ModelGatewayShareDollarCap(t *testing.T) {
	fh := newFedHarness(t)
	var refuse atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages/count_tokens" {
			_, _ = io.WriteString(w, `{"input_tokens":10}`)
			return
		}
		if refuse.Load() {
			w.WriteHeader(529)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
			return
		}
		// Real usage objects mix counts with strings and nested objects.
		_, _ = io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":6,"cache_creation_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":0},"cache_read_input_tokens":0,"output_tokens":3,"service_tier":"standard"}}`)
	}))
	defer upstream.Close()
	fedModelPolicy(t, fh, upstream.URL)
	_, err := config.Update(func(cfg *config.Config, err error) error {
		if err != nil {
			return err
		}
		gw := cfg.Agent.HTTPProxies["model"]
		gw.ModelPolicy.Models = []string{"test-model", "free-model"}
		// 0.1 USD per token keeps the arithmetic readable: 13 tokens = $1.30.
		gw.ModelPolicy.Prices = map[string]config.ModelProxyPrice{"test-*": {Input: 100000, Output: 100000}}
		cfg.Agent.HTTPProxies["model"] = gw
		return nil
	})
	require.NoError(t, err)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermMessageDirect, "spawn_policy": map[string]any{"max_usd": 5}})
	require.Equal(t, http.StatusBadRequest, rec.Code, "a dollar cap belongs on models.proxy only")
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermModelsProxy, "scope": "http_proxy=model", "spawn_policy": map[string]any{"max_usd": 2.5}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	body := `{"model":"test-model","max_tokens":5,"messages":[{"role":"user","content":"hello"}]}`
	usage := func() []db.ModelProxyUsage {
		var out []db.ModelProxyUsage
		rec := fedHuman(t, fh.f, http.MethodGet, "/v1/models/usage", nil)
		require.Equal(t, 200, rec.Code)
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}
	settled := func(n int) []db.ModelProxyUsage {
		var out []db.ModelProxyUsage
		fedEventually(t, "usage settled", func() bool {
			out = usage()
			if len(out) != n {
				return false
			}
			for _, u := range out {
				if u.Status == 0 {
					return false
				}
			}
			return true
		})
		return out
	}

	status, result := fedModelCall(t, fh, body)
	require.Equal(t, 200, status, result)
	got := settled(1)
	require.True(t, got[0].Complete, "usage with service_tier and cache_creation still completes")
	require.True(t, got[0].Priced)
	require.EqualValues(t, 13, got[0].ChargedTokens)
	require.EqualValues(t, 1_300_000, got[0].ChargedMicroUSD)

	refuse.Store(true)
	status, _ = fedModelCall(t, fh, body)
	require.Equal(t, 529, status)
	got = settled(2)
	require.EqualValues(t, 0, got[1].ChargedMicroUSD, "a provider refusal is not billed")
	refuse.Store(false)

	status, result = fedModelCall(t, fh, body)
	require.Equal(t, 200, status, "$1.30 of $2.50 spent: the next request still starts: "+result)
	settled(3)
	status, result = fedModelCall(t, fh, body)
	require.Equal(t, 429, status)
	require.Contains(t, result, "usage share used up: $2.60 of $2.50")

	status, result = fedModelCall(t, fh, `{"model":"free-model","max_tokens":5,"messages":[]}`)
	require.Equal(t, 403, status)
	require.Contains(t, result, "no price for free-model")

	// Raising the cap keeps what was spent; the share continues.
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermModelsProxy, "scope": "http_proxy=model", "spawn_policy": map[string]any{"max_usd": 4}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	status, result = fedModelCall(t, fh, body)
	require.Equal(t, 200, status, result)
}

// Every capped grant covering a gateway limits it: a gateway-scoped cap does
// not lift the peer's overall cap. Token counting is free; a request sent
// without an answer keeps its worst case, since the provider may bill it.
func TestFederation_ModelGatewayShareCapsStackAndUnansweredRequests(t *testing.T) {
	fh := newFedHarness(t)
	var hang atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages/count_tokens" {
			_, _ = io.WriteString(w, `{"input_tokens":10}`)
			return
		}
		if hang.Load() {
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
			return
		}
		_, _ = io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":10,"output_tokens":3}}`)
	}))
	defer upstream.Close()
	fedModelPolicy(t, fh, upstream.URL)
	_, err := config.Update(func(cfg *config.Config, err error) error {
		if err != nil {
			return err
		}
		gw := cfg.Agent.HTTPProxies["model"]
		gw.ModelPolicy.Prices = map[string]config.ModelProxyPrice{"test-model": {Input: 100000, Output: 100000}}
		gw.ModelPolicy.MaxDurationSeconds = 1
		cfg.Agent.HTTPProxies["model"] = gw
		return nil
	})
	require.NoError(t, err)
	grant := func(scope string, usd float64) {
		rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermModelsProxy, "scope": scope, "spawn_policy": map[string]any{"max_usd": usd}})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	grant("http_proxy=model", 100)
	grant("", 1) // the overall cap is the tighter one
	usage := func() []db.ModelProxyUsage {
		var out []db.ModelProxyUsage
		rec := fedHuman(t, fh.f, http.MethodGet, "/v1/models/usage", nil)
		require.Equal(t, 200, rec.Code)
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}
	settledRow := func(n int) db.ModelProxyUsage {
		var out []db.ModelProxyUsage
		fedEventually(t, "usage settled", func() bool {
			out = usage()
			return len(out) == n && out[n-1].Status != 0
		})
		return out[n-1]
	}

	status, result := fedModelPathCall(t, fh, "/v1/messages/count_tokens", `{"model":"test-model","messages":[]}`)
	require.Equal(t, 200, status, result)
	row := settledRow(1)
	require.EqualValues(t, 0, row.ChargedMicroUSD, "token counting is not billed")

	hang.Store(true)
	// The gateway gives up after its 1s duration bound, with no answer.
	status, _ = fedModelTryCall(t, fh, `{"model":"test-model","max_tokens":5,"messages":[]}`)
	require.NotEqual(t, 200, status)
	row = settledRow(2)
	require.Greater(t, row.ChargedMicroUSD, int64(0), "a request sent without an answer keeps its worst case")
	hang.Store(false)

	status, result = fedModelCall(t, fh, `{"model":"test-model","max_tokens":5,"messages":[]}`)
	require.Equal(t, 429, status, "the $1 overall cap holds although the gateway's own cap is $100")
	require.Contains(t, result, "usage share used up")
}

// fedModelTryCall is fedModelCall for a request the gateway may drop: a
// stream closed without a response reads as status 0.
func fedModelTryCall(t *testing.T, fh *fedHarness, body string) (int, string) {
	t.Helper()
	flow := fedModelFlow(t, fh)
	defer func() { _ = flow.Close() }()
	req, err := http.NewRequest(http.MethodPost, "http://model/v1/messages", strings.NewReader(body))
	require.NoError(t, err)
	req.Host = ""
	req.Header.Set("Content-Type", "application/json")
	require.NoError(t, req.Write(flow))
	require.NoError(t, flow.CloseWrite())
	resp, err := http.ReadResponse(bufio.NewReader(flow), req)
	if err != nil {
		return 0, err.Error()
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(data)
}

// fedModelPathCall is fedModelCall for another gateway endpoint.
func fedModelPathCall(t *testing.T, fh *fedHarness, path, body string) (int, string) {
	t.Helper()
	flow := fedModelFlow(t, fh)
	req, err := http.NewRequest(http.MethodPost, "http://model"+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Host = ""
	req.Header.Set("Content-Type", "application/json")
	require.NoError(t, req.Write(flow))
	require.NoError(t, flow.CloseWrite())
	resp, err := http.ReadResponse(bufio.NewReader(flow), req)
	require.NoError(t, err)
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	_ = flow.Close()
	return resp.StatusCode, string(data)
}
