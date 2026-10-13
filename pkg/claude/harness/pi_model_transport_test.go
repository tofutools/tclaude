package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func piWriteAgentFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
}

func TestPiModelTransportResolvesReviewedProvidersAndRefusesTheRest(t *testing.T) {
	resolver := piModelTransport{}

	got, err := resolver.ResolveModelTransport(ResolvedModelTransport{Provider: "anthropic", ProviderResolved: true})
	require.NoError(t, err)
	assert.Equal(t, []sandboxpolicy.NetworkAllowEntry{{Domain: "api.anthropic.com", Ports: []int{443}}}, got.Destinations)

	got, err = resolver.ResolveModelTransport(ResolvedModelTransport{Provider: "openai-codex", ProviderResolved: true})
	require.NoError(t, err)
	var domains []string
	for _, d := range got.Destinations {
		domains = append(domains, d.Domain)
	}
	assert.ElementsMatch(t, []string{"chatgpt.com", "auth.openai.com"}, domains,
		"the ChatGPT route refreshes its login at auth.openai.com")

	got, err = resolver.ResolveModelTransport(ResolvedModelTransport{
		Provider: "ollama", BaseURL: "http://127.0.0.1:11434/v1", ProviderResolved: true})
	require.NoError(t, err, "a models.json endpoint is followed, whatever the provider is called")
	require.NotEmpty(t, got.Destinations)

	_, err = resolver.ResolveModelTransport(ResolvedModelTransport{Provider: "amazon-bedrock", ProviderResolved: true})
	require.Error(t, err, "an account- or region-dependent host is refused, never guessed")
	assert.Contains(t, err.Error(), "no reviewed filtered-network route")

	_, err = resolver.ResolveModelTransport(ResolvedModelTransport{Provider: "anthropic"})
	require.Error(t, err, "an unresolved provider is no proof")
}

func TestPiLaunchModelRoute(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	cwd := testutil.CanonicalTempDir(t)
	agentDir := filepath.Join(home, "agent")
	getenv := func(name string) string {
		if name == PiAgentDirEnvVar {
			return agentDir
		}
		return ""
	}

	provider, baseURL, err := PiLaunchModelRoute(getenv, "openai/gpt-5.5", cwd)
	require.NoError(t, err)
	assert.Equal(t, "openai", provider)
	assert.Empty(t, baseURL, "pi's catalog endpoint")

	_, _, err = PiLaunchModelRoute(getenv, "gpt-5.5", cwd)
	require.Error(t, err, "a bare model id leaves the provider to pi's guess")
	_, _, err = PiLaunchModelRoute(getenv, "", cwd)
	require.Error(t, err, "no model and no default provider")

	piWriteAgentFile(t, agentDir, "settings.json", `{"defaultProvider":"anthropic"}`)
	provider, _, err = PiLaunchModelRoute(getenv, "", cwd)
	require.NoError(t, err)
	assert.Equal(t, "anthropic", provider)

	piWriteAgentFile(t, filepath.Join(cwd, ".pi"), "settings.json", `{"defaultProvider":"openai"}`)
	_, _, err = PiLaunchModelRoute(getenv, "", cwd)
	require.Error(t, err, "a project default that overrides the agent's is ambiguous")
	provider, _, err = PiLaunchModelRoute(getenv, "anthropic/claude-sonnet-4-5", cwd)
	require.NoError(t, err, "an explicit model settles it")
	assert.Equal(t, "anthropic", provider)

	piWriteAgentFile(t, agentDir, "models.json", `{"providers":{"ollama":{"baseUrl":"http://127.0.0.1:11434/v1"}}}`)
	provider, baseURL, err = PiLaunchModelRoute(getenv, "ollama/qwen3", cwd)
	require.NoError(t, err)
	assert.Equal(t, "ollama", provider)
	assert.Equal(t, "http://127.0.0.1:11434/v1", baseURL)

	piWriteAgentFile(t, agentDir, "settings.json", `{"defaultProvider":"anthropic","httpProxy":"http://proxy:3128"}`)
	_, _, err = PiLaunchModelRoute(getenv, "anthropic/claude-sonnet-4-5", cwd)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "httpProxy"))

	piWriteAgentFile(t, agentDir, "settings.json", `{not json`)
	_, _, err = PiLaunchModelRoute(getenv, "anthropic/claude-sonnet-4-5", cwd)
	require.Error(t, err, "a settings file pi would read but tclaude cannot is not guessed past")
}

func TestPiSessionModelFollowsPisBranchSelection(t *testing.T) {
	agentDir := piTestAgentDir(t)
	cwd := testutil.CanonicalTempDir(t)
	asst := func(provider, model, api string) obj {
		return obj{"role": "assistant", "content": "ok", "provider": provider, "model": model, "api": api, "stopReason": "stop"}
	}
	const switched, virtual, none = "aaaa1111-0000-4000-8000-00000000000a", "bbbb2222-0000-4000-8000-00000000000b", "cccc3333-0000-4000-8000-00000000000c"
	piWriteSession(t, agentDir, cwd, switched, piHeader(switched, cwd),
		obj{"type": "model_change", "id": "m0", "parentId": nil, "provider": "openai", "modelId": "gpt-5.5"},
		piMsg("a1", "m0", asst("openai", "gpt-5.5", "openai-responses")),
		obj{"type": "model_change", "id": "m1", "parentId": "a1", "provider": "anthropic", "modelId": "claude-sonnet-4-5"},
		piMsg("a2", "m1", asst("anthropic", "claude-sonnet-4-5", "anthropic-messages")),
	)
	// A router extension's virtual model answers with the physical model it
	// routed to; the selection that holds is the virtual one.
	piWriteSession(t, agentDir, cwd, virtual, piHeader(virtual, cwd),
		obj{"type": "model_change", "id": "m0", "parentId": nil, "provider": "router", "modelId": "auto"},
		piMsg("a1", "m0", asst("openai", "gpt-5.5", "openai-responses")),
	)
	piWriteSession(t, agentDir, cwd, none, piHeader(none, cwd),
		piMsg("u1", nil, obj{"role": "user", "content": "hi"}),
	)

	assert.Equal(t, "anthropic/claude-sonnet-4-5", PiSessionModel(switched, cwd))
	assert.Equal(t, "router/auto", PiSessionModel(virtual, cwd))
	assert.Empty(t, PiSessionModel(none, cwd), "pi falls back to its settings default")
	assert.Empty(t, PiSessionModel("dddd4444-0000-4000-8000-00000000000d", cwd))
}

func TestPiSpawnerRunsOfflineUnderFilteredNetwork(t *testing.T) {
	open := piSpawner{}.BuildCommand(SpawnSpec{})
	assert.NotContains(t, open, "PI_OFFLINE", "an open launch leaves the operator's pi settings in charge")

	filtered := piSpawner{}.BuildCommand(SpawnSpec{NetworkFiltered: true, PreLaunchScript: "PRELAUNCH; "})
	for name, value := range PiFilteredNetworkEnv() {
		assert.Contains(t, filtered, "export "+name+"="+value)
	}
	assert.Less(t, strings.Index(filtered, "PRELAUNCH"), strings.Index(filtered, "PI_OFFLINE"),
		"set after the profile's pre-launch script, so it cannot undo them")
}
