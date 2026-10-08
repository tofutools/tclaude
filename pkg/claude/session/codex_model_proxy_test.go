package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestCodexModelProxyProviderRefusesCompetingAuthentication(t *testing.T) {
	valid := codexEffectiveProvider{BaseURL: "http://127.0.0.1:1234/model/v1", EnvKey: "TCLAUDE_MODEL_PROXY_TOKEN", WireAPI: "responses"}
	check := func(p codexEffectiveProvider) error {
		return verifyCodexModelProxyProvider(codexEffectiveConfig{ModelProvider: "launch", ModelProviders: map[string]codexEffectiveProvider{"launch": p}}, "launch", valid.BaseURL)
	}
	require.NoError(t, check(valid))
	for _, mutate := range []func(*codexEffectiveProvider){
		func(p *codexEffectiveProvider) { p.RequiresOpenAIAuth = true },
		func(p *codexEffectiveProvider) { p.Auth = json.RawMessage(`{"command":"helper"}`) },
		func(p *codexEffectiveProvider) { p.HTTPHeaders = map[string]string{"Authorization": "ambient"} },
		func(p *codexEffectiveProvider) {
			p.EnvHTTPHeaders = map[string]string{"Authorization": "OPENAI_API_KEY"}
		},
		func(p *codexEffectiveProvider) { p.EnvKey = "OPENAI_API_KEY" },
		func(p *codexEffectiveProvider) { p.BaseURL = "https://elsewhere.example" },
		func(p *codexEffectiveProvider) { p.SupportsWebsockets = true },
		func(p *codexEffectiveProvider) { p.QueryParams = map[string]string{"api-key": "ambient"} },
	} {
		p := valid
		mutate(&p)
		require.Error(t, check(p))
	}
	for _, name := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "HTTP_PROXY", "https_proxy", "ALL_PROXY", "TCLAUDE_MODEL_PROXY_TOKEN"} {
		require.True(t, modelProxyCompetingEnvironment(name), name)
	}
}

func TestCodexModelProxyOverridesCoverBothDriveCommands(t *testing.T) {
	h, err := harness.Resolve("codex")
	require.NoError(t, err)
	require.True(t, h.SupportsModelProxy())
	for _, appServer := range []bool{false, true} {
		spec := harness.SpawnSpec{ModelProxy: "models@gateway"}
		if appServer {
			spec.CodexAppServerSocket = "/private/app.sock"
			spec.CodexAppServerURL = "ws://127.0.0.1:32123"
		}
		command := HTTPProxySpawnCommand("launch", h, spec)
		require.Contains(t, command, "--model-proxy-harness codex")
		expected := 1
		if appServer {
			expected = 2
		}
		require.Equal(t, expected, strings.Count(command, "--codex-env-marker-offset"))
		require.Equal(t, expected, strings.Count(command, harness.CodexHTTPProxyEnvironmentMarker+"_"))
	}
}

func TestCodexModelProxyFilteredPreflightUsesSameOverrides(t *testing.T) {
	previous := codexEffectiveConfigReader
	t.Cleanup(func() { codexEffectiveConfigReader = previous })
	codexEffectiveConfigReader = func(_ string, env []sandboxpolicy.EnvironmentEntry, _ string, overrides ...string) (codexEffectiveConfig, error) {
		require.Equal(t, harness.CodexModelProxyOverrides("tclaude_gateway_preflight", "http://127.0.0.1:1/model/v1"), overrides)
		for _, e := range env {
			require.NotEqual(t, "OPENAI_API_KEY", e.Name)
			require.NotEqual(t, "HTTP_PROXY", e.Name)
		}
		return codexEffectiveConfig{ModelProvider: "tclaude_gateway_preflight", ModelProviders: map[string]codexEffectiveProvider{"tclaude_gateway_preflight": {BaseURL: "http://127.0.0.1:1/model/v1", WireAPI: "responses", EnvKey: "TCLAUDE_MODEL_PROXY_TOKEN"}}}, nil
	}
	h, err := harness.Resolve("codex")
	require.NoError(t, err)
	result, err := ResolveTclaudeLayerModelTransport(h, ModelTransportLaunchContext{ModelProxy: "models@gateway", Environment: []sandboxpolicy.EnvironmentEntry{{Name: "OPENAI_API_KEY", Value: "ambient"}, {Name: "HTTP_PROXY", Value: "http://elsewhere"}}})
	require.NoError(t, err)
	require.True(t, result.SessionGateway)
	require.True(t, result.ProviderResolved)
	require.Empty(t, result.AuxiliaryBaseURLs)
	_, err = ResolveTclaudeLayerModelTransport(h, ModelTransportLaunchContext{ModelProxy: "models@gateway", ExtraArgs: []string{"--config=model_provider=elsewhere"}})
	require.Error(t, err)
}
