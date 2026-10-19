package agentd_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A gateway set up from the dashboard serves a peer end to end: the pasted key
// lands in a private file, is never read back, reaches the provider, and is
// deleted with the gateway.
func TestDashboardModelGatewaySetupEndToEnd(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	var want atomic.Value
	want.Store("sk-pasted-1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != want.Load().(string) {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`)
			return
		}
		if r.URL.Path == "/v1/models" {
			require.Equal(t, "2023-06-01", r.Header.Get("anthropic-version"))
			_, _ = io.WriteString(w, `{"data":[{"id":"test-model"},{"id":"other-model"}]}`)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":10,"output_tokens":3}}`)
	}))
	defer upstream.Close()
	h := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, body any, status int) map[string]any {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, "/api/federation/models/"+tail, body))
		require.Equal(t, status, rec.Code, rec.Body.String())
		require.NotContains(t, rec.Body.String(), "sk-pasted")
		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}
	policy := map[string]any{"enabled": true, "models": []string{"test-model"}, "daily_requests": 30, "daily_tokens": 30000, "peer_daily_requests": 30, "peer_daily_tokens": 30000, "session_daily_requests": 30, "session_daily_tokens": 30000, "max_input_tokens": 1000, "max_output_tokens": 100, "max_concurrent": 2, "requests_per_minute": 30}
	setup := func(extra map[string]any) map[string]any {
		body := map[string]any{"url": upstream.URL, "header": "x-api-key", "policy": policy}
		for k, v := range extra {
			body[k] = v
		}
		return body
	}

	// The connection test lists the provider's models, and says when the key is refused.
	got := call("POST", "gateway-test", setup(map[string]any{"key": "sk-pasted-1"}), 200)
	require.Equal(t, true, got["ok"], got)
	require.Equal(t, []any{"other-model", "test-model"}, got["models"])
	got = call("POST", "gateway-test", setup(map[string]any{"key": "sk-pasted-wrong"}), 200)
	require.Equal(t, false, got["ok"])
	require.Contains(t, got["message"], "refused the key (HTTP 401)")

	call("PUT", "gateways/model", setup(map[string]any{"create": true}), 400)
	got = call("PUT", "gateways/model", setup(map[string]any{"create": true, "key": " sk-pasted-1 "}), 200)
	require.Equal(t, "managed", got["credential"])
	call("PUT", "gateways/model", setup(map[string]any{"create": true, "key": "sk-pasted-1"}), 400)
	keyFile := func() string {
		t.Helper()
		cfg, err := config.Load()
		require.NoError(t, err)
		return cfg.Agent.HTTPProxies["model"].HeaderValueFile
	}
	first := keyFile()
	require.Equal(t, filepath.Join(config.DataDir(), "model-gateways"), filepath.Dir(first))
	info, err := os.Stat(first)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.Equal(t, "managed", call("GET", "gateways/model", nil, 200)["credential"])

	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermModelsProxy, "scope": "http_proxy=model"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	status, result := fedModelCall(t, fh, `{"model":"test-model","max_tokens":5,"messages":[{"role":"user","content":"hello"}]}`)
	require.Equal(t, 200, status, result)

	// Editing without a key keeps it; the test reuses the stored key by name.
	got = call("PUT", "gateways/model", setup(map[string]any{"policy": map[string]any{"enabled": true, "models": []string{"test-model"}, "daily_requests": 40, "daily_tokens": 30000, "peer_daily_requests": 30, "peer_daily_tokens": 30000, "session_daily_requests": 30, "session_daily_tokens": 30000, "max_input_tokens": 1000, "max_output_tokens": 100, "max_concurrent": 2, "requests_per_minute": 30}}), 200)
	require.EqualValues(t, 40, got["policy"].(map[string]any)["daily_requests"])
	require.Equal(t, first, keyFile())
	require.Equal(t, true, call("POST", "gateway-test", setup(map[string]any{"name": "model"}), 200)["ok"])

	// A new key replaces the old file.
	want.Store("sk-pasted-2")
	call("PUT", "gateways/model", setup(map[string]any{"key": "sk-pasted-2"}), 200)
	second := keyFile()
	require.NotEqual(t, first, second)
	_, err = os.Stat(first)
	require.True(t, os.IsNotExist(err), "the replaced key file is deleted")
	status, result = fedModelCall(t, fh, `{"model":"test-model","max_tokens":5,"messages":[{"role":"user","content":"hello"}]}`)
	require.Equal(t, 200, status, result)

	call("PUT", "gateways/model", setup(map[string]any{"policy": map[string]any{"enabled": true, "models": []string{}}}), 400)
	require.Equal(t, true, call("DELETE", "gateways/model", nil, 200)["removed"])
	_, err = os.Stat(second)
	require.True(t, os.IsNotExist(err), "removing the gateway deletes its pasted key")
	require.Empty(t, call("GET", "control", nil, 200)["gateways"])
	call("GET", "gateways/model", nil, 404)
}
