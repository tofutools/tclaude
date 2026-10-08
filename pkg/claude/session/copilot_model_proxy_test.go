package session

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestCopilotModelProxyLaunchAndFilteredRoute(t *testing.T) {
	h, err := harness.Resolve("copilot")
	require.NoError(t, err)
	require.True(t, h.SupportsModelProxy())
	require.Equal(t, "openai", h.ModelProxyDialect())
	for _, model := range []string{"", "auto"} {
		_, err = ResolveTclaudeLayerModelTransport(h, ModelTransportLaunchContext{ModelProxy: "gateway@peer", Model: model})
		require.ErrorContains(t, err, "explicit model")
	}
	resolved, err := ResolveTclaudeLayerModelTransport(h, ModelTransportLaunchContext{ModelProxy: "gateway@peer", Model: "gpt-5.4"})
	require.NoError(t, err)
	require.True(t, resolved.SessionGateway)
	for _, port := range []int{0, 32123} {
		cmd := HTTPProxySpawnCommand("launch", h, harness.SpawnSpec{ModelProxy: "gateway@peer", Model: "gpt-5.4", CopilotAPIPort: port})
		require.Contains(t, cmd, "--model-proxy-harness copilot")
		require.Contains(t, cmd, "--model=gpt-5.4")
		if port > 0 {
			require.Contains(t, cmd, "--ui-server")
		}
	}
	for _, name := range []string{"COPILOT_PROVIDER_API_KEY", "COPILOT_PROVIDER_API_KEY_COMMAND", "COPILOT_PROVIDER_HEADERS", "COPILOT_PROVIDER_TRANSPORT", "COPILOT_PROVIDER_WIRE_MODEL", "COPILOT_PROVIDERS_CONFIG", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "COPILOT_OFFLINE", "COPILOT_MODEL"} {
		require.True(t, modelProxyCompetingEnvironmentForHarness(name, "copilot"), name)
	}
	require.False(t, modelProxyCompetingEnvironmentForHarness("GH_TOKEN", "claude"))
	for _, name := range []string{"opencode", "gemini"} {
		unsupported, err := harness.Resolve(name)
		require.NoError(t, err)
		require.False(t, unsupported.SupportsModelProxy())
		require.Contains(t, unsupported.ModelProxyRefusal(), "unsupported")
	}
}

func TestCopilotModelProxyBridgeUsesResponses(t *testing.T) {
	var bearer string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models/bind" {
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "copilot", body["harness"])
			require.NotEmpty(t, body["bearer_hash"])
			_, _ = io.WriteString(w, `{}`)
			return
		}
		require.Equal(t, "/v1/models/request/v1/responses", r.URL.Path)
		require.Equal(t, "Bearer "+bearer, r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"object":"response","status":"completed"}`)
	}))
	defer daemon.Close()
	client := &http.Client{Transport: httpProxyTestTransport(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme = "http"
		r.URL.Host = strings.TrimPrefix(daemon.URL, "http://")
		return http.DefaultTransport.RoundTrip(r)
	})}
	bridge, err := newModelProxyBridge(client, "launch", "gateway@peer", "copilot")
	require.NoError(t, err)
	bearer = bridge.bearer
	for _, path := range []string{"responses", "messages"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "http://local/model/v1/"+path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+bearer)
		bridge.ServeHTTP(rec, req)
		if path == "responses" {
			require.Equal(t, 200, rec.Code)
			require.Contains(t, rec.Body.String(), `"object":"response"`)
		} else {
			require.Equal(t, 404, rec.Code)
		}
	}
}
