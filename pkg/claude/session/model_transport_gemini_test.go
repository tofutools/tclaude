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

// geminiLaunchContext pins every variable the Gemini route resolver reads, so
// the developer's own shell cannot decide the outcome.
func geminiLaunchContext(t *testing.T, overrides map[string]string) (ModelTransportLaunchContext, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cwd := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".gemini"), 0o755))
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	values := map[string]string{
		"HOME":                            home,
		harness.GeminiHomeEnvVar:          "",
		"GEMINI_CLI_SYSTEM_SETTINGS_PATH": filepath.Join(root, "system", "settings.json"),
		"GEMINI_CLI_SYSTEM_DEFAULTS_PATH": filepath.Join(root, "system", "system-defaults.json"),
	}
	for _, name := range append(harness.GeminiRouteMovingEnvVars(), ModelTransportProxyVariables()...) {
		values[name] = ""
	}
	for name, value := range overrides {
		values[name] = value
	}
	var environment []sandboxpolicy.EnvironmentEntry
	for name, value := range values {
		environment = append(environment, sandboxpolicy.EnvironmentEntry{Name: name, Value: value})
	}
	return ModelTransportLaunchContext{Model: "gemini-3-flash", Cwd: cwd, Environment: environment}, home
}

func TestResolveGeminiModelTransportFollowsTheSelectedAuthType(t *testing.T) {
	h := harness.MustGet(harness.GeminiName)
	context, home := geminiLaunchContext(t, nil)

	_, err := ResolveTclaudeLayerModelTransport(h, context)
	require.Error(t, err, "no selected auth type means an unknown route")

	require.NoError(t, os.WriteFile(filepath.Join(home, ".gemini", "settings.json"),
		[]byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`), 0o600))
	resolved, err := ResolveTclaudeLayerModelTransport(h, context)
	require.NoError(t, err)
	assert.True(t, resolved.ProviderResolved)
	assert.Equal(t, harness.GeminiAuthLoginWithGoogle, resolved.Provider)

	requirement, err := harness.ResolveModelTransportRequirement(h, resolved)
	require.NoError(t, err)
	assert.Equal(t, harness.GeminiLoginNetworkPack, requirement.Template)
	assert.Error(t, harness.ValidateModelTransportCoverage(h,
		sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeList, Allow: harness.GeminiAPIKeyDestinations()},
		requirement), "the API-key host does not cover a Google sign-in launch")
	assert.NoError(t, harness.ValidateModelTransportCoverage(h,
		sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeList, Allow: harness.GeminiLoginDestinations()},
		requirement))

	moved, _ := geminiLaunchContext(t, map[string]string{"CODE_ASSIST_ENDPOINT": "https://gw.example"})
	_, err = ResolveTclaudeLayerModelTransport(h, moved)
	assert.ErrorContains(t, err, "CODE_ASSIST_ENDPOINT")
}
