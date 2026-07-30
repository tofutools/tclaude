//go:build linux

package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const codexSystemLayerSmokeEnv = "TCLAUDE_CODEX_SYSTEM_LAYER_SMOKE"

func requireCodexHarness(t *testing.T) *harness.Harness {
	t.Helper()
	h, err := harness.Resolve(harness.CodexName)
	require.NoError(t, err)
	return h
}

// TestPinnedCodexEffectiveConfigSeam executes the real CI-pinned Codex binary
// and proves the launch seam reads provider routing from Codex's own merged
// effective config rather than from a file tclaude parses itself.
//
// It also pins the loader fact that keeps this seam safe to widen: a repository
// `.codex/config.toml` is enumerated as a real layer but may not set any
// provider-routing key (PROJECT_LOCAL_CONFIG_DENYLIST in
// codex-rs/config/src/loader/mod.rs), so untrusted repository contents cannot
// move model traffic. The layers that can — system, MDM, and the enterprise
// cloud-config bundle — are exactly the ones only this read can see.
func TestPinnedCodexEffectiveConfigSeam(t *testing.T) {
	if os.Getenv(filteredModelEndpointSmokeEnv) != "1" {
		t.Skip("set TCLAUDE_FILTERED_MODEL_ENDPOINT_SMOKE=1 on the pinned Linux CI boundary")
	}
	requirePinnedFilteredHarness(t, "codex", filteredCodexPinnedVersion)
	for _, variable := range []string{
		"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy",
		"ALL_PROXY", "all_proxy",
	} {
		t.Setenv(variable, "")
	}

	home := t.TempDir()
	cwd := filepath.Join(home, "repo")
	codexHome := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(filepath.Join(cwd, ".codex"), 0o700))
	require.NoError(t, os.MkdirAll(codexHome, 0o700))

	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(`
model_provider = "corp"
openai_base_url = "https://openai-gateway.example/v1"
chatgpt_base_url = "https://codex.acme.example/backend-api/"
cli_auth_credentials_store = "file"

[model_providers.corp]
name = "Corp"
base_url = "https://models.example/v1"
wire_api = "responses"
requires_openai_auth = true

[projects."`+cwd+`"]
trust_level = "trusted"
`), 0o600))
	// A trusted repository layer that tries to take over the provider route.
	require.NoError(t, os.WriteFile(
		filepath.Join(cwd, ".codex", "config.toml"),
		[]byte("model_provider = \"repo-controlled\"\n"), 0o600))

	environment := []sandboxpolicy.EnvironmentEntry{
		{Name: "HOME", Value: home},
		{Name: "CODEX_HOME", Value: codexHome},
		{Name: "PATH", Value: os.Getenv("PATH")},
	}
	config, err := readCodexEffectiveConfig(cwd, environment, "")
	require.NoError(t, err)

	assert.Equal(t, "corp", config.ModelProvider,
		"repository-local config may not choose the provider")
	require.Contains(t, config.ModelProviders, "corp")
	assert.Equal(t, "https://models.example/v1",
		config.ModelProviders["corp"].BaseURL)
	assert.Equal(t, "https://openai-gateway.example/v1", config.OpenAIBaseURL)

	// Every field below drives a refusal or a disclosed destination, and each
	// one fails toward LESS enforcement if the response shape ever renames it:
	// a missed cli_auth_credentials_store reads as "file" and stops the opaque
	// credential-store refusal firing, a missed requires_openai_auth reads as
	// false and stops the unauthenticated-provider refusal firing, and a missed
	// chatgpt_base_url silently falls back to the chatgpt.com default. Pin them
	// against the real binary rather than trusting the field names.
	assert.Equal(t, "https://codex.acme.example/backend-api/", config.ChatGPTBaseURL)
	assert.Equal(t, "file", config.AuthStore)
	assert.True(t, config.ModelProviders["corp"].RequiresOpenAIAuth)

	// The provenance disclosure is built entirely from `origins`. Asserting
	// only that no REMOTE origin is present would pass just as well if the
	// response carried no origins at all, leaving the disclosure dead in
	// production, so pin that local origins are reported and attributed.
	require.NotEmpty(t, rawOrigins(t, cwd, environment),
		"config/read must report per-key origins even with includeLayers false")
	assert.Empty(t, config.RemoteOrigins,
		"no remotely delivered layer is configured on the CI boundary")

	// The same read is what the launch path resolves through, so pin the
	// end-to-end result rather than only the probe's projection.
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "auth.json"),
		[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"invalid-ci-evidence-key"}`),
		0o600))
	resolved, err := ResolveTclaudeLayerModelTransport(
		requireCodexHarness(t), ModelTransportLaunchContext{
			Model: "gpt-5.4", Cwd: cwd, Environment: environment,
		})
	require.NoError(t, err)
	assert.Equal(t, "corp", resolved.Provider)
	assert.Equal(t, "https://models.example/v1", resolved.BaseURL)
	assert.True(t, resolved.ProviderResolved)
}

// rawOrigins re-reads the effective config and returns the winning layer type
// per provider-routing key, so a test can prove the origins map is populated
// rather than silently absent.
func rawOrigins(
	t *testing.T,
	cwd string,
	environment []sandboxpolicy.EnvironmentEntry,
) map[string]string {
	t.Helper()
	raw, err := readCodexEffectiveConfigJSON(cwd, environment, "")
	require.NoError(t, err)
	var response struct {
		Origins map[string]struct {
			Name struct {
				Type string `json:"type"`
			} `json:"name"`
		} `json:"origins"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	routing := make(map[string]string, len(response.Origins))
	for key, origin := range response.Origins {
		if codexProviderRoutingKey(key) {
			routing[key] = origin.Name.Type
		}
	}
	return routing
}

// TestPinnedCodexSystemConfigLayerEvidence proves the effective read observes
// the machine-wide `/etc/codex/config.toml` layer.
//
// This is the evidence for a refusal that was REMOVED: the launch seam used to
// refuse outright when an external Codex config named a provider-routing key,
// because it could not merge that file. It now relies on Codex's own merge
// including the system layer. Without this, that removal would rest on a code
// comment, and a Codex release that stopped reading the file would silently
// resolve to the wrong destination while reporting the harness default.
//
// The system config is machine-wide, so its own CI step writes it immediately
// before this test and removes it immediately after, sequenced behind the other
// pinned-harness steps that would otherwise inherit the routing override.
func TestPinnedCodexSystemConfigLayerEvidence(t *testing.T) {
	if os.Getenv(codexSystemLayerSmokeEnv) != "1" {
		t.Skip("set TCLAUDE_CODEX_SYSTEM_LAYER_SMOKE=1 on the pinned Linux CI boundary")
	}
	requirePinnedFilteredHarness(t, "codex", filteredCodexPinnedVersion)
	for _, variable := range []string{
		"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy",
		"ALL_PROXY", "all_proxy",
	} {
		t.Setenv(variable, "")
	}

	const systemConfig = "/etc/codex/config.toml"
	contents, err := os.ReadFile(systemConfig)
	require.NoErrorf(t, err,
		"this named boundary requires %s to be staged by its CI step", systemConfig)
	require.Contains(t, string(contents), "openai_base_url",
		"the staged system config must carry a provider-routing key")

	home := t.TempDir()
	cwd := filepath.Join(home, "repo")
	codexHome := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	environment := []sandboxpolicy.EnvironmentEntry{
		{Name: "HOME", Value: home},
		{Name: "CODEX_HOME", Value: codexHome},
		{Name: "PATH", Value: os.Getenv("PATH")},
	}

	// No user config at all: whatever routing shows up can only have come from
	// the machine-wide layer.
	config, err := readCodexEffectiveConfig(cwd, environment, "")
	require.NoError(t, err)
	assert.Equal(t, "https://system-layer.example/v1", config.OpenAIBaseURL,
		"the effective read must observe the machine-wide system config layer")
	assert.Equal(t, "system", rawOrigins(t, cwd, environment)["openai_base_url"],
		"and must attribute that key to the system layer")
}
