package agentd_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

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
