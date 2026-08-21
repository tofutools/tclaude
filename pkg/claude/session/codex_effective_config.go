package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	// Layer source discriminators from ConfigLayerSource. Only the two remote
	// channels are named here; the rest are ordinary local files.
	codexConfigLayerEnterprise = "enterpriseManaged"
	codexConfigLayerMDM        = "mdm"
)

// codexEffectiveConfigTimeout bounds the probe. It is a var only so a test can
// shrink it: the timeout branch is the one failure path that cannot be reached
// by making Codex fail fast, and waiting out the production value to cover it
// would put 45s into every run.
var codexEffectiveConfigTimeout = 45 * time.Second

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

// codexEffectiveConfig is the small projection of one `config/read` used by
// tclaude: provider routing for filtered-launch enforcement, plus the service
// tier used as a best-effort baseline by the live Fast-mode control. Everything
// else Codex reports is deliberately dropped.
type codexEffectiveConfig struct {
	ModelProvider  string
	OpenAIBaseURL  string
	ChatGPTBaseURL string
	AuthStore      string
	ServiceTier    string
	ModelProviders map[string]codexEffectiveProvider
	// RemoteOrigins names, per routing key, the remotely delivered layer that
	// won it. Empty when every routing key came from a local file.
	RemoteOrigins []codexRemoteConfigOrigin
}

// CodexEffectiveFastMode asks Codex's own config loader for the inherited
// service tier that a flagless launch in cwd would receive. This is a
// best-effort baseline for live controls: a running thread may have been
// launched before the config changed, and its thread_settings_applied
// telemetry remains authoritative whenever it exists.
func CodexEffectiveFastMode(
	cwd string,
	environment []sandboxpolicy.EnvironmentEntry,
	permissionProfile string,
) (fast, known bool, err error) {
	config, err := codexEffectiveConfigReader(cwd, environment, permissionProfile)
	if err != nil {
		return false, false, err
	}
	return strings.EqualFold(config.ServiceTier, "priority") ||
		strings.EqualFold(config.ServiceTier, "fast"), true, nil
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

// String names the layer well enough for an operator to go and change it. The
// enterprise bundle carries an admin-facing name; the MDM layer instead carries
// a preferences domain and key, so falling back to the bare layer type would
// tell the operator only what they already read in the layer name.
func (o codexRemoteConfigOrigin) String() string {
	rendered := o.Key + " from " + o.Layer + " layer"
	if name := strings.TrimSpace(o.Name); name != "" {
		rendered += fmt.Sprintf(" %q", name)
	}
	if version := strings.TrimSpace(o.Version); version != "" {
		rendered += " (" + version + ")"
	}
	return rendered
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
	read func(cwd string, environment []sandboxpolicy.EnvironmentEntry, permissionProfile string) (json.RawMessage, error),
) func() {
	previous := codexEffectiveConfigReader
	codexEffectiveConfigReader = func(
		cwd string,
		environment []sandboxpolicy.EnvironmentEntry,
		permissionProfile string,
	) (codexEffectiveConfig, error) {
		raw, err := read(cwd, environment, permissionProfile)
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
	permissionProfile string,
) (codexEffectiveConfig, error) {
	raw, err := readCodexEffectiveConfigJSON(cwd, environment, permissionProfile)
	if err != nil {
		return codexEffectiveConfig{}, err
	}
	return parseCodexEffectiveConfig(raw)
}

// readCodexEffectiveConfigJSON returns the raw `config/read` result so callers
// and tests can inspect the response shape itself, not only its projection.
func readCodexEffectiveConfigJSON(
	cwd string,
	environment []sandboxpolicy.EnvironmentEntry,
	permissionProfile string,
) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(
		context.Background(), codexEffectiveConfigTimeout)
	defer cancel()

	// exec.Command resolves the binary against the PARENT process PATH at
	// construction time, so assigning command.Env afterwards would not change
	// which binary runs. The launch profile can set its own PATH, and reading
	// a different Codex build than the one that launches would mean reading a
	// different layer precedence, so resolve against the launch environment.
	launchEnvironment := launchModelEnvironment(environment)
	executable, lookErr := codexEffectiveConfigLookPath(launchEnvironment["PATH"])
	if lookErr != nil {
		return nil, fmt.Errorf(
			"cannot locate the Codex binary to read its effective config: %w",
			lookErr)
	}

	// --disable only sets features.<name>, never a provider-routing key, so the
	// probe still reads the routing this launch will get. Plugin/marketplace
	// sync is unrelated to model transport and would otherwise make the probe
	// wait on catalog fetches.
	arguments := []string{
		"--disable", "plugins",
		"--disable", "remote_plugin",
		"--disable", "plugin_sharing",
	}
	if strings.TrimSpace(permissionProfile) != "" {
		arguments = append(arguments, "-p", strings.TrimSpace(permissionProfile))
	}
	arguments = append(arguments, "app-server", "--listen", "stdio://")

	command := exec.CommandContext(ctx, executable, arguments...)
	command.Dir = cwd
	command.Env = sortedEnvironment(launchEnvironment)
	// Keep the tail of stderr: without it every startup failure — a renamed
	// subcommand on a newer Codex, a broken install — collapses into the same
	// "produced no result" message with nothing to diagnose it from.
	var diagnostics boundedBuffer
	command.Stderr = &diagnostics

	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf(
			"cannot drive the Codex app-server effective-config read: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf(
			"cannot drive the Codex app-server effective-config read: %w", err)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf(
			"cannot run the Codex app-server effective-config read: %w", err)
	}
	waited := false
	waitErr := error(nil)
	defer func() {
		_ = stdin.Close()
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		if !waited {
			_ = command.Wait()
		}
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
			return nil, marshalErr
		}
		if _, writeErr := stdin.Write(append(encoded, '\n')); writeErr != nil {
			return nil, fmt.Errorf(
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
		// A JSON-RPC error for a malformed or unsupported request can carry a
		// null id, so an id-matched-only check would drop the one message that
		// explains the failure and fall through to the generic no-result path.
		if message.Error != nil && (message.ID == nil || *message.ID == 2) {
			return nil, fmt.Errorf(
				"the Codex app-server refused config/read: %s",
				message.Error.Message)
		}
		if message.ID == nil || *message.ID != 2 {
			continue
		}
		return message.Result, nil
	}
	if ctx.Err() != nil {
		// The stderr tail matters most on exactly this path. A Codex that is
		// blocked from its own state root — an LSM denial, a read-only
		// CODEX_HOME — can print the reason and then hang instead of exiting,
		// and dropping the tail here left the operator a bare "did not answer"
		// with nothing to act on.
		return nil, fmt.Errorf(
			"the Codex app-server effective-config read did not answer within %s%s",
			codexEffectiveConfigTimeout, diagnostics.suffix())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf(
			"cannot read the Codex app-server effective-config reply: %w%s",
			err, diagnostics.suffix())
	}
	_ = stdin.Close()
	waitErr = command.Wait()
	waited = true
	return nil, fmt.Errorf(
		"the Codex app-server produced no config/read result (%s)%s",
		codexEffectiveConfigExit(waitErr), diagnostics.suffix())
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
			ServiceTier    *string `json:"service_tier"`
			ModelProviders map[string]struct {
				BaseURL            *string `json:"base_url"`
				RequiresOpenAIAuth *bool   `json:"requires_openai_auth"`
			} `json:"model_providers"`
		} `json:"config"`
		Origins map[string]struct {
			Name struct {
				Type   string `json:"type"`
				Name   string `json:"name"`
				File   string `json:"file"`
				Domain string `json:"domain"`
				Key    string `json:"key"`
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
		ServiceTier:    value(response.Config.ServiceTier),
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
		name := strings.TrimSpace(origin.Name.Name)
		if name == "" && strings.TrimSpace(origin.Name.Domain) != "" {
			name = strings.TrimSpace(origin.Name.Domain)
			if key := strings.TrimSpace(origin.Name.Key); key != "" {
				name += "/" + key
			}
		}
		effective.RemoteOrigins = append(
			effective.RemoteOrigins, codexRemoteConfigOrigin{
				Key:     key,
				Layer:   origin.Name.Type,
				Name:    name,
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

func sortedEnvironment(environment map[string]string) []string {
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

// codexEffectiveConfigLookPath resolves the Codex binary against the launch
// PATH rather than the parent process's. It walks the entries itself instead of
// swapping os.Environ around exec.LookPath, because a launch preflight can run
// concurrently with others and must not mutate process-global state.
//
// It mirrors exec.LookPath's rule that a candidate this process cannot execute
// is not a match: mode bits alone are not executability. A file can carry an
// execute bit for an owner this process is not, sit on a noexec mount, or be
// refused by an LSM, and testing the bits instead of the access would both stop
// the walk at that candidate — never reaching a working codex later in PATH —
// and turn what should be "no usable codex here" into an EACCES from execve,
// reported as if Codex itself had failed.
var codexEffectiveConfigLookPath = func(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return exec.LookPath("codex")
	}
	var unexecutable []string
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, "codex")
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if err := codexExecutableAccess(candidate); err != nil {
			unexecutable = append(unexecutable, candidate)
			continue
		}
		return candidate, nil
	}
	// Naming the rejected candidates is the whole point of the distinction. An
	// operator whose only codex is one this process may not execute is looking
	// at a permission problem, and a bare "not found" would send them to
	// install a binary they already have.
	if len(unexecutable) > 0 {
		return "", fmt.Errorf(
			"%q is present in the launch PATH at %s but not executable by this process; grant execute permission there, or put an executable %q earlier in PATH",
			"codex", strings.Join(unexecutable, ", "), "codex")
	}
	return "", fmt.Errorf(
		"%q not found in the launch PATH", "codex")
}

// boundedBuffer keeps only the tail of a stream. Codex can be chatty on stderr
// and none of it belongs in a refusal message beyond the part that explains
// the failure.
//
// The mutex is load-bearing rather than defensive. Assigning a non-*os.File
// Stderr makes os/exec pipe the stream and copy it on its own goroutine, which
// is joined by Wait() and by nothing else. Every failure path that reports the
// tail without having called Wait() first — the timeout and the scanner-error
// branches — therefore reads it while that goroutine may still be writing.
type boundedBuffer struct {
	mu   sync.Mutex
	data []byte
}

const codexEffectiveConfigDiagnosticsLimit = 2048

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > codexEffectiveConfigDiagnosticsLimit {
		b.data = b.data[len(b.data)-codexEffectiveConfigDiagnosticsLimit:]
	}
	return len(p), nil
}

func (b *boundedBuffer) suffix() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := strings.TrimSpace(string(b.data))
	if text == "" {
		return ""
	}
	return "; Codex reported: " + text
}

func codexEffectiveConfigExit(err error) string {
	if err == nil {
		return "the process exited without answering"
	}
	return err.Error()
}
