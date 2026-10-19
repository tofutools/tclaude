package agentd_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type shareRow struct {
	Peer      string     `json:"peer"`
	Gateway   string     `json:"gateway"`
	Dialect   string     `json:"dialect"`
	Models    []string   `json:"models"`
	MaxUSD    float64    `json:"max_usd"`
	SpentUSD  float64    `json:"spent_usd"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// Usage sharing discovery: a peer asks which gateways it may use here and
// sees its cap, spend and end; this node lists what it shares with whom.
func TestFederation_ModelSharesDiscovery(t *testing.T) {
	fh := newFedHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages/count_tokens" {
			_, _ = io.WriteString(w, `{"input_tokens":10}`)
			return
		}
		_, _ = io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":10,"output_tokens":3}}`)
	}))
	defer upstream.Close()
	view := agentd.PeerViewHandler(fh.peer.id.ID())
	asPeer := func() []shareRow {
		rec := testharness.Serve(view, testharness.JSONRequest(t, "GET", "/api/model-shares", nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var out struct {
			Shares []shareRow `json:"shares"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out.Shares
	}
	require.Empty(t, asPeer(), "nothing shared: an empty answer, not a refusal")

	fedModelPolicy(t, fh, upstream.URL) // grants bob models.proxy on gateway "model"
	_, err := config.Update(func(cfg *config.Config, err error) error {
		if err != nil {
			return err
		}
		gw := cfg.Agent.HTTPProxies["model"]
		gw.ModelPolicy.Prices = map[string]config.ModelProxyPrice{"test-model": {Input: 100000, Output: 100000}}
		cfg.Agent.HTTPProxies["model"] = gw
		return nil
	})
	require.NoError(t, err)
	ends := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermModelsProxy, "scope": "http_proxy=model", "spawn_policy": map[string]any{"max_usd": 20}, "expires_at": ends})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	status, result := fedModelCall(t, fh, `{"model":"test-model","max_tokens":5,"messages":[]}`)
	require.Equal(t, 200, status, result)

	var shares []shareRow
	fedEventually(t, "the spend shows", func() bool {
		shares = asPeer()
		return len(shares) == 1 && shares[0].SpentUSD > 1.29 && shares[0].SpentUSD < 1.31
	})
	s := shares[0]
	require.Equal(t, "model", s.Gateway)
	require.Equal(t, "anthropic", s.Dialect)
	require.Equal(t, []string{"test-model"}, s.Models)
	require.Equal(t, 20.0, s.MaxUSD)
	require.NotNil(t, s.ExpiresAt)
	require.True(t, s.ExpiresAt.Equal(ends))

	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/models/shares", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var given struct {
		Shares []shareRow `json:"shares"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &given))
	require.Len(t, given.Shares, 1)
	require.Equal(t, fh.peer.id.ID(), given.Shares[0].Peer)

	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/models/control", map[string]any{"name": "model", "peer": "bob", "disabled": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Empty(t, asPeer(), "a peer blocked on the gateway is not offered it")
}
