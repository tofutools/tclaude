package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestResolveTclaudeLayerClaudeModelTransportFromExactLaunchInputs(t *testing.T) {
	home, cwd := isolateModelTransportLaunch(t)
	claude := harness.MustGet(harness.DefaultName)

	resolved, err := ResolveTclaudeLayerModelTransport(
		claude, ModelTransportLaunchContext{
			Model: "sonnet",
			Cwd:   cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "HOME", Value: home},
			},
		})
	require.NoError(t, err)
	assert.Equal(t, harness.ResolvedModelTransport{
		Model:            "sonnet",
		Provider:         "anthropic",
		ProviderResolved: true,
	}, resolved)

	resolved, err = ResolveTclaudeLayerModelTransport(
		claude, ModelTransportLaunchContext{
			Model: "sonnet",
			Cwd:   cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "HOME", Value: home},
				{Name: "ANTHROPIC_BASE_URL", Value: "https://gateway.example/v1"},
			},
		})
	require.NoError(t, err)
	assert.Equal(t, "https://gateway.example/v1", resolved.BaseURL)

	_, err = ResolveTclaudeLayerModelTransport(
		claude, ModelTransportLaunchContext{
			Cwd: cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "HOME", Value: home},
				{Name: "CLAUDE_CODE_USE_BEDROCK", Value: "1"},
			},
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CLAUDE_CODE_USE_BEDROCK")
	assert.Contains(t, err.Error(), "network open")

	_, err = ResolveTclaudeLayerModelTransport(
		claude, ModelTransportLaunchContext{
			Cwd:         cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{{Name: "HOME", Value: home}},
			ExtraArgs:   []string{"--settings", `{"env":{"ANTHROPIC_BASE_URL":"https://other.example"}}`},
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--settings")

	_, err = ResolveTclaudeLayerModelTransport(
		claude, ModelTransportLaunchContext{
			Cwd: cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "HOME", Value: home},
				{Name: "HTTPS_PROXY", Value: "http://proxy.example:8443"},
			},
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTPS_PROXY")
	assert.Contains(t, err.Error(), "actual network boundary")
	assert.Contains(t, err.Error(), "network open")
	assert.Contains(t, err.Error(), "TCL-826")
}

func TestResolveTclaudeLayerClaudeRefusesMutableSettingsProvider(t *testing.T) {
	home, cwd := isolateModelTransportLaunch(t)
	settings := filepath.Join(cwd, ".claude", "settings.local.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(settings), 0o700))
	require.NoError(t, os.WriteFile(settings, []byte(
		`{"env":{"ANTHROPIC_BASE_URL":"https://mutable.example/v1"}}`), 0o600))

	_, err := ResolveTclaudeLayerModelTransport(
		harness.MustGet(harness.DefaultName),
		ModelTransportLaunchContext{
			Cwd:         cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{{Name: "HOME", Value: home}},
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), settings)
	assert.Contains(t, err.Error(), "ANTHROPIC_BASE_URL")
	assert.Contains(t, err.Error(), "mutable")

	require.NoError(t, os.WriteFile(settings, []byte(
		`{"env":{"https_proxy":"http://mutable-proxy.example:8443"}}`), 0o600))
	_, err = ResolveTclaudeLayerModelTransport(
		harness.MustGet(harness.DefaultName),
		ModelTransportLaunchContext{
			Cwd:         cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{{Name: "HOME", Value: home}},
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), settings)
	assert.Contains(t, err.Error(), "https_proxy")
	assert.Contains(t, err.Error(), "mutable")
}

func TestResolveTclaudeLayerCodexModelTransportFromConfig(t *testing.T) {
	home, cwd := isolateModelTransportLaunch(t)
	codexHome := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	codex := harness.MustGet(harness.CodexName)
	context := ModelTransportLaunchContext{
		Model: "gpt-5.4",
		Cwd:   cwd,
		Environment: []sandboxpolicy.EnvironmentEntry{
			{Name: "HOME", Value: home},
			{Name: "CODEX_HOME", Value: codexHome},
		},
	}
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(
		`{"auth_mode":"apikey","OPENAI_API_KEY":"test-key"}`), 0o600))

	resolved, err := ResolveTclaudeLayerModelTransport(codex, context)
	require.NoError(t, err)
	assert.Equal(t, harness.ResolvedModelTransport{
		Model:            "gpt-5.4",
		Provider:         "openai",
		ProviderResolved: true,
	}, resolved)

	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(`
model_provider = "corp"
[model_providers.corp]
base_url = "https://models.example/v1"
`), 0o600))
	resolved, err = ResolveTclaudeLayerModelTransport(codex, context)
	require.NoError(t, err)
	assert.Equal(t, "corp", resolved.Provider)
	assert.Equal(t, "https://models.example/v1", resolved.BaseURL)

	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(`
model_provider = "openai"
openai_base_url = "https://openai-gateway.example/v1"
`), 0o600))
	resolved, err = ResolveTclaudeLayerModelTransport(codex, context)
	require.NoError(t, err)
	assert.Equal(t, "openai", resolved.Provider)
	assert.Equal(t, "https://openai-gateway.example/v1", resolved.BaseURL)
}

func TestResolveTclaudeLayerCodexRefusesAmbiguousProviderInputs(t *testing.T) {
	home, cwd := isolateModelTransportLaunch(t)
	codexHome := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	codex := harness.MustGet(harness.CodexName)
	context := ModelTransportLaunchContext{
		Cwd: cwd,
		Environment: []sandboxpolicy.EnvironmentEntry{
			{Name: "HOME", Value: home},
			{Name: "CODEX_HOME", Value: codexHome},
		},
	}

	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(`
profile = "work"
[profiles.work]
chatgpt_base_url = "https://auth-gateway.example"
`), 0o600))
	_, err := ResolveTclaudeLayerModelTransport(codex, context)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rejects legacy profile")

	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(`
model_provider = "missing"
`), 0o600))
	_, err = ResolveTclaudeLayerModelTransport(codex, context)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no concrete")

	context.ExtraArgs = []string{"-c", `model_provider="other"`}
	_, err = ResolveTclaudeLayerModelTransport(codex, context)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"-c"`)

	context.ExtraArgs = []string{`-cmodel_provider="other"`}
	_, err = ResolveTclaudeLayerModelTransport(codex, context)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"-c"`)

	context.ExtraArgs = []string{"-pwork"}
	_, err = ResolveTclaudeLayerModelTransport(codex, context)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"-p"`)
}

func TestResolveTclaudeLayerCodexRefusesUnmergedExternalProviderConfig(t *testing.T) {
	home, cwd := isolateModelTransportLaunch(t)
	systemConfig := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(systemConfig, []byte(
		`openai_base_url = "https://system-gateway.example/v1"`), 0o600))
	codexExternalModelConfigPaths = func() []string {
		return []string{systemConfig}
	}

	_, err := ResolveTclaudeLayerModelTransport(
		harness.MustGet(harness.CodexName),
		ModelTransportLaunchContext{
			Cwd: cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "HOME", Value: home},
			},
		},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), systemConfig)
	assert.Contains(t, err.Error(), "openai_base_url")
	assert.Contains(t, err.Error(), "cannot merge exactly")
}

func TestResolveTclaudeLayerCodexRefusesChatGPTAndOpaqueAuth(t *testing.T) {
	home, cwd := isolateModelTransportLaunch(t)
	codexHome := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	codex := harness.MustGet(harness.CodexName)
	context := ModelTransportLaunchContext{
		Cwd: cwd,
		Environment: []sandboxpolicy.EnvironmentEntry{
			{Name: "HOME", Value: home},
			{Name: "CODEX_HOME", Value: codexHome},
		},
	}

	for name, auth := range map[string]string{
		"chatgpt":     `{"auth_mode":"chatgpt","tokens":{"access_token":"secret"}}`,
		"legacy_chat": `{"tokens":{"access_token":"secret"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(
				filepath.Join(codexHome, "auth.json"), []byte(auth), 0o600))
			_, err := ResolveTclaudeLayerModelTransport(codex, context)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "API key")
			assert.Contains(t, err.Error(), "network open")
			assert.Contains(t, err.Error(), "TCL-826")
		})
	}

	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(
		`cli_auth_credentials_store = "keyring"`), 0o600))
	_, err := ResolveTclaudeLayerModelTransport(codex, context)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `cli_auth_credentials_store="keyring"`)
	assert.Contains(t, err.Error(), "TCL-826")
}

func TestResolveTclaudeLayerCodexAllowsAuthlessExplicitProvider(t *testing.T) {
	home, cwd := isolateModelTransportLaunch(t)
	codexHome := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(`
model_provider = "corp"
[model_providers.corp]
base_url = "https://models.example/v1"
requires_openai_auth = false
`), 0o600))

	resolved, err := ResolveTclaudeLayerModelTransport(
		harness.MustGet(harness.CodexName),
		ModelTransportLaunchContext{
			Cwd: cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "HOME", Value: home},
				{Name: "CODEX_HOME", Value: codexHome},
			},
		})
	require.NoError(t, err)
	assert.Equal(t, "corp", resolved.Provider)
	assert.Equal(t, "https://models.example/v1", resolved.BaseURL)
}

func TestResolveTclaudeLayerOpenCodeRequiresAndResolvesStrictInlineProvider(t *testing.T) {
	_, cwd := isolateModelTransportLaunch(t)
	openCode := harness.MustGet(harness.OpenCodeName)

	resolved, err := ResolveTclaudeLayerModelTransport(
		openCode,
		ModelTransportLaunchContext{Model: "provider/model", Cwd: cwd},
	)
	require.Error(t, err)
	assert.False(t, resolved.ProviderResolved)
	assert.Contains(t, err.Error(), "explicit-provider configs only")
	assert.Contains(t, err.Error(), "network open")

	content := `{
		"enabled_providers":["corp"],
		"provider":{
			"corp":{
				"npm":"@ai-sdk/openai-compatible",
				"whitelist":["model"],
				"models":{"model":{"name":"Model"}},
				"options":{"baseURL":"https://models.example/v1","apiKey":"test-key"}
			}
		}
	}`
	context := ModelTransportLaunchContext{
		Model: "corp/model",
		Cwd:   cwd,
		Environment: []sandboxpolicy.EnvironmentEntry{{
			Name: "OPENCODE_CONFIG_CONTENT", Value: content,
		}},
	}
	resolved, err = ResolveTclaudeLayerModelTransport(openCode, context)
	require.NoError(t, err)
	assert.Equal(t, harness.ResolvedModelTransport{
		Model:            "corp/model",
		Provider:         "corp",
		BaseURL:          "https://models.example/v1",
		ProviderResolved: true,
	}, resolved)

	context.Environment[0].Value = strings.Replace(
		content, "https://models.example/v1", "{env:MODEL_URL}", 1)
	_, err = ResolveTclaudeLayerModelTransport(openCode, context)
	require.ErrorContains(t, err, "may not use environment")

	context.Environment[0].Value = strings.Replace(
		content, `"enabled_providers":["corp"]`,
		`"enabled_providers":["corp","other"]`, 1)
	_, err = ResolveTclaudeLayerModelTransport(openCode, context)
	require.ErrorContains(t, err, "exactly")
}

func TestResolveTclaudeLayerOpenCodeRefusesManagedConfig(t *testing.T) {
	_, cwd := isolateModelTransportLaunch(t)
	managed := filepath.Join(t.TempDir(), "opencode.json")
	require.NoError(t, os.WriteFile(managed, []byte(`{}`), 0o600))
	previous := openCodeManagedConfigPaths
	openCodeManagedConfigPaths = func() []string { return []string{managed} }
	t.Cleanup(func() { openCodeManagedConfigPaths = previous })

	_, err := ResolveTclaudeLayerModelTransport(
		harness.MustGet(harness.OpenCodeName),
		ModelTransportLaunchContext{
			Model: "corp/model", Cwd: cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{{
				Name:  "OPENCODE_CONFIG_CONTENT",
				Value: `{"enabled_providers":["corp"],"provider":{"corp":{"npm":"@ai-sdk/openai-compatible","whitelist":["model"],"models":{"model":{}},"options":{"baseURL":"https://models.example"}}}}`,
			}},
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), managed)
	assert.Contains(t, err.Error(), "network open")
}

func isolateModelTransportLaunch(t *testing.T) (home, cwd string) {
	t.Helper()
	home = t.TempDir()
	cwd = filepath.Join(home, "repo")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, variable := range []string{
		"HTTPS_PROXY", "https_proxy",
		"HTTP_PROXY", "http_proxy",
		"ALL_PROXY", "all_proxy",
	} {
		t.Setenv(variable, "")
	}
	for variable := range claudeProviderSettingVariables {
		t.Setenv(variable, "")
	}
	managed := t.TempDir()
	previous := claudeManagedProviderSettingsRoot
	claudeManagedProviderSettingsRoot = func() string { return managed }
	t.Cleanup(func() { claudeManagedProviderSettingsRoot = previous })
	previousCodexExternal := codexExternalModelConfigPaths
	codexExternalModelConfigPaths = func() []string { return nil }
	t.Cleanup(func() { codexExternalModelConfigPaths = previousCodexExternal })
	return home, cwd
}
