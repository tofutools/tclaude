package session

import (
	"encoding/json"
	"errors"
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
	assert.Contains(t, err.Error(), "behind a proxy this seam does not resolve")
	assert.Contains(t, err.Error(), "network open")
}

func TestResolveTclaudeLayerClaudeRefusesRemoteManagedProviderSettings(t *testing.T) {
	home, cwd := isolateModelTransportLaunch(t)
	remote := filepath.Join(home, ".claude", "remote-settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(remote), 0o700))
	require.NoError(t, os.WriteFile(remote, []byte(
		`{"env":{"ANTHROPIC_BASE_URL":"https://remote-managed.example/v1"}}`), 0o600))

	_, err := ResolveTclaudeLayerModelTransport(
		harness.MustGet(harness.DefaultName),
		ModelTransportLaunchContext{
			Cwd:         cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{{Name: "HOME", Value: home}},
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), remote)
	assert.Contains(t, err.Error(), "ANTHROPIC_BASE_URL")

	// The relocation override has to be followed, or the inspected set silently
	// misses the file the harness will actually read.
	relocated := filepath.Join(t.TempDir(), "policy.json")
	require.NoError(t, os.WriteFile(relocated, []byte(
		`{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`), 0o600))
	_, err = ResolveTclaudeLayerModelTransport(
		harness.MustGet(harness.DefaultName),
		ModelTransportLaunchContext{
			Cwd: cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "HOME", Value: home},
				{Name: "CLAUDE_CODE_REMOTE_SETTINGS_PATH", Value: relocated},
			},
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), relocated)
	assert.Contains(t, err.Error(), "CLAUDE_CODE_USE_BEDROCK")

	// A stale or wrong override must not hide the default cache: both are
	// inspected, so the routing sitting in the default location still refuses.
	_, err = ResolveTclaudeLayerModelTransport(
		harness.MustGet(harness.DefaultName),
		ModelTransportLaunchContext{
			Cwd: cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "HOME", Value: home},
				{Name: "CLAUDE_CODE_REMOTE_SETTINGS_PATH", Value: filepath.Join(t.TempDir(), "absent.json")},
			},
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), remote)
	assert.Contains(t, err.Error(), "ANTHROPIC_BASE_URL")
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

func TestResolveTclaudeLayerCodexModelTransportFromEffectiveConfig(t *testing.T) {
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
	effective := stubCodexEffectiveConfig(t, codexEffectiveConfig{})
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(
		`{"auth_mode":"apikey","OPENAI_API_KEY":"test-key"}`), 0o600))

	resolved, err := ResolveTclaudeLayerModelTransport(codex, context)
	require.NoError(t, err)
	assert.Equal(t, harness.ResolvedModelTransport{
		Model:            "gpt-5.4",
		Provider:         "openai",
		ProviderResolved: true,
	}, resolved)

	// An enterprise cloud-config layer that a local config.toml parser cannot
	// see still resolves, because the effective read is the authority.
	*effective = codexEffectiveConfig{
		ModelProvider: "corp",
		ModelProviders: map[string]codexEffectiveProvider{
			"corp": {BaseURL: "https://models.example/v1"},
		},
		RemoteOrigins: []codexRemoteConfigOrigin{{
			Key: "model_provider", Layer: "enterpriseManaged",
			Name: "acme-workspace", Version: "sha256:abc",
		}},
	}
	resolved, err = ResolveTclaudeLayerModelTransport(codex, context)
	require.NoError(t, err)
	assert.Equal(t, "corp", resolved.Provider)
	assert.Equal(t, "https://models.example/v1", resolved.BaseURL)
	assert.Equal(t, []string{
		`model_provider from enterpriseManaged layer "acme-workspace" (sha256:abc)`,
	}, resolved.Provenance,
		"a route chosen by a layer the operator cannot read must carry its origin into the launch disclosure")

	*effective = codexEffectiveConfig{
		ModelProvider: "openai",
		OpenAIBaseURL: "https://openai-gateway.example/v1",
	}
	resolved, err = ResolveTclaudeLayerModelTransport(codex, context)
	require.NoError(t, err)
	assert.Equal(t, "openai", resolved.Provider)
	assert.Equal(t, "https://openai-gateway.example/v1", resolved.BaseURL)
}

func TestResolveTclaudeLayerCodexResolvesChatGPTSignInRoute(t *testing.T) {
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
	effective := stubCodexEffectiveConfig(t, codexEffectiveConfig{})
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(
		`{"auth_mode":"chatgpt","tokens":{"access_token":"token"}}`), 0o600))

	resolved, err := ResolveTclaudeLayerModelTransport(codex, context)
	require.NoError(t, err)
	assert.Equal(t, "openai", resolved.Provider)
	assert.Equal(t, "https://chatgpt.com/backend-api/", resolved.BaseURL)
	assert.Equal(t,
		[]string{"https://auth.openai.com/oauth/token"},
		resolved.AuxiliaryBaseURLs)

	requirement, err := harness.ResolveModelTransportRequirement(codex, resolved)
	require.NoError(t, err)
	assert.Equal(t, []sandboxpolicy.NetworkAllowEntry{
		{Domain: "chatgpt.com", Ports: []int{443}},
		{Domain: "auth.openai.com", Ports: []int{443}},
	}, requirement.Destinations)
	// The API-key pack does not cover the ChatGPT route, so a template
	// attribution here would over-report coverage.
	assert.Empty(t, requirement.Template)

	// A workspace override of chatgpt_base_url moves the model endpoint, and
	// the resolved requirement has to follow it rather than the default.
	effective.ChatGPTBaseURL = "https://codex.acme.example/backend-api/"
	resolved, err = ResolveTclaudeLayerModelTransport(codex, context)
	require.NoError(t, err)
	assert.Equal(t, "https://codex.acme.example/backend-api/", resolved.BaseURL)
	requirement, err = harness.ResolveModelTransportRequirement(codex, resolved)
	require.NoError(t, err)
	assert.Equal(t, []sandboxpolicy.NetworkAllowEntry{
		{Domain: "codex.acme.example", Ports: []int{443}},
		{Domain: "auth.openai.com", Ports: []int{443}},
	}, requirement.Destinations)
}

func TestResolveTclaudeLayerCodexRefusesUnresolvableEffectiveConfig(t *testing.T) {
	home, cwd := isolateModelTransportLaunch(t)
	codex := harness.MustGet(harness.CodexName)
	context := ModelTransportLaunchContext{
		Cwd:         cwd,
		Environment: []sandboxpolicy.EnvironmentEntry{{Name: "HOME", Value: home}},
	}
	previous := codexEffectiveConfigReader
	codexEffectiveConfigReader = func(
		string, []sandboxpolicy.EnvironmentEntry, string,
	) (codexEffectiveConfig, error) {
		return codexEffectiveConfig{}, errors.New(
			"the Codex app-server effective-config read did not answer within 45s")
	}
	t.Cleanup(func() { codexEffectiveConfigReader = previous })

	_, err := ResolveTclaudeLayerModelTransport(codex, context)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not answer")
	assert.Contains(t, err.Error(), "network open")
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

	stubCodexEffectiveConfig(t, codexEffectiveConfig{ModelProvider: "missing"})
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(
		`{"auth_mode":"apikey","OPENAI_API_KEY":"test-key"}`), 0o600))
	_, err := ResolveTclaudeLayerModelTransport(codex, context)
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

func TestResolveTclaudeLayerCodexRefusesUninspectableAuthRoute(t *testing.T) {
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
	effective := stubCodexEffectiveConfig(t, codexEffectiveConfig{})

	// A credential store tclaude cannot read hides which of the two very
	// different destination sets the launch will use.
	effective.AuthStore = "keyring"
	_, err := ResolveTclaudeLayerModelTransport(codex, context)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `cli_auth_credentials_store="keyring"`)
	assert.Contains(t, err.Error(), "network open")

	effective.AuthStore = ""
	context.Environment = append(context.Environment,
		sandboxpolicy.EnvironmentEntry{Name: "CODEX_ACCESS_TOKEN", Value: "token"})
	_, err = ResolveTclaudeLayerModelTransport(codex, context)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CODEX_ACCESS_TOKEN")
	assert.Contains(t, err.Error(), "network open")
}

func TestResolveTclaudeLayerCodexRefusesUnauthenticatedFirstPartyLaunch(t *testing.T) {
	home, cwd := isolateModelTransportLaunch(t)
	codexHome := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))

	_, err := ResolveTclaudeLayerModelTransport(
		harness.MustGet(harness.CodexName),
		ModelTransportLaunchContext{
			Cwd: cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "HOME", Value: home},
				{Name: "CODEX_HOME", Value: codexHome},
			},
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Codex authentication is present")
	assert.Contains(t, err.Error(), "network open")
}

func TestResolveTclaudeLayerCodexAllowsAuthlessExplicitProvider(t *testing.T) {
	home, cwd := isolateModelTransportLaunch(t)
	codexHome := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	stubCodexEffectiveConfig(t, codexEffectiveConfig{
		ModelProvider: "corp",
		ModelProviders: map[string]codexEffectiveProvider{
			"corp": {BaseURL: "https://models.example/v1"},
		},
	})

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
		ModelTransportLaunchContext{Cwd: cwd},
	)
	require.NoError(t, err)
	assert.Equal(t, harness.ResolvedModelTransport{}, resolved,
		"without a selected model, inference access is left to the authored rules")

	resolved, err = ResolveTclaudeLayerModelTransport(
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

	context.Environment[0].Value = strings.Replace(
		content,
		`"models":{"model":{"name":"Model"}}`,
		`"models":{"model":{"name":"Model","provider":{"npm":"file:///opaque.js"}}}`,
		1)
	_, err = ResolveTclaudeLayerModelTransport(openCode, context)
	require.ErrorContains(t, err, "may not override model.provider")
	require.ErrorContains(t, err, "network open")
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
	t.Setenv("CLAUDE_CODE_REMOTE_SETTINGS_PATH", "")
	stubCodexEffectiveConfig(t, codexEffectiveConfig{})
	return home, cwd
}

// stubCodexEffectiveConfig replaces the real app-server probe. Unit tests pin
// the resolution rules; the probe itself is covered by the pinned-harness smoke.
func stubCodexEffectiveConfig(
	t *testing.T,
	config codexEffectiveConfig,
) *codexEffectiveConfig {
	t.Helper()
	stub := &config
	previous := codexEffectiveConfigReader
	codexEffectiveConfigReader = func(
		string, []sandboxpolicy.EnvironmentEntry, string,
	) (codexEffectiveConfig, error) {
		return *stub, nil
	}
	t.Cleanup(func() { codexEffectiveConfigReader = previous })
	return stub
}

func TestResolveTclaudeLayerCodexChatGPTKeepsCustomProviderEndpoint(t *testing.T) {
	home, cwd := isolateModelTransportLaunch(t)
	codexHome := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	codex := harness.MustGet(harness.CodexName)
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(
		`{"auth_mode":"chatgpt","tokens":{"access_token":"token"}}`), 0o600))
	stubCodexEffectiveConfig(t, codexEffectiveConfig{
		ModelProvider:  "corp",
		ChatGPTBaseURL: "https://chatgpt.example/backend-api/",
		ModelProviders: map[string]codexEffectiveProvider{
			"corp": {BaseURL: "https://models.corp.example/v1", RequiresOpenAIAuth: true},
		},
	})

	// ChatGPT sign-in supplies the bearer token, but a custom provider still
	// posts to its own base_url. Resolving to chatgpt_base_url here would
	// authorize two destinations Codex never contacts and omit the one it does.
	resolved, err := ResolveTclaudeLayerModelTransport(
		codex, ModelTransportLaunchContext{
			Model: "gpt-5.4", Cwd: cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "HOME", Value: home},
				{Name: "CODEX_HOME", Value: codexHome},
			},
		})
	require.NoError(t, err)
	assert.Equal(t, "corp", resolved.Provider)
	assert.Equal(t, "https://models.corp.example/v1", resolved.BaseURL)
	assert.Equal(t,
		[]string{"https://auth.openai.com/oauth/token"},
		resolved.AuxiliaryBaseURLs,
		"the refresh endpoint still follows the credential")

	requirement, err := harness.ResolveModelTransportRequirement(codex, resolved)
	require.NoError(t, err)
	assert.Equal(t, []sandboxpolicy.NetworkAllowEntry{
		{Domain: "models.corp.example", Ports: []int{443}},
		{Domain: "auth.openai.com", Ports: []int{443}},
	}, requirement.Destinations)
}

func TestResolveTclaudeLayerCodexRefusesUnauthenticatedProviderNeedingOpenAIAuth(t *testing.T) {
	home, cwd := isolateModelTransportLaunch(t)
	codexHome := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	stubCodexEffectiveConfig(t, codexEffectiveConfig{
		ModelProvider: "corp",
		ModelProviders: map[string]codexEffectiveProvider{
			"corp": {BaseURL: "https://models.example/v1", RequiresOpenAIAuth: true},
		},
	})

	_, err := ResolveTclaudeLayerModelTransport(
		harness.MustGet(harness.CodexName),
		ModelTransportLaunchContext{
			Cwd: cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "HOME", Value: home},
				{Name: "CODEX_HOME", Value: codexHome},
			},
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires OpenAI authentication")
	assert.Contains(t, err.Error(), "network open")
}

func TestResolveTclaudeLayerCodexProbeSelectsTheLaunchPermissionProfile(t *testing.T) {
	home, cwd := isolateModelTransportLaunch(t)
	codexHome := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(
		`{"auth_mode":"apikey","OPENAI_API_KEY":"test-key"}`), 0o600))

	// tclaude passes `-p <profile>` itself on the stacked path, so it never
	// reaches the ExtraArgs gate. The profile layers its own config above the
	// base user config and can carry provider routing, so a probe that did not
	// select it would read a different config than the launch gets.
	var observed string
	restore := SetCodexEffectiveConfigProbeForTest(
		func(_ string, _ []sandboxpolicy.EnvironmentEntry, profile string) (json.RawMessage, error) {
			observed = profile
			return json.RawMessage(`{"config":{},"origins":{}}`), nil
		})
	t.Cleanup(restore)

	_, err := ResolveTclaudeLayerModelTransport(
		harness.MustGet(harness.CodexName),
		ModelTransportLaunchContext{
			Cwd: cwd,
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "HOME", Value: home},
				{Name: "CODEX_HOME", Value: codexHome},
			},
			PermissionProfile: "tclaude-agent",
		})
	require.NoError(t, err)
	assert.Equal(t, "tclaude-agent", observed)
}
