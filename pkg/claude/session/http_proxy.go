package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const HTTPProxyEnvPrefix = "TCLAUDE_HTTP_PROXY_"

// WrapHTTPProxyCommand puts the bridge inside the workload's namespace, so
// ordinary clients can use its loopback URLs even with isolated networking.
// This runs after the launch gate has bound the pane's generation.
func WrapHTTPProxyCommand(sessionID, command string) string {
	return wrapHTTPProxyCommand(sessionID, command, false)
}

// WrapHTTPProxyRuntimeCommand wraps a managed tool-executing server rather
// than its attach-only pane. Its daemon-recorded process root is the authority.
func WrapHTTPProxyRuntimeCommand(sessionID, command string, cliPath ...string) string {
	return wrapHTTPProxyCommand(sessionID, command, true, cliPath...)
}

func wrapHTTPProxyCommand(sessionID, command string, runtime bool, cliPath ...string) string {
	cfg, err := config.Load()
	if err != nil || !cfg.HTTPProxyConfigured() {
		return command
	}
	return renderHTTPProxyCommand(sessionID, command, runtime, nil, cliPath...)
}

func renderHTTPProxyCommand(sessionID, command string, runtime bool, markerOffsets []int, cliPath ...string) string {
	executable := clcommon.SelfTclaudePath()
	if len(cliPath) > 0 && cliPath[0] != "" {
		executable = cliPath[0]
	}
	runtimeArg := ""
	if runtime {
		runtimeArg = " --runtime"
	}
	for _, offset := range markerOffsets {
		runtimeArg += fmt.Sprintf(" --codex-env-marker-offset %d", offset)
	}
	// A quoted here-document carries the opaque workload on a private fd.
	// Keeping it out of argv preserves the launch script's credential privacy.
	// Quote the delimiter unconditionally: argument quoting may leave a safe
	// word bare, which enables heredoc expansion and corrupts marker offsets.
	lines := map[string]bool{}
	for _, line := range strings.Split(command, "\n") {
		lines[line] = true
	}
	delimiter := "TCLAUDE_HTTP_PROXY_COMMAND_EOF"
	for lines[delimiter] {
		delimiter += "_"
	}
	return clcommon.ShellQuoteArg(executable) + " session http-proxy-exec" + runtimeArg + " --session-id " + clcommon.ShellQuoteArg(sessionID) + " --command-fd 99 99<<'" + delimiter + "'\n" + command + "\n" + delimiter + "\ntclaude_http_proxy_exit=$?; (exit \"$tclaude_http_proxy_exit\")"
}

func httpProxyExecCmd() *cobra.Command {
	var sessionID, command, modelProxy, modelHarness, modelProfile string
	var runtime bool
	var commandFD int
	var markerOffsets []int
	cmd := &cobra.Command{Use: "http-proxy-exec", Hidden: true, Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if commandFD >= 0 {
				file := os.NewFile(uintptr(commandFD), "http-proxy-workload")
				data, readErr := io.ReadAll(io.LimitReader(file, 1024*1024+1))
				_ = file.Close()
				if readErr != nil || len(data) > 1024*1024 {
					return errors.New("HTTP proxy workload handoff unreadable or exceeds 1 MiB")
				}
				command = strings.TrimSuffix(string(data), "\n")
			}
			code, err := runHTTPProxyExecWithOptions(sessionID, command, runtime, markerOffsets, modelProxy, modelHarness, modelProfile)
			if err != nil {
				return err
			}
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&commandFD, "command-fd", -1, "private workload descriptor")
	cmd.Flags().IntSliceVar(&markerOffsets, "codex-env-marker-offset", nil, "position of the compile-time Codex environment marker")
	cmd.Flags().BoolVar(&runtime, "runtime", false, "managed server process boundary")
	cmd.Flags().StringVar(&modelProfile, "model-proxy-profile", "", "Codex launch permission profile")
	cmd.Flags().StringVar(&modelHarness, "model-proxy-harness", "claude", "model gateway harness")
	cmd.Flags().StringVar(&modelProxy, "model-proxy", "", "named model gateway reference")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "generation-bound launch row")
	cmd.Flags().StringVar(&command, "command", "", "workload shell command")
	_ = cmd.MarkFlagRequired("session-id")
	// The renderer uses --command-fd; --command is retained for explicit
	// diagnostic invocations without changing the public command surface.
	return cmd
}

var newHTTPProxyDaemonClient = httpProxyDaemonClient

func httpProxyDaemonClient() *http.Client {
	return &http.Client{Timeout: 75 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var last error
			for _, path := range agentipc.ClientSocketPaths() {
				conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
				if err == nil {
					return conn, nil
				}
				last = err
			}
			if last == nil {
				last = errors.New("no daemon socket configured")
			}
			return nil, last
		},
	}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func httpProxyNames(ctx context.Context, client *http.Client, sessionID string) ([]string, map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://tclaude/v1/http/environment", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set(agentipc.SessionClaimHeader, sessionID)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("daemon refused HTTP proxy discovery (status %d)", resp.StatusCode)
	}
	var data struct {
		Names       []string          `json:"names"`
		Environment map[string]string `json:"environment_variables"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&data)
	return data.Names, data.Environment, err
}

// newHTTPProxyBridge binds each unguessable local URL to one instance. Requests
// cross the Unix socket with the bridge's kernel identity and a verified pane
// claim; neither a caller's name nor HTTP headers can select another agent.
func newHTTPProxyBridge(client *http.Client, sessionID string, names []string, aliases ...map[string]string) (http.Handler, map[string]string, error) {
	instances := map[string]string{}
	suffixes := map[string]string{}
	for _, name := range names {
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return nil, nil, errors.New("HTTP proxy name is not usable in an environment variable")
		}
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return nil, nil, err
		}
		capability := hex.EncodeToString(secret[:])
		instances[capability] = name
		variable := HTTPProxyEnvPrefix + name
		if len(aliases) > 0 && aliases[0][name] != "" {
			variable = aliases[0][name]
		}
		if strings.ContainsAny(variable, "=\x00") {
			return nil, nil, errors.New("invalid HTTP proxy environment variable")
		}
		if _, duplicate := suffixes[variable]; duplicate {
			return nil, nil, errors.New("duplicate HTTP proxy environment variable")
		}
		suffixes[variable] = "/" + capability + "/"
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use EscapedPath so encoded separators/traversal survive to the daemon's
		// confinement validator. ServeMux cleaning must never erase them first.
		cap, path, ok := strings.Cut(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
		name, found := instances[cap]
		if !ok || !found || r.URL.IsAbs() {
			http.Error(w, "unknown HTTP proxy endpoint", http.StatusNotFound)
			return
		}
		target := "http://tclaude/v1/http/proxy/" + url.PathEscape(name) + "/" + path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, http.MaxBytesReader(w, r.Body, 4*1024*1024))
		if err != nil {
			http.Error(w, "invalid HTTP request", 400)
			return
		}
		req.Header = r.Header.Clone()
		req.Header.Del(HTTPProxyRuntimeClaimHeader)
		req.Header.Del("X-Tclaude-Human-Token")
		req.Header.Del("X-Tclaude-Route-Helper-Credential")
		req.Header.Del(agentipc.AgentHintHeader)
		req.Header.Set(agentipc.SessionClaimHeader, sessionID)
		// Never disclose the local capability through a browser Referer.
		req.Header.Del("Referer")
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "HTTP gateway unavailable; upstream outcome may be unknown", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for name, values := range resp.Header {
			w.Header()[name] = values
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 4*1024*1024+1))
	})
	return handler, suffixes, nil
}

func runHTTPProxyExec(sessionID, command string, runtimeMode ...bool) (int, error) {
	runtime := len(runtimeMode) > 0 && runtimeMode[0]
	return runHTTPProxyExecWithOptions(sessionID, command, runtime, nil)
}

func runHTTPProxyExecWithOptions(sessionID, command string, runtime bool, markerOffsets []int, modelRefs ...string) (int, error) {
	client := newHTTPProxyDaemonClient()
	if runtime {
		client.Transport = &httpProxyRuntimeTransport{inner: client.Transport, sessionID: sessionID}
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	names, aliases, err := httpProxyNames(ctx, client, sessionID)
	// A managed server publishes its recorded root immediately after fork.
	// Retry this read-only bootstrap while that publication races the child.
	if runtime {
		for err != nil && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(25 * time.Millisecond):
			}
			if ctx.Err() == nil {
				names, aliases, err = httpProxyNames(ctx, client, sessionID)
			}
		}
	}
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Warning: HTTP proxy URLs unavailable; continuing without them")
		names = nil
	}
	handler, entries, err := newHTTPProxyBridge(client, sessionID, names, aliases)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Warning: HTTP proxy URLs unavailable; continuing without them")
		handler = http.NotFoundHandler()
		entries = nil
	}
	modelRef := ""
	if len(modelRefs) > 0 {
		modelRef = modelRefs[0]
	}
	modelHarness := "claude"
	if len(modelRefs) > 1 && modelRefs[1] != "" {
		modelHarness = modelRefs[1]
	}
	if modelHarness != "claude" && modelHarness != "codex" {
		return 0, errors.New("unsupported model gateway harness")
	}
	var modelBridge *modelProxyBridge
	if modelRef != "" {
		cwd, e := os.Getwd()
		if e != nil {
			return 0, e
		}
		if modelHarness == "claude" {
			e = validateModelProxySettings(cwd, os.Environ())
		}
		if e != nil {
			return 0, e
		}
		modelBridge, err = newModelProxyBridge(client, sessionID, modelRef, modelHarness)
		if err != nil {
			return 0, fmt.Errorf("model gateway binding failed; launch refused: %w", err)
		}
		defer modelBridge.revoke()
		ordinary := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/model/") {
				modelBridge.ServeHTTP(w, r)
				return
			}
			ordinary.ServeHTTP(w, r)
		})
	}
	environ := []string{}
	for _, pair := range os.Environ() {
		name, value, _ := strings.Cut(pair, "=")
		if !httpProxyReservedEnvironment(name) && !config.IsHTTPProxyGatewayURL(value) && (modelBridge == nil || !modelProxyCompetingEnvironment(name)) {
			environ = append(environ, pair)
		}
	}
	gatewayEnvironment := map[string]string{}
	var server *http.Server
	var listener net.Listener
	if len(entries) > 0 || modelBridge != nil {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			if modelBridge != nil {
				return 0, errors.New("model gateway loopback listener unavailable; launch refused")
			}
			fmt.Fprintln(os.Stderr, "Warning: HTTP proxy loopback listener unavailable; continuing without URLs")
		}
	}
	if listener != nil {
		defer listener.Close()
		base := "http://" + listener.Addr().String()
		for name, path := range entries {
			environ = append(environ, name+"="+base+path)
			gatewayEnvironment[name] = base + path
		}
		if modelBridge != nil {
			if modelHarness == "codex" {
				environ = append(environ, "TCLAUDE_MODEL_PROXY_TOKEN="+modelBridge.bearer)
				profile := ""
				if len(modelRefs) > 2 {
					profile = modelRefs[2]
				}
				if err := validateCodexModelProxyEffective(environ, profile, modelBridge.provider, base+"/model/v1"); err != nil {
					return 0, err
				}
			} else {
				environ = append(environ, "ANTHROPIC_BASE_URL="+base+"/model", "ANTHROPIC_AUTH_TOKEN="+modelBridge.bearer, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
			}
		}
		server = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 65 * time.Second, WriteTimeout: 65 * time.Second, IdleTimeout: 30 * time.Second}
		if modelBridge != nil {
			server.ReadTimeout = 0
			server.WriteTimeout = 0
			server.MaxHeaderBytes = 32 << 10
		}
		defer func() { _ = server.Close() }()
		go func() { _ = server.Serve(listener) }()
	}
	for i := len(markerOffsets) - 1; i >= 0; i-- {
		offset := markerOffsets[i]
		prefix := harness.CodexHTTPProxyEnvironmentMarker + "_"
		if offset < 0 || offset > len(command) || !strings.HasPrefix(command[offset:], prefix) {
			return 0, errors.New("invalid Codex gateway environment marker")
		}
		fields := strings.Fields(command[offset+len(prefix):])
		if len(fields) == 0 {
			return 0, errors.New("invalid Codex gateway environment marker")
		}
		payload := fields[0]
		data, decodeErr := base64.StdEncoding.DecodeString(payload)
		var base map[string]string
		if decodeErr != nil || json.Unmarshal(data, &base) != nil {
			return 0, errors.New("invalid Codex gateway environment data")
		}
		args := harness.CodexHTTPProxyEnvironmentArgs(base, gatewayEnvironment)
		if modelBridge != nil && modelHarness == "codex" {
			args += harness.CodexModelProxyArgs(modelBridge.provider, "http://"+listener.Addr().String()+"/model/v1")
		}
		markerLength := len(prefix) + len(payload)
		command = command[:offset] + args + command[offset+markerLength:]
	}
	child := exec.Command(clcommon.BootstrapShellPath(), "-c", command)
	child.Env = environ
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Keep the terminal's foreground process group: the workload must retain
	// stdin and receive terminal resize/interrupt signals directly from tmux.
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	if err := child.Start(); err != nil {
		return 0, err
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	for {
		select {
		case sig := <-signals:
			// Terminal interrupts already reach the workload's shared process group.
			if runtime || sig != syscall.SIGINT {
				_ = child.Process.Signal(sig)
			}
		case err := <-done:
			if err == nil {
				return 0, nil
			}
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					return 128 + int(status.Signal()), nil
				}
				return exit.ExitCode(), nil
			}
			return 0, err
		}
	}
}

// LaunchResumedTmuxSession establishes a fresh pane generation before a
// resumed HTTP gateway bootstrap discovers its permissions. Without a gate,
// the resumed command could race its parent writing the new session row.
func LaunchResumedTmuxSession(sessionID, tmuxSession, cwd, command, harnessName string, markers ...string) error {
	cfg, err := config.Load()
	if err != nil || !cfg.HTTPProxyConfigured() {
		return LaunchDetachedTmuxSession(tmuxSession, cwd, command, markers...)
	}
	rows, err := db.FindSessionsByConvID(sessionID)
	if err != nil {
		return err
	}
	state := &SessionState{}
	if len(rows) > 0 {
		state = fromRow(rows[0])
	}
	state.StatusDetail, state.ExitReason = "", ""
	state.Subagents, state.BgShells, state.Monitors = nil, nil, nil
	state.ID, state.ConvID, state.TmuxSession, state.Cwd = sessionID, sessionID, tmuxSession, cwd
	state.PID, state.Status = 0, StatusIdle
	if state.Created.IsZero() {
		state.Created = time.Now()
	}
	state.Updated = time.Now()
	state.Harness = harnessName
	generation := newExitLaunchGeneration(sessionID, tmuxSession)
	guard, err := newExitLaunchGuard(sessionID, tmuxSession, generation)
	if err != nil {
		return err
	}
	defer guard.abort()
	if err := SaveSessionStateForLaunch(state, generation, db.SessionExitGatePending); err != nil {
		return err
	}
	if err := LaunchDetachedTmuxSession(tmuxSession, cwd, guard.wrap(command), markers...); err != nil {
		state.Status = StatusExited
		_ = SaveSessionState(state)
		return err
	}
	guard.armPaneHook()
	guard.bind()
	state.PID = ParsePIDFromTmux(tmuxSession)
	if err := SaveSessionState(state); err != nil {
		_ = clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(tmuxSession)).Run()
		return err
	}
	if err := guard.release(); err != nil {
		_ = clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(tmuxSession)).Run()
		return err
	}
	return nil
}

const HTTPProxyRuntimeClaimHeader = "X-Tclaude-HTTP-Runtime"

type httpProxyRuntimeTransport struct {
	inner     http.RoundTripper
	sessionID string
}

func (t *httpProxyRuntimeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Del(agentipc.SessionClaimHeader)
	clone.Header.Set(HTTPProxyRuntimeClaimHeader, t.sessionID)
	return t.inner.RoundTrip(clone)
}
func (t *httpProxyRuntimeTransport) CloseIdleConnections() {
	if closer, ok := t.inner.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// HTTPProxySpawnCommand builds the ordinary harness launch plus its optional
// gateway. Codex receives a compile-time argument marker at a known offset;
// only that marker is replaced at runtime, never user prompt or command text.
func HTTPProxySpawnCommand(sessionID string, h *harness.Harness, spec harness.SpawnSpec, cliPath ...string) string {
	filteredEnvironment := map[string]string{}
	for name, value := range spec.ShellEnvironment {
		if !httpProxyReservedEnvironment(name) && !config.IsHTTPProxyGatewayURL(value) && (spec.ModelProxy == "" || !modelProxyCompetingEnvironment(name)) {
			filteredEnvironment[name] = value
		}
	}
	spec.ShellEnvironment = filteredEnvironment
	if spec.ModelProxy != "" {
		prefix := spec.EnvExports + spec.PreLaunchScript
		spec.EnvExports, spec.PreLaunchScript = "", ""
		offsets := []int{}
		if h.Name == harness.CodexName {
			spec.RuntimeHTTPProxyEnvironment = true
		}
		command := h.Spawn.BuildCommand(spec)
		if h.Name == harness.CodexName {
			marker := clcommon.ShellQuoteArg(harness.CodexHTTPProxyEnvironmentMarkerArg(spec.ShellEnvironment))
			count := 1
			if spec.CodexAppServerSocket != "" {
				count = 2
			}
			start := 0
			for range count {
				at := strings.Index(command[start:], marker)
				if at < 0 {
					panic("Codex model gateway marker absent")
				}
				at += start
				offsets = append(offsets, at)
				start = at + len(marker)
			}
		}
		command = renderHTTPProxyCommand(sessionID, command, false, offsets, cliPath...)
		return prefix + strings.Replace(command, " session http-proxy-exec", " session http-proxy-exec --model-proxy "+clcommon.ShellQuoteArg(spec.ModelProxy)+" --model-proxy-harness "+clcommon.ShellQuoteArg(h.Name)+" --model-proxy-profile "+clcommon.ShellQuoteArg(spec.PermissionProfile), 1)
	}
	cfg, err := config.Load()
	if err != nil || !cfg.HTTPProxyConfigured() || h.UsesAuthoritativeServer() {
		return h.Spawn.BuildCommand(spec)
	}
	if h.Name != harness.CodexName {
		return renderHTTPProxyCommand(sessionID, h.Spawn.BuildCommand(spec), false, nil, cliPath...)
	}
	original := spec
	spec.RuntimeHTTPProxyEnvironment = true
	command := h.Spawn.BuildCommand(spec)
	prefix := spec.EnvExports + spec.PreLaunchScript
	marker := clcommon.ShellQuoteArg(harness.CodexHTTPProxyEnvironmentMarkerArg(spec.ShellEnvironment))
	if !strings.HasPrefix(command, prefix) {
		return renderHTTPProxyCommand(sessionID, h.Spawn.BuildCommand(original), false, nil)
	}
	count := 1
	if spec.CodexAppServerSocket != "" {
		count = 2
	}
	offsets := make([]int, 0, count)
	start := len(prefix)
	for range count {
		relative := strings.Index(command[start:], marker)
		if relative < 0 {
			return renderHTTPProxyCommand(sessionID, h.Spawn.BuildCommand(original), false, nil)
		}
		offset := start + relative
		offsets = append(offsets, offset)
		start = offset + len(marker)
	}
	return renderHTTPProxyCommand(sessionID, command, false, offsets, cliPath...)
}

func httpProxyReservedEnvironment(name string) bool {
	if strings.HasPrefix(name, HTTPProxyEnvPrefix) {
		return true
	}
	cfg, err := config.Load()
	if err == nil && cfg.Agent != nil {
		for _, instance := range cfg.Agent.HTTPProxies {
			if instance.EnvironmentVariable != "" && instance.EnvironmentVariable == name {
				return true
			}
		}
	}
	return false
}

// HTTPProxyCLIForLayerSpec selects the CLI from the same mount plan the
// sandbox renderer uses, rather than guessing from files visible at launch.
func HTTPProxyCLIForLayerSpec(spec *TclaudeLayerLaunchSpec) (string, error) {
	cfg, loadErr := config.Load()
	if spec == nil || runtime.GOOS != "linux" || loadErr != nil || !cfg.HTTPProxyConfigured() {
		return clcommon.SelfTclaudePath(), nil
	}
	_, _, _, _, _, plan, err := tclaudeLayerSpecRenderInput(*spec)
	if err != nil {
		return "", err
	}
	return httpProxyCLIForPlan(plan), nil
}

func httpProxyCLIForPlan(plan sandboxpolicy.MountPlan) string {
	if tclaudeLayerPlanUsesConstructedRoot(plan) {
		return tclaudeLayerConstructedRootTclaudePath
	}
	return clcommon.SelfTclaudePath()
}
