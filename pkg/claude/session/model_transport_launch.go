package session

import (
	"encoding/json"
	"errors"
	"fmt"
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
	switch h.Name {
	case harness.DefaultName:
		return resolveClaudeModelTransport(h, context, environment)
	case harness.CodexName:
		return resolveCodexModelTransport(h, context, environment)
	default:
		return harness.ResolvedModelTransport{
			Model: context.Model,
		}, nil
	}
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
	if profileName := strings.TrimSpace(config.Profile); profileName != "" {
		profile, ok := config.Profiles[profileName]
		if !ok {
			return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
				fmt.Sprintf("Codex profile %q is selected but not defined; fix config.toml or use network open",
					profileName))
		}
		if selected := strings.TrimSpace(profile.ModelProvider); selected != "" {
			provider = selected
		}
		if strings.TrimSpace(profile.ChatGPTBaseURL) != "" {
			return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
				fmt.Sprintf("Codex profile %q overrides chatgpt_base_url, which has no complete filtered auth-endpoint resolver; remove the override or use network open",
					profileName))
		}
	}
	if strings.TrimSpace(config.ChatGPTBaseURL) != "" {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			"Codex chatgpt_base_url overrides the pinned first-party auth endpoint; remove the override or use network open")
	}

	baseURL := ""
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
	}
	return harness.ResolvedModelTransport{
		Model:            context.Model,
		Provider:         provider,
		BaseURL:          baseURL,
		ProviderResolved: true,
	}, nil
}

type codexModelTransportConfig struct {
	ModelProvider  string `toml:"model_provider"`
	Profile        string `toml:"profile"`
	OpenAIBaseURL  string `toml:"openai_base_url"`
	ChatGPTBaseURL string `toml:"chatgpt_base_url"`
	ModelProviders map[string]struct {
		BaseURL string `toml:"base_url"`
	} `toml:"model_providers"`
	Profiles map[string]struct {
		ModelProvider  string `toml:"model_provider"`
		ChatGPTBaseURL string `toml:"chatgpt_base_url"`
	} `toml:"profiles"`
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
