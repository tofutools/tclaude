package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ModelTransportLaunchContext contains only inputs that the executing harness
// will actually consume. A resolver may mint ProviderResolved only from this
// boundary; editor predictions and model names alone are not provider proof.
type ModelTransportLaunchContext struct {
	Model       string
	Cwd         string
	Environment []sandboxpolicy.EnvironmentEntry
	ExtraArgs   []string
}

// ResolveTclaudeLayerModelTransport resolves the concrete provider endpoint for
// one tclaude-layer launch. It deliberately refuses provider-changing inputs it
// cannot reproduce exactly instead of guessing the harness default.
func ResolveTclaudeLayerModelTransport(
	h *harness.Harness,
	context ModelTransportLaunchContext,
) (harness.ResolvedModelTransport, error) {
	if h == nil {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(
			nil, "cannot resolve provider configuration without a harness")
	}
	environment := launchModelEnvironment(context.Environment)
	if variable := modelTransportProxyVariable(environment); variable != "" {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			fmt.Sprintf("model transport proxy variable %s changes the actual network boundary; remove it or use network open (proxy-aware resolution tracked in TCL-826)",
				variable))
	}
	switch h.Name {
	case harness.DefaultName:
		return resolveClaudeModelTransport(h, context, environment)
	case harness.CodexName:
		return resolveCodexModelTransport(h, context, environment)
	case harness.OpenCodeName:
		return resolveOpenCodeModelTransport(h, context, environment)
	default:
		return harness.ResolvedModelTransport{
			Model: context.Model,
		}, nil
	}
}

const openCodeFilteredProviderNPM = "@ai-sdk/openai-compatible"

var openCodeManagedConfigPaths = func() []string {
	return []string{
		"/etc/opencode/opencode.json",
		"/etc/opencode/opencode.jsonc",
	}
}

type openCodeFilteredConfig struct {
	EnabledProviders []string                            `json:"enabled_providers"`
	Provider         map[string]openCodeFilteredProvider `json:"provider"`
}

type openCodeFilteredProvider struct {
	NPM       string                     `json:"npm"`
	Whitelist []string                   `json:"whitelist"`
	Models    map[string]json.RawMessage `json:"models"`
	Options   struct {
		BaseURL json.RawMessage `json:"baseURL"`
		APIKey  json.RawMessage `json:"apiKey"`
	} `json:"options"`
}

// resolveOpenCodeModelTransport accepts only the pinned OpenCode loader's
// explicit-provider shape. The executing server separately forces the loader's
// own isolation affordances, making this inline, frozen profile value the
// provider authority instead of guessing a built-in or remotely mutable
// default.
func resolveOpenCodeModelTransport(
	h *harness.Harness,
	context ModelTransportLaunchContext,
	environment map[string]string,
) (harness.ResolvedModelTransport, error) {
	provider, modelID, ok := strings.Cut(strings.TrimSpace(context.Model), "/")
	if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(modelID) == "" {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			"OpenCode filtered networking requires an explicit provider/model launch model and inline explicit-provider config; choose one or use network open")
	}
	provider = strings.TrimSpace(provider)
	modelID = strings.TrimSpace(modelID)

	for _, path := range openCodeManagedConfigPaths() {
		if _, err := os.Stat(path); err == nil {
			return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
				fmt.Sprintf("OpenCode managed config %s loads after inline config and can change provider routing; remove the managed provider config or use network open", path))
		} else if !errors.Is(err, os.ErrNotExist) {
			return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
				fmt.Sprintf("cannot inspect OpenCode managed config %s: %v; fix its visibility or use network open", path, err))
		}
	}

	content := ""
	for _, entry := range context.Environment {
		if entry.Name == "OPENCODE_CONFIG_CONTENT" {
			content = entry.Value
		}
	}
	if strings.TrimSpace(content) == "" {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			"OpenCode filtered networking supports explicit-provider configs only: set frozen profile environment OPENCODE_CONFIG_CONTENT with the launch provider/model and concrete options.baseURL, or use network open")
	}
	var config openCodeFilteredConfig
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			"cannot inspect OPENCODE_CONFIG_CONTENT as the strict filtered-provider shape: "+err.Error()+"; fix the inline config or use network open")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			"OPENCODE_CONFIG_CONTENT contains trailing data; fix the inline config or use network open")
	}
	if len(config.EnabledProviders) != 1 ||
		strings.TrimSpace(config.EnabledProviders[0]) != provider {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			fmt.Sprintf("OpenCode filtered config must set enabled_providers to exactly [%q] so the executing server cannot switch provider routes; fix OPENCODE_CONFIG_CONTENT or use network open", provider))
	}
	if len(config.Provider) != 1 {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			"OpenCode filtered config must define exactly one explicit provider; fix OPENCODE_CONFIG_CONTENT or use network open")
	}
	configured, found := config.Provider[provider]
	if !found {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			fmt.Sprintf("OpenCode filtered config does not define launch provider %q; fix OPENCODE_CONFIG_CONTENT or use network open", provider))
	}
	if strings.TrimSpace(configured.NPM) != openCodeFilteredProviderNPM {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			fmt.Sprintf("OpenCode filtered provider %q must use the inspected %s adapter; other provider loaders remain opaque, so choose that adapter or use network open",
				provider, openCodeFilteredProviderNPM))
	}
	if len(configured.Whitelist) != 1 ||
		strings.TrimSpace(configured.Whitelist[0]) != modelID {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			fmt.Sprintf("OpenCode filtered provider %q must whitelist exactly launch model %q; fix OPENCODE_CONFIG_CONTENT or use network open",
				provider, modelID))
	}
	if len(configured.Models) != 1 {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			fmt.Sprintf("OpenCode filtered provider %q must define exactly one model; fix OPENCODE_CONFIG_CONTENT or use network open", provider))
	}
	if _, found := configured.Models[modelID]; !found {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			fmt.Sprintf("OpenCode filtered provider %q does not define launch model %q; fix OPENCODE_CONFIG_CONTENT or use network open",
				provider, modelID))
	}
	var selectedModel map[string]json.RawMessage
	if err := json.Unmarshal(configured.Models[modelID], &selectedModel); err != nil {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			fmt.Sprintf("OpenCode filtered provider %q model %q is not inspectable JSON; fix OPENCODE_CONFIG_CONTENT or use network open",
				provider, modelID))
	}
	if _, found := selectedModel["provider"]; found {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			fmt.Sprintf("OpenCode filtered provider %q model %q may not override model.provider: it can replace the inspected adapter or endpoint; remove the override or use network open",
				provider, modelID))
	}
	var baseURL string
	if len(configured.Options.BaseURL) == 0 ||
		json.Unmarshal(configured.Options.BaseURL, &baseURL) != nil ||
		strings.TrimSpace(baseURL) == "" {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			fmt.Sprintf("OpenCode filtered provider %q requires a concrete string options.baseURL; fix OPENCODE_CONFIG_CONTENT or use network open", provider))
	}
	baseURL = strings.TrimSpace(baseURL)
	if strings.Contains(baseURL, "{env:") || strings.Contains(baseURL, "{file:") ||
		strings.Contains(baseURL, "${") {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			fmt.Sprintf("OpenCode filtered provider %q options.baseURL may not use environment, file, or runtime substitution; put the concrete endpoint inline or use network open", provider))
	}
	_ = environment // proxy inspection is shared above; content must be frozen.
	return harness.ResolvedModelTransport{
		Model:            context.Model,
		Provider:         provider,
		BaseURL:          baseURL,
		ProviderResolved: true,
	}, nil
}

// ValidateTclaudeLayerOpenCodeLocalModelTransport keeps the local convenience
// presets behind the existing model-transport launch gate. General OpenCode
// explicit-provider filtering is supported, but resolving an effective local
// provider for these presets belongs to TCL-826.
func ValidateTclaudeLayerOpenCodeLocalModelTransport(
	h *harness.Harness,
	_ sandboxpolicy.EffectiveProfile,
	_ ModelTransportLaunchContext,
) error {
	return modelTransportLaunchError(h,
		"OpenCode local-preset effective-config model transport resolution is tracked in TCL-826; use Claude Code or Codex with a resolvable provider, or use network open")
}

func resolveClaudeModelTransport(
	h *harness.Harness,
	context ModelTransportLaunchContext,
	environment map[string]string,
) (harness.ResolvedModelTransport, error) {
	if flag, ok := providerChangingArg(context.ExtraArgs, map[string]bool{
		"--settings":        true,
		"--setting-sources": true,
	}); ok {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			fmt.Sprintf("Claude pass-through argument %q can change provider routing; remove it or use network open", flag))
	}
	if source, variable, err := claudeSettingsProviderVariable(
		context.Cwd, environment); err != nil {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			"cannot inspect Claude provider settings: "+err.Error()+"; fix the settings file or use network open")
	} else if variable != "" {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			fmt.Sprintf("Claude provider routing is mutable through %s (%s); remove that provider variable from settings, put a concrete endpoint in the launch environment, or use network open",
				source, variable))
	}

	for _, variable := range []string{
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS",
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CODE_USE_MANTLE",
		"CLAUDE_CODE_USE_VERTEX",
		"ANTHROPIC_BEDROCK_BASE_URL",
		"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
		"ANTHROPIC_FOUNDRY_BASE_URL",
		"ANTHROPIC_VERTEX_BASE_URL",
	} {
		if strings.TrimSpace(environment[variable]) != "" {
			return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
				fmt.Sprintf("Claude provider variable %s has no reviewed filtered-network endpoint resolver; choose the direct Anthropic provider with a concrete ANTHROPIC_BASE_URL, remove the variable, or use network open",
					variable))
		}
	}
	return harness.ResolvedModelTransport{
		Model:            context.Model,
		Provider:         "anthropic",
		BaseURL:          strings.TrimSpace(environment["ANTHROPIC_BASE_URL"]),
		ProviderResolved: true,
	}, nil
}

var claudeProviderSettingVariables = map[string]struct{}{
	"ANTHROPIC_BASE_URL":                   {},
	"ANTHROPIC_BEDROCK_BASE_URL":           {},
	"ANTHROPIC_BEDROCK_MANTLE_BASE_URL":    {},
	"ANTHROPIC_FOUNDRY_BASE_URL":           {},
	"ANTHROPIC_VERTEX_BASE_URL":            {},
	"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST": {},
	"CLAUDE_CODE_USE_ANTHROPIC_AWS":        {},
	"CLAUDE_CODE_USE_BEDROCK":              {},
	"CLAUDE_CODE_USE_FOUNDRY":              {},
	"CLAUDE_CODE_USE_MANTLE":               {},
	"CLAUDE_CODE_USE_VERTEX":               {},
	"HTTPS_PROXY":                          {},
	"https_proxy":                          {},
	"HTTP_PROXY":                           {},
	"http_proxy":                           {},
	"ALL_PROXY":                            {},
	"all_proxy":                            {},
}

// claudeSettingsProviderVariable refuses provider routing in live-reloaded
// settings. Claude reapplies settings env values while a session is running;
// a one-time preflight therefore cannot truthfully freeze or follow them.
func claudeSettingsProviderVariable(
	cwd string,
	environment map[string]string,
) (source, variable string, err error) {
	for _, path := range claudeProviderSettingsPaths(cwd, environment) {
		data, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return "", "", fmt.Errorf("read %s: %w", path, readErr)
		}
		var settings struct {
			Environment map[string]json.RawMessage `json:"env"`
		}
		if jsonErr := json.Unmarshal(data, &settings); jsonErr != nil {
			return "", "", fmt.Errorf("parse %s", path)
		}
		for name, raw := range settings.Environment {
			if _, relevant := claudeProviderSettingVariables[name]; !relevant {
				continue
			}
			var value string
			if valueErr := json.Unmarshal(raw, &value); valueErr != nil {
				return "", "", fmt.Errorf("parse %s env.%s", path, name)
			}
			if strings.TrimSpace(value) != "" {
				return path, name, nil
			}
		}
	}
	return "", "", nil
}

func claudeProviderSettingsPaths(
	cwd string,
	environment map[string]string,
) []string {
	paths := claudeManagedProviderSettingsPaths()
	configDir := strings.TrimSpace(environment["CLAUDE_CONFIG_DIR"])
	if configDir == "" {
		if home := strings.TrimSpace(environment["HOME"]); home != "" {
			configDir = filepath.Join(home, ".claude")
		}
	}
	if configDir != "" {
		paths = append(paths, filepath.Join(configDir, "settings.json"))
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	for _, dir := range ancestorDirsForModelTransport(cwd) {
		paths = append(paths,
			filepath.Join(dir, ".claude", "settings.local.json"),
			filepath.Join(dir, ".claude", "settings.json"))
	}
	return uniqueCleanPaths(paths)
}

func claudeManagedProviderSettingsPaths() []string {
	root := claudeManagedProviderSettingsRoot()
	paths := []string{filepath.Join(root, "managed-settings.json")}
	entries, err := os.ReadDir(filepath.Join(root, "managed-settings.d"))
	if err != nil {
		return paths
	}
	var dropIns []string
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") ||
			!strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		dropIns = append(dropIns,
			filepath.Join(root, "managed-settings.d", entry.Name()))
	}
	sort.Strings(dropIns)
	return append(paths, dropIns...)
}

var claudeManagedProviderSettingsRoot = func() string {
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/ClaudeCode"
	}
	return "/etc/claude-code"
}

func resolveCodexModelTransport(
	h *harness.Harness,
	context ModelTransportLaunchContext,
	environment map[string]string,
) (harness.ResolvedModelTransport, error) {
	if flag, ok := providerChangingArg(context.ExtraArgs, map[string]bool{
		"-c":               true,
		"--config":         true,
		"-p":               true,
		"--profile":        true,
		"--oss":            false,
		"--local-provider": true,
		"--remote":         false,
	}); ok {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			fmt.Sprintf("Codex pass-through argument %q can change provider routing; remove it or use network open", flag))
	}
	configDir := strings.TrimSpace(environment["CODEX_HOME"])
	if configDir == "" {
		home := strings.TrimSpace(environment["HOME"])
		if home == "" {
			return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
				"cannot locate Codex config.toml because HOME and CODEX_HOME are unset; set CODEX_HOME or use network open")
		}
		configDir = filepath.Join(home, ".codex")
	}
	for _, path := range codexExternalModelConfigPaths() {
		key, inspectErr := codexProviderKeyInConfig(path)
		if inspectErr != nil {
			return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
				inspectErr.Error()+"; fix the external Codex config or use network open")
		}
		if key != "" {
			return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
				fmt.Sprintf("Codex external config %s sets provider-routing key %s, which this launch seam cannot merge exactly; remove the override, move the concrete provider into CODEX_HOME/config.toml, or use network open",
					path, key))
		}
	}
	config, err := readCodexModelTransportConfig(
		filepath.Join(configDir, "config.toml"))
	if err != nil {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			err.Error()+"; fix config.toml or use network open")
	}
	provider := strings.TrimSpace(config.ModelProvider)
	if provider == "" {
		provider = "openai"
	}
	if strings.TrimSpace(config.Profile) != "" || len(config.Profiles) != 0 {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			"Codex 0.145.0 rejects legacy profile/[profiles] configuration; remove it and use concrete top-level provider configuration, or use network open")
	}
	if strings.TrimSpace(config.ChatGPTBaseURL) != "" {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			"Codex chatgpt_base_url overrides the pinned first-party auth endpoint; remove the override or use network open")
	}

	baseURL := ""
	requiresOpenAIAuth := true
	if provider == "openai" {
		baseURL = strings.TrimSpace(config.OpenAIBaseURL)
	} else {
		configured, ok := config.ModelProviders[provider]
		if !ok || strings.TrimSpace(configured.BaseURL) == "" {
			return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
				fmt.Sprintf("Codex provider %q has no concrete model_providers.%s.base_url; configure one or use network open",
					provider, provider))
		}
		baseURL = strings.TrimSpace(configured.BaseURL)
		requiresOpenAIAuth = configured.RequiresOpenAIAuth
	}
	if authErr := validateCodexFilteredAuth(
		configDir, config.AuthCredentialsStore, environment, requiresOpenAIAuth,
	); authErr != nil {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			authErr.Error())
	}
	return harness.ResolvedModelTransport{
		Model:            context.Model,
		Provider:         provider,
		BaseURL:          baseURL,
		ProviderResolved: true,
	}, nil
}

type codexModelTransportConfig struct {
	ModelProvider        string `toml:"model_provider"`
	Profile              string `toml:"profile"`
	OpenAIBaseURL        string `toml:"openai_base_url"`
	ChatGPTBaseURL       string `toml:"chatgpt_base_url"`
	AuthCredentialsStore string `toml:"cli_auth_credentials_store"`
	ModelProviders       map[string]struct {
		BaseURL            string `toml:"base_url"`
		RequiresOpenAIAuth bool   `toml:"requires_openai_auth"`
	} `toml:"model_providers"`
	Profiles map[string]struct {
		ModelProvider  string `toml:"model_provider"`
		ChatGPTBaseURL string `toml:"chatgpt_base_url"`
	} `toml:"profiles"`
}

func validateCodexFilteredAuth(
	configDir string,
	store string,
	environment map[string]string,
	requiresOpenAIAuth bool,
) error {
	if strings.TrimSpace(environment["CODEX_ACCESS_TOKEN"]) != "" {
		return codexFilteredAuthError(
			"CODEX_ACCESS_TOKEN selects externally supplied authentication")
	}
	switch normalized := strings.ToLower(strings.TrimSpace(store)); normalized {
	case "", "file":
	case "auto", "keyring", "ephemeral":
		return codexFilteredAuthError(fmt.Sprintf(
			"cli_auth_credentials_store=%q is not inspectable at the launch seam",
			normalized))
	default:
		return codexFilteredAuthError(fmt.Sprintf(
			"cli_auth_credentials_store=%q is unsupported", normalized))
	}

	path := filepath.Join(configDir, "auth.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if requiresOpenAIAuth {
			return codexFilteredAuthError(
				"no persisted API-key authentication was found")
		}
		return nil
	}
	if err != nil {
		return codexFilteredAuthError(fmt.Sprintf(
			"cannot inspect Codex authentication at %s: %v", path, err))
	}
	var auth struct {
		Mode         *string `json:"auth_mode"`
		OpenAIAPIKey *string `json:"OPENAI_API_KEY"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return codexFilteredAuthError(fmt.Sprintf(
			"cannot parse Codex authentication at %s", path))
	}
	mode := ""
	if auth.Mode != nil {
		mode = strings.TrimSpace(*auth.Mode)
	} else if auth.OpenAIAPIKey != nil {
		mode = "apikey"
	} else {
		mode = "chatgpt"
	}
	if mode != "apikey" {
		return codexFilteredAuthError(fmt.Sprintf(
			"persisted Codex auth mode %q can load remote provider overrides",
			mode))
	}
	if auth.OpenAIAPIKey == nil || strings.TrimSpace(*auth.OpenAIAPIKey) == "" {
		return codexFilteredAuthError(
			"persisted Codex API-key authentication is missing its key")
	}
	return nil
}

func codexFilteredAuthError(reason string) error {
	return fmt.Errorf(
		"%s; filtered networking requires an inspectable API-key route because ChatGPT-auth provider overrides can refresh after launch. Sign in with a Codex API key or use network open (dynamic provider-resolution remedy tracked in TCL-826)",
		reason)
}

var codexExternalModelConfigPaths = func() []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	return []string{
		"/etc/codex/managed_config.toml",
		"/etc/codex/config.toml",
	}
}

func codexProviderKeyInConfig(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read Codex external config %s: %w", path, err)
	}
	var config map[string]any
	if err := toml.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("parse Codex external config %s", path)
	}
	for _, key := range []string{
		"model_provider",
		"model_providers",
		"openai_base_url",
		"chatgpt_base_url",
		"profile",
		"profiles",
	} {
		if _, ok := config[key]; ok {
			return key, nil
		}
	}
	return "", nil
}

func readCodexModelTransportConfig(path string) (codexModelTransportConfig, error) {
	var config codexModelTransportConfig
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return config, fmt.Errorf("read Codex config %s: %w", path, err)
	}
	if err := toml.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("parse Codex config %s", path)
	}
	return config, nil
}

func launchModelEnvironment(
	overrides []sandboxpolicy.EnvironmentEntry,
) map[string]string {
	environment := make(map[string]string)
	for _, pair := range os.Environ() {
		name, value, ok := strings.Cut(pair, "=")
		if ok {
			environment[name] = value
		}
	}
	for _, entry := range overrides {
		environment[entry.Name] = entry.Value
	}
	return environment
}

func modelTransportProxyVariable(environment map[string]string) string {
	for _, variable := range []string{
		"HTTPS_PROXY", "https_proxy",
		"HTTP_PROXY", "http_proxy",
		"ALL_PROXY", "all_proxy",
	} {
		if strings.TrimSpace(environment[variable]) != "" {
			return variable
		}
	}
	return ""
}

func providerChangingArg(
	args []string,
	flags map[string]bool,
) (string, bool) {
	for _, argument := range args {
		name := argument
		if before, _, ok := strings.Cut(argument, "="); ok {
			name = before
		}
		if _, found := flags[name]; found {
			return name, true
		}
		for flag, takesValue := range flags {
			if !takesValue || !strings.HasPrefix(flag, "-") ||
				strings.HasPrefix(flag, "--") {
				continue
			}
			if strings.HasPrefix(argument, flag) && len(argument) > len(flag) {
				return flag, true
			}
		}
	}
	return "", false
}

func modelTransportLaunchError(
	h *harness.Harness,
	message string,
) error {
	name := ""
	if h != nil {
		name = h.Name
	}
	return &harness.SandboxCapabilityError{
		Harness: name,
		Kind:    harness.SandboxCapabilityModelTransport,
		Message: message,
	}
}

func ancestorDirsForModelTransport(path string) []string {
	path = filepath.Clean(path)
	var dirs []string
	for path != "." && path != string(filepath.Separator) {
		dirs = append(dirs, path)
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	return dirs
}

func uniqueCleanPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		if clean == "." {
			continue
		}
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}
