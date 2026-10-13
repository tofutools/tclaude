package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// pi's filtered-network model route, read from pi-coding-agent and pi-ai at
// PiMinimumVersion.
//
// pi talks to exactly one provider per model, named by the model's
// `provider/id`. The endpoint is, in falling precedence:
//
//  1. models.json in the agent directory: a provider entry with a `baseUrl`
//     (a custom provider such as a local Ollama, or a built-in one pointed at
//     a proxy);
//  2. pi-ai's catalog, which fixes one first-party host per built-in provider.
//
// tclaude follows (1) as a custom endpoint and covers (2) with a reviewed
// table of the providers below. Everything else is REFUSED rather than
// guessed: a provider an extension registers, one whose host depends on
// account or region (Bedrock, Vertex, Azure, Cloudflare), GitHub Copilot
// (its endpoint comes from a token exchange), and agent-directory settings
// `httpProxy`, which pi applies as HTTP(S)_PROXY and so moves the route.
//
// Under a filtered network a pi launch also runs offline (PI_OFFLINE=1):
// no version check, catalog refresh or install telemetry to pi.dev, and no
// fd/rg download, so the model host is the only destination it needs. See
// PiFilteredNetworkEnv.
//
// The honest limit, as for every harness: this names what a launch NEEDS for
// model traffic. Web tools, MCP servers and a first `/login` are separate
// features with their own destinations.

// piProviderRoute is one reviewed built-in provider: its model host and any
// host its login needs to refresh a token.
type piProviderRoute struct {
	host      string
	auxiliary []string
}

// piProviderRoutes are pi-ai's catalog hosts for the providers tclaude has
// reviewed (providers/data/<provider>.json and auth/oauth at
// PiMinimumVersion). openai-codex is the ChatGPT-subscription route: chatgpt.com
// serves the model, and auth.openai.com refreshes the login.
var piProviderRoutes = map[string]piProviderRoute{
	"anthropic":    {host: "api.anthropic.com"},
	"openai":       {host: "api.openai.com"},
	"openai-codex": {host: "chatgpt.com", auxiliary: []string{"https://auth.openai.com"}},
	"google":       {host: "generativelanguage.googleapis.com"},
	"openrouter":   {host: "openrouter.ai"},
	"xai":          {host: "api.x.ai"},
	"groq":         {host: "api.groq.com"},
	"mistral":      {host: "api.mistral.ai"},
	"deepseek":     {host: "api.deepseek.com"},
	"cerebras":     {host: "api.cerebras.ai"},
	"together":     {host: "api.together.ai"},
	"fireworks":    {host: "api.fireworks.ai"},
	"zai":          {host: "api.z.ai"},
	"moonshotai":   {host: "api.moonshot.ai"},
	"huggingface":  {host: "router.huggingface.co"},
}

// PiFilteredNetworkEnv is the environment a pi launch runs with under a
// filtered network: offline, so pi starts no traffic outside its model
// route. Outside a filtered network tclaude sets none of these, and the
// operator's own pi settings decide.
func PiFilteredNetworkEnv() map[string]string {
	return map[string]string{
		"PI_OFFLINE":            "1",
		"PI_SKIP_VERSION_CHECK": "1",
		"PI_TELEMETRY":          "0",
	}
}

// piModelTransport is pi's ModelTransportResolver.
type piModelTransport struct{}

func (piModelTransport) ResolveModelTransport(
	resolved ResolvedModelTransport,
) (ModelTransportRequirement, error) {
	if !resolved.ProviderResolved {
		return ModelTransportRequirement{}, errors.New("pi model provider was not resolved")
	}
	provider := strings.TrimSpace(resolved.Provider)
	if endpoint := strings.TrimSpace(resolved.BaseURL); endpoint != "" {
		return customModelTransportRequirement(endpoint,
			fmt.Sprintf("pi models.json endpoint for provider %q", provider))
	}
	route, ok := piProviderRoutes[provider]
	if !ok {
		return ModelTransportRequirement{}, fmt.Errorf(
			"pi provider %q has no reviewed filtered-network route; give it a baseUrl in pi's models.json, "+
				"use a reviewed provider (%s), or use network open", provider, piReviewedProviders())
	}
	requirement := ModelTransportRequirement{
		Destinations: []sandboxpolicy.NetworkAllowEntry{{Domain: route.host, Ports: []int{443}}},
		ResolvedBy:   fmt.Sprintf("pi %s provider (pi catalog endpoint)", provider),
	}
	return withAuxiliaryModelTransport(requirement, route.auxiliary)
}

func piReviewedProviders() string {
	names := make([]string, 0, len(piProviderRoutes))
	for name := range piProviderRoutes {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// PiLaunchModelRoute resolves the provider a pi launch talks to and the
// endpoint models.json gives it ("" for pi's catalog endpoint). model is the
// launch's `--model`; with none, the agent directory's defaultProvider
// decides. getenv is the launch environment; cwd is the launch directory.
func PiLaunchModelRoute(getenv func(string) string, model, cwd string) (provider, baseURL string, err error) {
	agentDir, err := piAgentDir(getenv)
	if err != nil {
		return "", "", err
	}
	settings, err := readPiSettings(filepath.Join(agentDir, "settings.json"))
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(settings.HTTPProxy) != "" {
		return "", "", errors.New("pi's settings.json sets httpProxy, which puts the real destination " +
			"behind a proxy this route does not resolve; remove it or use network open")
	}
	model = strings.TrimSpace(model)
	if model != "" {
		var ok bool
		provider, _, ok = strings.Cut(model, "/")
		if !ok || provider == "" {
			return "", "", fmt.Errorf("pi model %q names no provider; pass --model provider/id so tclaude "+
				"can resolve the endpoint it needs", model)
		}
	} else {
		provider = strings.TrimSpace(settings.DefaultProvider)
		if provider == "" {
			return "", "", errors.New("pi has no default provider in its settings.json; pass --model " +
				"provider/id so tclaude can resolve the endpoint it needs")
		}
		project, err := readPiSettings(filepath.Join(cwd, ".pi", "settings.json"))
		if err != nil {
			return "", "", err
		}
		if p := strings.TrimSpace(project.DefaultProvider); p != "" && p != provider {
			return "", "", fmt.Errorf("the project's .pi/settings.json picks provider %q over the agent "+
				"directory's %q; pass --model provider/id so the route is not ambiguous", p, provider)
		}
	}
	baseURL, err = piModelsJSONBaseURL(filepath.Join(agentDir, "models.json"), provider)
	return provider, baseURL, err
}

type piSettings struct {
	DefaultProvider string `json:"defaultProvider"`
	HTTPProxy       string `json:"httpProxy"`
}

// readPiSettings reads the keys the route depends on. A missing file is no
// settings; a file pi would read but tclaude cannot parse is an error, never
// a guess.
func readPiSettings(path string) (piSettings, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return piSettings{}, nil
	}
	if err != nil {
		return piSettings{}, err
	}
	var settings piSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return piSettings{}, fmt.Errorf("read pi settings %s: %w", path, err)
	}
	return settings, nil
}

// piModelsJSONBaseURL is the baseUrl models.json gives provider, "" when it
// gives none.
func piModelsJSONBaseURL(path, provider string) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var models struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &models); err != nil {
		return "", fmt.Errorf("read pi models.json %s: %w", path, err)
	}
	return strings.TrimSpace(models.Providers[provider].BaseURL), nil
}

// PiSessionModel is the `provider/id` model a resumed session continues with,
// as pi's getBranchSelection picks it: walking back along the current branch,
// a model change, or the physical model of the latest reply. A reply routed
// through a virtual model names the physical model, so the model change before
// it holds when it names another one; tclaude cannot tell a virtual model from
// a since-unregistered one without pi's catalog, and the virtual one is the
// route a filtered launch must resolve, or refuse. "" when the session records
// no model (pi then falls back to its settings default) or cannot be read.
func PiSessionModel(id, cwd string) string {
	path := piLocateSessionFile(id, cwd)
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	session, err := readPiSession(f)
	if err != nil {
		return ""
	}
	branch := session.Branch()
	for i := len(branch) - 1; i >= 0; i-- {
		e := branch[i]
		if e.Type == "model_change" && e.ModelID != "" {
			return piModelRef(e.Provider, e.ModelID)
		}
		if e.Type != "message" || e.Message == nil || e.Message.Role != "assistant" ||
			e.Message.Model == "" || e.Message.API == piVirtualModelAPI {
			continue
		}
		response := piModelRef(e.Message.Provider, e.Message.Model)
		for j := i - 1; j >= 0; j-- {
			if c := branch[j]; c.Type == "model_change" && c.ModelID != "" {
				if change := piModelRef(c.Provider, c.ModelID); change != response {
					return change
				}
				break
			}
		}
		return response
	}
	return ""
}

// piVirtualModelAPI is the api pi records on a reply a virtual model failed to
// route (core/virtual-models.js).
const piVirtualModelAPI = "pi-virtual"
