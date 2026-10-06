package agentd_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestHTTPProxyFlow(t *testing.T) {
	f := newFlow(t)
	const conv = "conv-http-proxy"
	f.HaveConvWithTitle(conv, "http-worker")
	f.HaveEnrolledAgent(conv)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		assert.Equal(t, "/api/items", r.URL.Path)
		assert.Equal(t, "q=one", r.URL.RawQuery)
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))
		data, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, []byte{0, 255, 10}, data)
		w.Header().Set("X-Result", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(data)
	}))
	defer upstream.Close()
	token := filepath.Join(testutil.CanonicalTempDir(t), "token")
	require.NoError(t, os.WriteFile(token, []byte("Bearer secret\n"), 0600))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{HTTPProxies: map[string]config.HTTPProxyConfig{
		"service": {URL: upstream.URL + "/api", Header: "Authorization", HeaderValue: "wrong", HeaderValueFile: token},
		"other":   {URL: upstream.URL, Header: "Authorization", HeaderValue: "other"},
	}}}))
	post := func(name, path string, headers map[string]string) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/http/request", map[string]any{
			"name": name, "path": path, "method": "POST", "headers": headers, "body": []byte{0, 255, 10},
		}), conv))
	}
	assert.Equal(t, 403, post("service", "items", nil).Code)
	assert.Zero(t, calls)
	require.NoError(t, db.GrantAgentPermissionWithScope(conv, agentd.PermHTTP, `{"http_proxy":["service"]}`, "test"))
	assert.Equal(t, 403, post("other", "items", nil).Code)
	assert.Equal(t, 403, post("Service", "items", nil).Code)
	assert.Zero(t, calls)
	for _, path := range []string{"https://evil.example/items", "//evil.example/items", "../items", "%2e%2e/items", "%252e%252e/items", "foo%2f..%2fitems", "items#fragment"} {
		assert.Equal(t, 400, post("service", path, nil).Code, path)
	}
	assert.Equal(t, 400, post("service", "items", map[string]string{"Connection": "Authorization"}).Code)
	assert.Zero(t, calls)
	response := post("service", "items?q=one", map[string]string{"authorization": "caller", "Content-Type": "application/octet-stream"})
	require.Equal(t, 200, response.Code, response.Body.String())
	var result struct {
		Status  int
		Headers http.Header
		Body    []byte
	}
	testharness.DecodeJSON(t, response, &result)
	assert.Equal(t, 201, result.Status)
	assert.Equal(t, "yes", result.Headers.Get("X-Result"))
	assert.Equal(t, []byte{0, 255, 10}, result.Body)
	assert.Equal(t, 1, calls)
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "http.request", Outcome: "success"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "service", rows[0].TargetLabel)
	assert.Contains(t, rows[0].Detail, "http_proxy=service")
	assert.NotContains(t, rows[0].Detail, "secret")
}

func TestHTTPProxyReturnsRedirectAndUpstreamErrors(t *testing.T) {
	f := newFlow(t)
	const conv = "conv-http-redirect"
	f.HaveConvWithTitle(conv, "http-worker")
	f.HaveEnrolledAgent(conv)
	leaked := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer destination.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, destination.URL, http.StatusFound)
			return
		}
		w.WriteHeader(429)
		_, _ = w.Write([]byte("slow down"))
	}))
	defer upstream.Close()
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{HTTPProxies: map[string]config.HTTPProxyConfig{
		"service": {URL: upstream.URL, Header: "X-API-Key", HeaderValue: "secret"},
	}}}))
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHTTP, "test"))
	for _, tc := range []struct {
		path   string
		status int
	}{{"redirect", 302}, {"limited", 429}} {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/http/request", map[string]any{"name": "service", "path": tc.path}), conv))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var result struct{ Status int }
		testharness.DecodeJSON(t, rec, &result)
		assert.Equal(t, tc.status, result.Status)
	}
	assert.False(t, leaked)
}

func TestHTTPProxyRawGatewayAndEnvironment(t *testing.T) {
	f := newFlow(t)
	const conv = "conv-http-gateway"
	f.HaveConvWithTitle(conv, "gateway-worker")
	f.HaveEnrolledAgent(conv)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("X-Tclaude-Session-Id"))
		assert.Equal(t, "/api/items", r.URL.Path)
		assert.Equal(t, "key=one", r.URL.RawQuery)
		data, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, []byte{0, 255, 10}, data)
		w.Header().Add("X-Result", "one")
		w.Header().Add("X-Result", "two")
		w.Header().Set("Connection", "X-Hop")
		w.Header().Set("X-Hop", "private")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write(data)
	}))
	defer upstream.Close()
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{HTTPProxies: map[string]config.HTTPProxyConfig{
		"inventory": {URL: upstream.URL + "/api", Header: "Authorization", HeaderValue: "Bearer secret"},
		"billing":   {URL: upstream.URL, Header: "Authorization", HeaderValue: "private"},
	}}}))
	require.NoError(t, db.GrantAgentPermissionWithScope(conv, agentd.PermHTTP, `{"http_proxy":["inventory"]}`, "test"))
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(httptest.NewRequest("GET", "/v1/http/environment", nil), conv))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var env struct{ Names []string }
	testharness.DecodeJSON(t, rec, &env)
	assert.Equal(t, []string{"inventory"}, env.Names)
	assert.NotContains(t, rec.Body.String(), "secret")
	req := httptest.NewRequest("POST", "/v1/http/proxy/inventory/items?key=one", bytes.NewReader([]byte{0, 255, 10}))
	req.Header.Set("Authorization", "caller")
	req.Header.Set("Connection", "X-Hop")
	req.Header.Set("X-Hop", "incoming")
	req.Header.Set("X-Tclaude-Session-Id", "forged")
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(req, conv))
	require.Equal(t, 422, rec.Code, rec.Body.String())
	assert.Equal(t, []byte{0, 255, 10}, rec.Body.Bytes())
	assert.Equal(t, []string{"one", "two"}, rec.Header().Values("X-Result"))
	assert.Empty(t, rec.Header().Get("Connection"))
	assert.Empty(t, rec.Header().Get("X-Hop"))
	assert.Equal(t, 1, calls)
	for _, path := range []string{"../items", "%2e%2e/items", "foo%2fbar", "%252e%252e/items"} {
		rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(httptest.NewRequest("GET", "/v1/http/proxy/inventory/"+path, nil), conv))
		assert.Equal(t, 400, rec.Code, path)
	}
	assert.Equal(t, 1, calls)
	require.NoError(t, db.SetAgentPermissionOverride(conv, agentd.PermHTTP, db.PermEffectDeny, "test"))
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(httptest.NewRequest("GET", "/v1/http/proxy/inventory/items", nil), conv))
	assert.Equal(t, 403, rec.Code)
	assert.Equal(t, 1, calls)
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "http.gateway"})
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	for _, row := range rows {
		assert.Equal(t, "/v1/http/proxy/", row.Path)
		assert.NotContains(t, row.Detail, "secret")
	}
}
