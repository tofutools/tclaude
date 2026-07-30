//go:build linux

package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

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

[model_providers.corp]
name = "Corp"
base_url = "https://models.example/v1"
wire_api = "responses"

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
	config, err := readCodexEffectiveConfig(cwd, environment)
	require.NoError(t, err)

	assert.Equal(t, "corp", config.ModelProvider,
		"repository-local config may not choose the provider")
	require.Contains(t, config.ModelProviders, "corp")
	assert.Equal(t, "https://models.example/v1",
		config.ModelProviders["corp"].BaseURL)
	assert.Equal(t, "https://openai-gateway.example/v1", config.OpenAIBaseURL)
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
