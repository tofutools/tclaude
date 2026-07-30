package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Codex's own config loader is the only honest authority on which provider a
// launch will actually use. Its layer stack includes an enterprise cloud-config
// bundle delivered out-of-band of any file tclaude can read, so a local
// config.toml parser cannot mint a truthful ProviderResolved. The app-server
// `config/read` request returns the merged effective config plus, per key, the
// layer that won and a content hash for it, which is exactly the evidence a
// filtered preflight needs.
//
// Analyzed against codex-cli 0.146.0 and confirmed byte-identical in shape at
// the CI-pinned 0.145.0:
// codex-rs/app-server-protocol/src/protocol/v2/config.rs defines
// ConfigReadParams{cwd,includeLayers} / ConfigReadResponse{config,origins,layers}
// and the ConfigLayerSource precedence mdm(0) < system(10) <
// enterpriseManaged(15) < user(20) < project(25) < sessionFlags(30).
const (
	codexEffectiveConfigTimeout = 45 * time.Second

	// Layer source discriminators from ConfigLayerSource. Only the two remote
	// channels are named here; the rest are ordinary local files.
	codexConfigLayerEnterprise = "enterpriseManaged"
	codexConfigLayerMDM        = "mdm"
)

// codexProviderRoutingKeys are the effective-config keys that can move model
// traffic to a different destination. Every one of them is reported with its
// winning layer so the launch disclosure can name a remote origin.
var codexProviderRoutingKeys = []string{
	"model_provider",
	"model_providers",
	"openai_base_url",
	"chatgpt_base_url",
	"profile",
	"profiles",
}

// codexEffectiveConfig is the provider-routing projection of one
// `config/read`. Everything else Codex reports is deliberately dropped: this
// seam decides network destinations, not harness behavior.
type codexEffectiveConfig struct {
	ModelProvider  string
	OpenAIBaseURL  string
	ChatGPTBaseURL string
	AuthStore      string
	ModelProviders map[string]codexEffectiveProvider
	// RemoteOrigins names, per routing key, the remotely delivered layer that
	// won it. Empty when every routing key came from a local file.
	RemoteOrigins []codexRemoteConfigOrigin
}

type codexEffectiveProvider struct {
	BaseURL            string
	RequiresOpenAIAuth bool
}

// codexRemoteConfigOrigin is one provider-routing key whose winning layer was
// delivered remotely rather than read from a file the operator can inspect.
type codexRemoteConfigOrigin struct {
	Key     string
	Layer   string
	Name    string
	Version string
}

func (o codexRemoteConfigOrigin) String() string {
	name := o.Name
	if strings.TrimSpace(name) == "" {
		name = o.Layer
	}
	return fmt.Sprintf("%s from %s layer %q (%s)", o.Key, o.Layer, name, o.Version)
}

// codexEffectiveConfigReader is the process seam. Production runs the real
// Codex binary; unit tests substitute a reader so they can exercise the
// resolution rules without a harness install.
var codexEffectiveConfigReader = readCodexEffectiveConfig

// SetCodexEffectiveConfigProbeForTest swaps the Codex effective-config
// subprocess boundary and returns a restore function. Resolving a filtered
// Codex launch executes the real `codex` binary, so flow tests on a host with
// no Codex install would otherwise refuse every Codex launch for a missing
// executable instead of exercising the path under test.
//
// The replacement supplies the raw `config/read` result, not a parsed struct,
// so a test still runs the production parser and layer-origin attribution over
// the shape Codex actually returns.
func SetCodexEffectiveConfigProbeForTest(
	read func(cwd string, environment []sandboxpolicy.EnvironmentEntry) (json.RawMessage, error),
) func() {
	previous := codexEffectiveConfigReader
	codexEffectiveConfigReader = func(
		cwd string,
		environment []sandboxpolicy.EnvironmentEntry,
	) (codexEffectiveConfig, error) {
		raw, err := read(cwd, environment)
		if err != nil {
			return codexEffectiveConfig{}, err
		}
		return parseCodexEffectiveConfig(raw)
	}
	return func() { codexEffectiveConfigReader = previous }
}

// readCodexEffectiveConfig starts a short-lived Codex app-server, asks it for
// the effective config as seen from the launch cwd, and stops it again. This is
// a launch-time side effect: Codex refreshes and re-signs its cloud-config
// bundle cache on start, so the probe can perform network I/O outside the
// sandbox before the sandbox exists. That is disclosed rather than avoided,
// because the alternative is resolving from a stale local file.
func readCodexEffectiveConfig(
	cwd string,
	environment []sandboxpolicy.EnvironmentEntry,
) (codexEffectiveConfig, error) {
	ctx, cancel := context.WithTimeout(
		context.Background(), codexEffectiveConfigTimeout)
	defer cancel()

	// --disable only sets features.<name>, never a provider-routing key, so the
	// probe still reads the routing this launch will get. Plugin/marketplace
	// sync is unrelated to model transport and would otherwise make the probe
	// wait on catalog fetches.
	command := exec.CommandContext(ctx, "codex",
		"--disable", "plugins",
		"--disable", "remote_plugin",
		"--disable", "plugin_sharing",
		"app-server", "--listen", "stdio://")
	command.Dir = cwd
	command.Env = codexEffectiveConfigEnv(environment)
	command.Stderr = nil

	stdin, err := command.StdinPipe()
	if err != nil {
		return codexEffectiveConfig{}, fmt.Errorf(
			"cannot drive the Codex app-server effective-config read: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return codexEffectiveConfig{}, fmt.Errorf(
			"cannot drive the Codex app-server effective-config read: %w", err)
	}
	if err := command.Start(); err != nil {
		return codexEffectiveConfig{}, fmt.Errorf(
			"cannot run the Codex app-server effective-config read: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
	}()

	requests := []any{
		map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{"clientInfo": map[string]any{
				"name": "tclaude", "title": "tclaude", "version": "0",
			}},
		},
		map[string]any{"jsonrpc": "2.0", "method": "initialized"},
		map[string]any{
			"jsonrpc": "2.0", "id": 2, "method": "config/read",
			"params": codexConfigReadParams(cwd),
		},
	}
	for _, request := range requests {
		encoded, marshalErr := json.Marshal(request)
		if marshalErr != nil {
			return codexEffectiveConfig{}, marshalErr
		}
		if _, writeErr := stdin.Write(append(encoded, '\n')); writeErr != nil {
			return codexEffectiveConfig{}, fmt.Errorf(
				"the Codex app-server closed before answering config/read: %w",
				writeErr)
		}
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var message struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			continue
		}
		if message.ID == nil || *message.ID != 2 {
			continue
		}
		if message.Error != nil {
			return codexEffectiveConfig{}, fmt.Errorf(
				"the Codex app-server refused config/read: %s",
				message.Error.Message)
		}
		return parseCodexEffectiveConfig(message.Result)
	}
	if ctx.Err() != nil {
		return codexEffectiveConfig{}, fmt.Errorf(
			"the Codex app-server effective-config read did not answer within %s",
			codexEffectiveConfigTimeout)
	}
	if err := scanner.Err(); err != nil {
		return codexEffectiveConfig{}, fmt.Errorf(
			"cannot read the Codex app-server effective-config reply: %w", err)
	}
	return codexEffectiveConfig{}, fmt.Errorf(
		"the Codex app-server produced no config/read result")
}

// codexConfigReadParams asks for the effective config as seen from the launch
// directory so project `.codex/` layers between cwd and the repo root are
// merged the same way the real launch will merge them.
func codexConfigReadParams(cwd string) map[string]any {
	params := map[string]any{"includeLayers": false}
	if strings.TrimSpace(cwd) != "" {
		params["cwd"] = cwd
	}
	return params
}

func parseCodexEffectiveConfig(
	raw json.RawMessage,
) (codexEffectiveConfig, error) {
	var response struct {
		Config struct {
			ModelProvider  *string `json:"model_provider"`
			OpenAIBaseURL  *string `json:"openai_base_url"`
			ChatGPTBaseURL *string `json:"chatgpt_base_url"`
			AuthStore      *string `json:"cli_auth_credentials_store"`
			ModelProviders map[string]struct {
				BaseURL            *string `json:"base_url"`
				RequiresOpenAIAuth *bool   `json:"requires_openai_auth"`
			} `json:"model_providers"`
		} `json:"config"`
		Origins map[string]struct {
			Name struct {
				Type string `json:"type"`
				Name string `json:"name"`
				File string `json:"file"`
			} `json:"name"`
			Version string `json:"version"`
		} `json:"origins"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return codexEffectiveConfig{}, fmt.Errorf(
			"cannot read the Codex effective config: %w", err)
	}

	value := func(in *string) string {
		if in == nil {
			return ""
		}
		return strings.TrimSpace(*in)
	}
	effective := codexEffectiveConfig{
		ModelProvider:  value(response.Config.ModelProvider),
		OpenAIBaseURL:  value(response.Config.OpenAIBaseURL),
		ChatGPTBaseURL: value(response.Config.ChatGPTBaseURL),
		AuthStore:      value(response.Config.AuthStore),
	}
	if len(response.Config.ModelProviders) > 0 {
		effective.ModelProviders = make(
			map[string]codexEffectiveProvider, len(response.Config.ModelProviders))
		for name, provider := range response.Config.ModelProviders {
			resolved := codexEffectiveProvider{BaseURL: value(provider.BaseURL)}
			if provider.RequiresOpenAIAuth != nil {
				resolved.RequiresOpenAIAuth = *provider.RequiresOpenAIAuth
			}
			effective.ModelProviders[name] = resolved
		}
	}

	for key, origin := range response.Origins {
		if !codexProviderRoutingKey(key) {
			continue
		}
		if origin.Name.Type != codexConfigLayerEnterprise &&
			origin.Name.Type != codexConfigLayerMDM {
			continue
		}
		effective.RemoteOrigins = append(
			effective.RemoteOrigins, codexRemoteConfigOrigin{
				Key:     key,
				Layer:   origin.Name.Type,
				Name:    origin.Name.Name,
				Version: origin.Version,
			})
	}
	sort.Slice(effective.RemoteOrigins, func(i, j int) bool {
		return effective.RemoteOrigins[i].Key < effective.RemoteOrigins[j].Key
	})
	return effective, nil
}

// codexRemoteProvenance renders the remotely delivered routing origins for the
// launch disclosure. Nothing is rendered when the whole route came from files.
func codexRemoteProvenance(
	origins []codexRemoteConfigOrigin,
) []string {
	if len(origins) == 0 {
		return nil
	}
	rendered := make([]string, 0, len(origins))
	for _, origin := range origins {
		rendered = append(rendered, origin.String())
	}
	return rendered
}

// codexProviderRoutingKey matches both a bare routing key and the dotted paths
// Codex reports for table members such as model_providers.<name>.base_url.
func codexProviderRoutingKey(key string) bool {
	for _, routing := range codexProviderRoutingKeys {
		if key == routing || strings.HasPrefix(key, routing+".") {
			return true
		}
	}
	return false
}

func codexEffectiveConfigEnv(
	overrides []sandboxpolicy.EnvironmentEntry,
) []string {
	environment := launchModelEnvironment(overrides)
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+environment[name])
	}
	return env
}
