package session

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

var httpProxyTestCodexConfig = flag.String("c", "", "fake Codex config override")

type httpProxyTestTransport func(*http.Request) (*http.Response, error)

func (f httpProxyTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestHTTPProxyLaunchChild is the workload spawned by the bootstrap test. It
// uses the injected URL with a normal net/http client, without any tclaude API.
func TestHTTPProxyLaunchChild(t *testing.T) {
	if os.Getenv("TCLAUDE_GATEWAY_TEST_CHILD") != "1" {
		return
	}
	if os.Getenv(HTTPProxyEnvPrefix+"billing") != "" {
		fmt.Fprintln(os.Stderr, "inherited denied proxy survived")
		os.Exit(1)
	}
	base := os.Getenv(HTTPProxyEnvPrefix + "inventory")
	if os.Getenv("TCLAUDE_GATEWAY_TEST_CODEX_PIN") == "1" {
		key, value, ok := strings.Cut(*httpProxyTestCodexConfig, "=")
		var pinned string
		if !ok || key != harness.CodexShellEnvironmentOverridePrefix+HTTPProxyEnvPrefix+"inventory" || json.Unmarshal([]byte(value), &pinned) != nil || pinned != base {
			os.Exit(1)
		}
		// Simulate the SDK's restrictive snapshot dropping the ambient variable.
		_ = os.Unsetenv(HTTPProxyEnvPrefix + "inventory")
		_ = os.Setenv(HTTPProxyEnvPrefix+"inventory", pinned)
		base = os.Getenv(HTTPProxyEnvPrefix + "inventory")
	}
	if base == "" {
		fmt.Fprintln(os.Stderr, "missing injected URL")
		os.Exit(1)
	}
	resp, err := http.Post(base+"items?key=one", "application/octet-stream", bytes.NewReader([]byte{0, 255, 10}))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 422 || !bytes.Equal(data, []byte{0, 255, 10}) {
		os.Exit(1)
	}
}

func TestHTTPProxyBootstrapInjectsWorkingURLs(t *testing.T) {
	t.Setenv("TCLAUDE_GATEWAY_TEST_CHILD", "1")
	t.Setenv(HTTPProxyEnvPrefix+"billing", "http://stale.invalid/")
	calls := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "launch-row", r.Header.Get(agentipc.SessionClaimHeader))
		if r.URL.Path == "/v1/http/environment" {
			_, _ = io.WriteString(w, `{"names":["inventory"]}`)
			return
		}
		calls++
		assert.Equal(t, "/v1/http/proxy/inventory/items", r.URL.Path)
		assert.Equal(t, "key=one", r.URL.RawQuery)
		assert.Equal(t, "POST", r.Method)
		data, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		w.WriteHeader(422)
		_, _ = w.Write(data)
	}))
	defer daemon.Close()
	target, err := url.Parse(daemon.URL)
	require.NoError(t, err)
	previous := newHTTPProxyDaemonClient
	t.Cleanup(func() { newHTTPProxyDaemonClient = previous })
	newHTTPProxyDaemonClient = func() *http.Client {
		return &http.Client{Transport: httpProxyTestTransport(func(r *http.Request) (*http.Response, error) {
			clone := r.Clone(r.Context())
			copyURL := *r.URL
			clone.URL = &copyURL
			clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
			return daemon.Client().Transport.RoundTrip(clone)
		})}
	}
	code, err := runHTTPProxyExec("launch-row", clcommon.ShellQuoteArg(os.Args[0])+" -test.run '^TestHTTPProxyLaunchChild$'")
	require.NoError(t, err)
	assert.Zero(t, code)
	assert.Equal(t, 1, calls)
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{HTTPProxies: map[string]config.HTTPProxyConfig{"inventory": {URL: "https://inventory.example", Header: "Authorization"}}}}))
	t.Setenv("TCLAUDE_GATEWAY_TEST_CODEX_PIN", "1")
	wrapped := HTTPProxySpawnCommand("launch-row", harness.MustGet(harness.CodexName), harness.SpawnSpec{ExecutablePath: os.Args[0], ExtraArgs: []string{"-test.run", "^TestHTTPProxyLaunchChild$"}})
	args := httpProxyWrappedArgs(t, wrapped)
	cmd := httpProxyExecCmd()
	require.NoError(t, cmd.ParseFlags(args[3:]))
	workload := httpProxyWrappedWorkload(t, wrapped)
	offsets, err := cmd.Flags().GetIntSlice("codex-env-marker-offset")
	require.NoError(t, err)
	require.Len(t, offsets, 1)
	code, err = runHTTPProxyExecWithOptions("launch-row", workload, false, offsets)
	require.NoError(t, err)
	assert.Zero(t, code)
	assert.Equal(t, 2, calls)
}

func TestHTTPProxyBridgeRejectsForeignCapabilitiesAndPreservesEscaping(t *testing.T) {
	client := &http.Client{Transport: httpProxyTestTransport(func(r *http.Request) (*http.Response, error) {
		assert.Equal(t, "/v1/http/proxy/inventory/foo%2fbar", r.URL.EscapedPath())
		assert.Equal(t, "launch-row", r.Header.Get(agentipc.SessionClaimHeader))
		assert.Empty(t, r.Header.Get("Referer"))
		assert.Empty(t, r.Header.Get(HTTPProxyRuntimeClaimHeader))
		assert.Empty(t, r.Header.Get("X-Tclaude-Human-Token"))
		return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("invalid path"))}, nil
	})}
	handler, entries, err := newHTTPProxyBridge(client, "launch-row", []string{"inventory"})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/unknown/items", nil))
	assert.Equal(t, 404, rec.Code)
	req := httptest.NewRequest("GET", entries[HTTPProxyEnvPrefix+"inventory"]+"foo%2fbar", nil)
	req.Header.Set("Referer", "http://localhost/secret/")
	req.Header.Set(agentipc.SessionClaimHeader, "foreign")
	req.Header.Set(HTTPProxyRuntimeClaimHeader, "foreign-runtime")
	req.Header.Set("X-Tclaude-Human-Token", "foreign-human")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, 400, rec.Code)
	// Discovery returns only names and does not follow HTTP redirects.
	_, _, err = httpProxyNames(context.Background(), &http.Client{Transport: httpProxyTestTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("denied"))}, nil
	})}, "launch-row")
	assert.Error(t, err)
}

func httpProxyWrappedArgs(t *testing.T, command string) []string {
	t.Helper()
	intro, _, ok := strings.Cut(command, "\n")
	require.True(t, ok)
	command = intro
	out, err := exec.Command(clcommon.BootstrapShellPath(), "-c", "set -- "+command+"; printf '%s\\0' \"$@\"").Output()
	require.NoError(t, err)
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

func TestHTTPProxyDiscoveryFailureStillStartsWorkload(t *testing.T) {
	previous := newHTTPProxyDaemonClient
	t.Cleanup(func() { newHTTPProxyDaemonClient = previous })
	newHTTPProxyDaemonClient = func() *http.Client {
		return &http.Client{Transport: httpProxyTestTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("denied"))}, nil
		})}
	}
	t.Setenv(HTTPProxyEnvPrefix+"inventory", "http://foreign/")
	code, err := runHTTPProxyExec("unknown-launch", "test -z \"${TCLAUDE_HTTP_PROXY_inventory+x}\"; exit 7")
	require.NoError(t, err)
	assert.Equal(t, 7, code)
}

func TestHTTPProxyCodexPinsBothExecutionOwnersAndPreservesPrompt(t *testing.T) {
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{HTTPProxies: map[string]config.HTTPProxyConfig{"inventory": {URL: "https://inventory.example", Header: "Authorization"}}}}))
	prompt := harness.CodexHTTPProxyEnvironmentMarker
	wrapped := HTTPProxySpawnCommand("launch", harness.MustGet(harness.CodexName), harness.SpawnSpec{
		InitialPrompt: prompt, CodexAppServerSocket: "/tmp/app.sock", CodexAppServerURL: "ws://127.0.0.1:34567", TclaudeExecutable: "/usr/bin/tclaude",
	})
	args := httpProxyWrappedArgs(t, wrapped)
	cmd := httpProxyExecCmd()
	require.NoError(t, cmd.ParseFlags(args[3:]))
	workload := httpProxyWrappedWorkload(t, wrapped)
	offsets, err := cmd.Flags().GetIntSlice("codex-env-marker-offset")
	require.NoError(t, err)
	require.Len(t, offsets, 2)
	marker := clcommon.ShellQuoteArg(harness.CodexHTTPProxyEnvironmentMarkerArg(map[string]string{}))
	for _, offset := range offsets {
		assert.True(t, strings.HasPrefix(workload[offset:], marker))
	}
	assert.Equal(t, 2, strings.Count(workload, marker))
	assert.Contains(t, workload, clcommon.ShellQuoteArg(prompt), "prompt text is not an argument slot")
}

func httpProxyWrappedWorkload(t *testing.T, wrapped string) string {
	t.Helper()
	intro, body, ok := strings.Cut(wrapped, "\n")
	require.True(t, ok)
	_, delim, ok := strings.Cut(intro, "99<<")
	require.True(t, ok)
	delim = strings.Trim(delim, "'")
	workload, _, ok := strings.Cut(body, "\n"+delim+"\n")
	require.True(t, ok)
	return workload
}

func TestHTTPProxyPrivateWorkloadHandoffCannotBeClosedByPrompt(t *testing.T) {
	command := "printf '%s\\n' 'secret-value'\nTCLAUDE_HTTP_PROXY_COMMAND_EOF\nTCLAUDE_HTTP_PROXY_COMMAND_EOF_"
	wrapped := renderHTTPProxyCommand("launch", command, false, nil)
	args := httpProxyWrappedArgs(t, wrapped)
	assert.NotContains(t, strings.Join(args, " "), "secret-value")
	assert.Contains(t, args, "--command-fd")
	assert.Equal(t, command, httpProxyWrappedWorkload(t, wrapped))
}

// The test executable dispatches the hidden wrapper in a subprocess so the
// real shell redirection and descriptor handoff are exercised end to end.
func init() {
	if os.Getenv("TCLAUDE_GATEWAY_TEST_DISPATCH") == "1" && len(os.Args) > 3 && os.Args[1] == "session" && os.Args[2] == "http-proxy-exec" {
		cmd := httpProxyExecCmd()
		cmd.SetArgs(os.Args[3:])
		if err := cmd.Execute(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func TestHTTPProxyPrivateHandoffShellExecution(t *testing.T) {
	t.Setenv("TCLAUDE_GATEWAY_TEST_DISPATCH", "1")
	t.Setenv(agentipc.SocketEnv, "/nonexistent/tclaude-gateway-test.sock")
	command := renderHTTPProxyCommand("test-launch", "printf private-handoff; exit 7", false, nil)
	child := exec.Command(clcommon.BootstrapShellPath(), "-c", "if true; then "+command+"; else exit 8; fi")
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit, string(output))
	assert.Equal(t, 7, exit.ExitCode(), string(output))
	assert.Contains(t, string(output), "private-handoff")
}

func TestHTTPProxyCustomEnvironmentVariable(t *testing.T) {
	_, environment, err := newHTTPProxyBridge(&http.Client{}, "launch", []string{"inventory", "billing"}, map[string]string{"inventory": "INVENTORY_API_URL"})
	require.NoError(t, err)
	assert.Contains(t, environment, "INVENTORY_API_URL")
	assert.Contains(t, environment, HTTPProxyEnvPrefix+"billing")
	assert.NotContains(t, environment, HTTPProxyEnvPrefix+"inventory")
	_, _, err = newHTTPProxyBridge(&http.Client{}, "launch", []string{"inventory", "billing"}, map[string]string{"inventory": "API_URL", "billing": "API_URL"})
	require.Error(t, err)
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{HTTPProxies: map[string]config.HTTPProxyConfig{"inventory": {EnvironmentVariable: "INVENTORY_API_URL"}}}}))
	assert.True(t, httpProxyReservedEnvironment("INVENTORY_API_URL"))
	assert.False(t, httpProxyReservedEnvironment("OTHER_URL"))
}

func TestHTTPProxyCLISelectionInFinalNamespace(t *testing.T) {
	assert.Equal(t, clcommon.SelfTclaudePath(), httpProxyCLIForPlan(sandboxpolicy.MountPlan{RootPosture: sandboxpolicy.RootHostInherited}))
	guest := httpProxyCLIForPlan(sandboxpolicy.MountPlan{RootPosture: sandboxpolicy.RootConstructed})
	assert.Equal(t, "/.tclaude/bin/tclaude", guest)
	command := renderHTTPProxyCommand("test-session", "exit 0", false, nil, guest)
	args := httpProxyWrappedArgs(t, command)
	assert.Equal(t, guest, args[0])
}

func TestHTTPProxyCodexPrivateHandoffPreservesShellExpansions(t *testing.T) {
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{HTTPProxies: map[string]config.HTTPProxyConfig{"inventory": {URL: "https://inventory.example", Header: "Authorization"}}}}))
	t.Setenv("TCLAUDE_GATEWAY_TEST_DISPATCH", "1")
	t.Setenv(agentipc.SocketEnv, "/nonexistent/tclaude-gateway-test.sock")
	sentinel := testutil.CanonicalTempDir(t) + "/unexpected-expansion"
	literal := "$UNDEFINED_GATEWAY_TEST_VAR $(touch " + clcommon.ShellQuoteArg(sentinel) + ") `touch " + clcommon.ShellQuoteArg(sentinel) + "`"
	spec := harness.SpawnSpec{
		ExecutablePath: clcommon.BootstrapShellPath(),
		EnvExports:     "export GATEWAY_LITERAL=" + clcommon.ShellQuoteArg(literal) + "; ",
		ExtraArgs:      []string{"-c", "printf '%s' \"$GATEWAY_LITERAL\"; exit 7"},
		InitialPrompt:  literal,
	}
	wrapped := HTTPProxySpawnCommand("test-launch", harness.MustGet(harness.CodexName), spec)
	child := exec.Command(clcommon.BootstrapShellPath(), "-c", wrapped)
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit, string(output))
	assert.Equal(t, 7, exit.ExitCode(), string(output))
	assert.Contains(t, string(output), literal)
	assert.NotContains(t, string(output), "invalid Codex gateway environment marker")
	assert.NoFileExists(t, sentinel, "handoff must not execute prompt or environment substitutions")
}

func TestHTTPProxySpawnCommandPinsManagedHarness(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".local", "share", "tclaude", "harnesses", "npm", "bin")
	require.NoError(t, os.MkdirAll(dir, 0700))
	path := filepath.Join(dir, "claude")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"), 0700))
	t.Setenv("PATH", dir)
	h := harness.MustGet(harness.DefaultName)
	command := HTTPProxySpawnCommand("managed-path-test", h, harness.SpawnSpec{})
	require.Contains(t, command, clcommon.ShellQuoteArg(path))
	command = HTTPProxySpawnCommand("managed-path-test", h, harness.SpawnSpec{ExecutablePath: "/explicit/claude"})
	require.Contains(t, command, "/explicit/claude")
	require.NotContains(t, command, path)
	command = HTTPProxySpawnCommand("managed-path-test", h, harness.SpawnSpec{PreLaunchScript: "export PATH=/custom/bin; "})
	require.NotContains(t, command, path)
}
