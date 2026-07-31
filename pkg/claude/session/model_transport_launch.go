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
	// PermissionProfile is the Codex permission profile the launch will pass as
	// `-p <name>`. tclaude sets this itself on the stacked path, so it is not
	// covered by the ExtraArgs gate; the profile layers
	// $CODEX_HOME/<name>.config.toml above the base user config and can carry
	// provider routing, so the effective-config probe has to select it too or
	// it reads a different config than the launch gets.
	PermissionProfile string
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
			fmt.Sprintf("model transport proxy variable %s puts the real destination behind a proxy this seam does not resolve, so the authored list cannot be checked against it; remove the proxy variable or use network open",
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

// AnnotateDenyDrivenFilteredModelTransport explains why an otherwise-open
// profile is being held to the filtered gateway's model-transport contract.
// Under a default-allow baseline the deny rows are the only reason the launch
// entered the filtered path, so a refusal naming just the model transport reads
// as unrelated to anything the operator authored. Enforcing those denies means
// this launch cannot silently continue with them dropped, so the refusal has to
// name the deny rules, the reason, and both ways out.
func AnnotateDenyDrivenFilteredModelTransport(
	rules sandboxpolicy.NetworkRules,
	err error,
) error {
	if err == nil ||
		rules.Mode != sandboxpolicy.AccessModeOpen ||
		len(rules.Deny) == 0 {
		return err
	}
	message := fmt.Sprintf(
		"this profile's network is open apart from %d enforced deny rule(s), and enforcing a deny requires the packet-filtered gateway, whose model transport this launch does not satisfy: %s; remove the deny rules to launch with open network, or resolve the model transport named above",
		len(rules.Deny), err)
	// The stable capability kind is what the CLI and dashboard use to render
	// the specific remedy, so re-mint the typed error rather than wrapping it
	// in a plain one whose message the failure converter would discard.
	var capabilityErr *harness.SandboxCapabilityError
	if errors.As(err, &capabilityErr) {
		return &harness.SandboxCapabilityError{
			Harness: capabilityErr.Harness,
			Kind:    capabilityErr.Kind,
			Message: message,
		}
	}
	return errors.New(message)
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
// explicit-provider filtering is supported because the frozen inline config is
// the provider authority; the local presets have no such authority and OpenCode
// exposes no effective-config read of its own loader, so they stay refused.
//
// TCL-895: that refusal is the PACKET gateway's. Its whole reason is that the
// gateway admits a destination only if the authored allow list can be checked
// against a launch endpoint resolved ahead of time, and these presets offer
// nothing to resolve one from. A proxy-engine launch decides on the identity
// the client states at connect time and needs no such pre-resolution, so
// refusing it here would describe a mechanism this launch does not run.
//
// The engine gate lives INSIDE this function rather than at its call sites,
// because both launch seams — the session boundary and the daemon spawn guard
// — call it, and a gate applied at one of them could drift from the other and
// from the rendered row. The renderer asks the same predicate through
// accessEnforcementTable's deployed-engine derivation.
//
// This is not a hole in the OpenCode launch gate: the ENGINE-INDEPENDENT
// model-transport resolve still runs for any filtered posture, so a
// proxy-engine local-preset launch without an explicit provider/model and
// inline explicit-provider config is still refused — which is exactly what the
// proxy row's OpenCodeFilteredExplicitProviderCaveat discloses.
func ValidateTclaudeLayerOpenCodeLocalModelTransport(
	h *harness.Harness,
	effective sandboxpolicy.EffectiveProfile,
	_ ModelTransportLaunchContext,
) error {
	engine, err := TclaudeLayerNetworkEngine(effective)
	if err != nil {
		return err
	}
	if engine == sandboxpolicy.NetworkEngineProxy {
		return nil
	}
	return modelTransportLaunchError(h,
		"OpenCode's local presets name no explicit provider and OpenCode exposes no effective-config read of its own loader, so their launch endpoint cannot be resolved; use an explicit-provider OpenCode config, use Claude Code or Codex with a resolvable provider, or use network open")
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
	// Claude Code 2.1.220 caches its remotely delivered managed settings here
	// and merges them as a policy-tier source. A verified payload is exempt
	// from the env filter that strips provider routing from an unverified one,
	// so this file can carry ANTHROPIC_BASE_URL or a provider selector just as
	// managed-settings.json can. The live fetch still happens after this
	// preflight; inspecting the cache catches the consented, persisted case
	// rather than pretending the channel does not exist.
	// Both are appended rather than either/or: a stale or wrong
	// CLAUDE_CODE_REMOTE_SETTINGS_PATH export must not make the default cache
	// invisible. A path that does not exist is skipped during inspection, so
	// naming both costs nothing and cannot under-inspect.
	if remote := strings.TrimSpace(
		environment["CLAUDE_CODE_REMOTE_SETTINGS_PATH"]); remote != "" {
		paths = append(paths, remote)
	}
	if configDir != "" {
		paths = append(paths, filepath.Join(configDir, "remote-settings.json"))
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
				"cannot locate the Codex home because HOME and CODEX_HOME are unset; set CODEX_HOME or use network open")
		}
		configDir = filepath.Join(home, ".codex")
	}
	config, err := codexEffectiveConfigReader(
		context.Cwd, context.Environment, context.PermissionProfile)
	if err != nil {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			err.Error()+"; fix the Codex installation or configuration, or use network open")
	}
	provider := config.ModelProvider
	if provider == "" {
		provider = "openai"
	}
	// A profile needs no separate gate here: `--profile` is refused above with
	// the other provider-changing pass-through arguments, and Codex 0.145.0's
	// own loader hard-errors on a legacy `profile` key, which surfaces as the
	// effective-config read failing rather than as a silent provider switch.

	authMode, authErr := codexFilteredAuthMode(configDir, config.AuthStore, environment)
	if authErr != nil {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			authErr.Error())
	}

	resolved := harness.ResolvedModelTransport{
		Model:            context.Model,
		Provider:         provider,
		Provenance:       codexRemoteProvenance(config.RemoteOrigins),
		ProviderResolved: true,
	}
	// ChatGPT sign-in substitutes chatgpt_base_url only for the built-in openai
	// provider. A custom provider keeps posting to its own base_url and merely
	// uses the ChatGPT token as its bearer, so the model endpoint is chosen by
	// the provider and only the refresh endpoint follows the credential.
	if provider == "openai" {
		if authMode == codexAuthChatGPT {
			baseURL := config.ChatGPTBaseURL
			if baseURL == "" {
				baseURL = codexDefaultChatGPTBaseURL
			}
			resolved.BaseURL = baseURL
		} else {
			resolved.BaseURL = config.OpenAIBaseURL
		}
	} else {
		configured, ok := config.ModelProviders[provider]
		if !ok || configured.BaseURL == "" {
			return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
				fmt.Sprintf("the effective Codex config selects provider %q with no concrete model_providers.%s.base_url; configure one or use network open",
					provider, provider))
		}
		resolved.BaseURL = configured.BaseURL
	}
	if authMode == codexAuthChatGPT {
		// The refresh endpoint is a compile-time constant in the harness
		// (codex-rs/login/src/auth/manager.rs REFRESH_TOKEN_URL), so no config
		// layer can move it and every ChatGPT-authenticated launch needs it,
		// whichever provider serves the model traffic.
		resolved.AuxiliaryBaseURLs = []string{codexChatGPTTokenRefreshURL}
	}
	if authMode == codexAuthNone && provider != "openai" {
		if configured, ok := config.ModelProviders[provider]; ok &&
			configured.RequiresOpenAIAuth {
			return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
				fmt.Sprintf("the effective Codex config selects provider %q, which requires OpenAI authentication, but no inspectable credential is present; sign in or use network open",
					provider))
		}
	}
	if authMode == codexAuthNone && provider == "openai" {
		return harness.ResolvedModelTransport{}, modelTransportLaunchError(h,
			"no Codex authentication is present, so the launch provider route cannot be resolved; sign in with an API key or ChatGPT, or use network open")
	}
	return resolved, nil
}

const (
	// codexDefaultChatGPTBaseURL matches the harness default applied when no
	// layer sets chatgpt_base_url (codex-rs/core/src/config/mod.rs).
	codexDefaultChatGPTBaseURL = "https://chatgpt.com/backend-api/"
	// codexChatGPTTokenRefreshURL is REFRESH_TOKEN_URL in
	// codex-rs/login/src/auth/manager.rs — a constant, not a config key.
	codexChatGPTTokenRefreshURL = "https://auth.openai.com/oauth/token"
)

type codexAuthMode int

const (
	codexAuthNone codexAuthMode = iota
	codexAuthAPIKey
	codexAuthChatGPT
)

// codexFilteredAuthMode reports which credential route the launch will take.
// The route decides the destination set — ChatGPT sign-in talks to
// chatgpt_base_url and the token-refresh endpoint, an API key talks to the
// model endpoint — so an uninspectable credential store is still refused: not
// because the credential is secret, but because the destination it selects is
// then unknown.
func codexFilteredAuthMode(
	configDir string,
	store string,
	environment map[string]string,
) (codexAuthMode, error) {
	if strings.TrimSpace(environment["CODEX_ACCESS_TOKEN"]) != "" {
		return codexAuthNone, codexFilteredAuthError(
			"CODEX_ACCESS_TOKEN supplies authentication from outside the inspected launch inputs")
	}
	switch normalized := strings.ToLower(strings.TrimSpace(store)); normalized {
	case "", "file":
	case "auto", "keyring", "ephemeral":
		return codexAuthNone, codexFilteredAuthError(fmt.Sprintf(
			"cli_auth_credentials_store=%q is not inspectable at the launch seam",
			normalized))
	default:
		return codexAuthNone, codexFilteredAuthError(fmt.Sprintf(
			"cli_auth_credentials_store=%q is unsupported", normalized))
	}

	path := filepath.Join(configDir, "auth.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return codexAuthNone, nil
	}
	if err != nil {
		return codexAuthNone, codexFilteredAuthError(fmt.Sprintf(
			"cannot inspect Codex authentication at %s: %v", path, err))
	}
	var auth struct {
		Mode         *string `json:"auth_mode"`
		OpenAIAPIKey *string `json:"OPENAI_API_KEY"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return codexAuthNone, codexFilteredAuthError(fmt.Sprintf(
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
	switch mode {
	case "apikey":
		if auth.OpenAIAPIKey == nil || strings.TrimSpace(*auth.OpenAIAPIKey) == "" {
			return codexAuthNone, codexFilteredAuthError(
				"persisted Codex API-key authentication is missing its key")
		}
		return codexAuthAPIKey, nil
	case "chatgpt":
		return codexAuthChatGPT, nil
	default:
		return codexAuthNone, codexFilteredAuthError(fmt.Sprintf(
			"persisted Codex auth mode %q selects an unknown model destination",
			mode))
	}
}

func codexFilteredAuthError(reason string) error {
	return fmt.Errorf(
		"%s, so filtered networking cannot tell which model destination this launch will use; sign in with a Codex API key or ChatGPT sign-in whose credentials are stored in the inspected auth file, or use network open",
		reason)
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
