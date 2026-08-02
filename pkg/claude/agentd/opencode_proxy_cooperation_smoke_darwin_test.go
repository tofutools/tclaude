//go:build darwin

package agentd

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

const (
	openCodeDarwinProxySmokeEnv      = "TCLAUDE_OPENCODE_PROXY_DARWIN_SMOKE"
	openCodeDarwinProxyPinnedVersion = "1.18.6"
	openCodeDarwinProxyOrigin        = "api.openai.com"
	openCodeDarwinProxyDenied        = "undeclared.opencode-proxy.tclaude.test"
	openCodeDarwinProxyModel         = "test/test-model"
	openCodeDarwinProxySessionID     = "opencode-darwin-proxy-smoke"
	openCodeDarwinProxyProbeEnv      = "TCLAUDE_OPENCODE_DARWIN_PROXY_PROBE"
	openCodeDarwinProxyMarkerEnv     = "TCLAUDE_OPENCODE_DARWIN_PROXY_MARKER"
)

func TestOpenCodeServeExecUsesDarwinProxyServerBoundary(t *testing.T) {
	previousResolve := resolveOpenCodeTclaudeLayer
	previousWrap := wrapOpenCodeTclaudeLayer
	previousBindWrap := wrapOpenCodeTclaudeLayerWithLoopbackBind
	t.Cleanup(func() {
		resolveOpenCodeTclaudeLayer = previousResolve
		wrapOpenCodeTclaudeLayer = previousWrap
		wrapOpenCodeTclaudeLayerWithLoopbackBind = previousBindWrap
	})
	resolveOpenCodeTclaudeLayer = func(
		posture sandboxpolicy.NetworkPosture,
		_ sandboxpolicy.RootPosture,
		engine sandboxpolicy.NetworkEngine,
	) (string, harness.LaunchOSSandbox, error) {
		assert.Equal(t, sandboxpolicy.NetworkFiltered, posture)
		assert.Equal(t, sandboxpolicy.NetworkEngineProxy, engine)
		return "/usr/bin/sandbox-exec", harness.LaunchOSSandbox{}, nil
	}
	wrapOpenCodeTclaudeLayer = func(
		string, session.TclaudeLayerLaunchSpec, string,
	) (string, error) {
		t.Fatal("Darwin's filtered proxy path must use the loopback-bind renderer")
		return "", nil
	}
	capturedPort := 0
	wrapOpenCodeTclaudeLayerWithLoopbackBind = func(
		binary string,
		_ session.TclaudeLayerLaunchSpec,
		port int,
		command string,
	) (string, error) {
		assert.Equal(t, "/usr/bin/sandbox-exec", binary)
		assert.Contains(t, command, "--hostname 127.0.0.1")
		capturedPort = port
		return "wrapped-opencode-server", nil
	}
	spec := openCodeDarwinProxyServeTestSpec()

	command, args, err := openCodeServeExec("/usr/bin/opencode", "43210", spec)
	require.NoError(t, err)
	assert.Equal(t, 43210, capturedPort)
	assert.Equal(t, "sh", command)
	assert.Equal(t, []string{"-c", "exec wrapped-opencode-server"}, args)

	for _, port := range []string{"not-a-port", "0", "65536"} {
		_, _, err = openCodeServeExec("/usr/bin/opencode", port, spec)
		require.ErrorContains(t, err, "parse OpenCode loopback control port")
	}
}

func openCodeDarwinProxyServeTestSpec() *session.TclaudeLayerLaunchSpec {
	return &session.TclaudeLayerLaunchSpec{
		Version: session.TclaudeLayerLaunchSpecVersion,
		Effective: sandboxpolicy.EffectiveProfile{
			Network: &sandboxpolicy.NetworkRules{
				Mode:   sandboxpolicy.AccessModeList,
				Engine: sandboxpolicy.NetworkEngineProxy,
				Allow: []sandboxpolicy.NetworkAllowEntry{{
					Domain: openCodeDarwinProxyOrigin, Ports: []int{443},
				}},
			},
		},
	}
}

// TestOpenCodeProxyCooperationDarwin is OpenCode's TCL-827 §8.2 test-7 arm.
// It launches the pinned, agentd-owned OpenCode server through the shipped
// Darwin filtering-proxy launcher and requires the production decision record
// to name the model origin over HTTP CONNECT. The separate undeclared probe
// proves the floor is enforcing, but its distinct host cannot satisfy the
// model-origin assertion. Credentials are deliberately invalid and permanent
// CI evidence never uses a real provider credential.
func TestOpenCodeProxyCooperationDarwin(t *testing.T) {
	fixture := prepareOpenCodeDarwinProxySmoke(t)
	launch, err := startOpenCodeRuntime(
		openCodeDarwinProxySessionID, fixture.cwd, "OpenCode Darwin proxy smoke",
		"", fixture.permissionJSON, string(sandboxpolicy.ImplementationTclaudeLayer),
		fixture.spec, "")
	if err != nil {
		logOpenCodeDarwinLayerSmokeServerLogs(t, fixture.serverLogDir)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = stopOpenCodeRuntime(openCodeDarwinProxySessionID) })

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(fixture.probeMarker)
		return statErr == nil
	}, 30*time.Second, 100*time.Millisecond,
		"the in-floor refusal probe did not observe the launcher-injected HTTP proxy")

	require.NoError(t, sendOpenCodePrompt(
		launch, fixture.cwd, "reply with exactly ok", openCodeDarwinProxyModel, ""))
	require.Eventuallyf(t, func() bool {
		return openCodeDarwinProxyDecisionsInclude(
			readOpenCodeDarwinProxyDecisions(t, fixture.supervisorHome),
			"http", openCodeDarwinProxyOrigin, 443, "allowed")
	}, 120*time.Second, 250*time.Millisecond,
		"the managed OpenCode model request did not use HTTP CONNECT through the production proxy; decisions:\n%s",
		formatOpenCodeDarwinProxyDecisions(
			readOpenCodeDarwinProxyDecisions(t, fixture.supervisorHome)))

	decisions := readOpenCodeDarwinProxyDecisions(t, fixture.supervisorHome)
	assert.True(t, openCodeDarwinProxyDecisionsInclude(
		decisions, "http", openCodeDarwinProxyDenied, 443, "not_authorized"),
		"the distinct undeclared control must execute a real refusal")
	assert.True(t, openCodeDarwinProxyDecisionsInclude(
		decisions, "http", openCodeDarwinProxyOrigin, 443, "allowed"),
		"synthetic control traffic cannot stand in for OpenCode's model-origin CONNECT")
}

// TestOpenCodeProxyCooperationDarwinFailureControl removes only the production
// managed-server bind carveout. The same pinned server must then fail to start,
// proving the green smoke depends on the real Darwin server/proxy wiring rather
// than merely on its synthetic refusal probe.
func TestOpenCodeProxyCooperationDarwinFailureControl(t *testing.T) {
	fixture := prepareOpenCodeDarwinProxySmoke(t)
	previous := wrapOpenCodeTclaudeLayerWithLoopbackBind
	wrapOpenCodeTclaudeLayerWithLoopbackBind = func(
		binary string,
		spec session.TclaudeLayerLaunchSpec,
		_ int,
		command string,
	) (string, error) {
		return session.WrapTclaudeLayerServerSpecWithLoopbackBind(
			binary, spec, 0, command)
	}
	t.Cleanup(func() { wrapOpenCodeTclaudeLayerWithLoopbackBind = previous })

	launch, err := startOpenCodeRuntime(
		openCodeDarwinProxySessionID, fixture.cwd, "OpenCode Darwin proxy failure control",
		"", fixture.permissionJSON, string(sandboxpolicy.ImplementationTclaudeLayer),
		fixture.spec, "")
	if launch != nil {
		_ = stopOpenCodeRuntime(openCodeDarwinProxySessionID)
	}
	require.Error(t, err,
		"the managed server must not start when the production bind carveout is absent")
	assert.False(t, openCodeDarwinProxyDecisionsInclude(
		readOpenCodeDarwinProxyDecisions(t, fixture.supervisorHome),
		"http", openCodeDarwinProxyOrigin, 443, "allowed"),
		"a launch that never established the managed server cannot provide model-origin evidence")
}

type openCodeDarwinProxySmokeFixture struct {
	cwd            string
	spec           *session.TclaudeLayerLaunchSpec
	permissionJSON string
	supervisorHome string
	serverLogDir   string
	probeMarker    string
}

func prepareOpenCodeDarwinProxySmoke(t *testing.T) openCodeDarwinProxySmokeFixture {
	t.Helper()
	if os.Getenv(openCodeDarwinProxySmokeEnv) != "1" {
		t.Skipf("set %s=1 on macOS with pinned OpenCode installed",
			openCodeDarwinProxySmokeEnv)
	}
	tclaudeBinary := strings.TrimSpace(os.Getenv(openCodeDarwinLayerSmokeTclaudeEnv))
	require.NotEmpty(t, tclaudeBinary, openCodeDarwinLayerSmokeTclaudeEnv)
	tclaudeBinary, err := filepath.Abs(tclaudeBinary)
	require.NoError(t, err)
	restoreLauncher := session.SetDarwinProxyLauncherExecutableForTest(tclaudeBinary)
	t.Cleanup(restoreLauncher)

	realOpenCode, err := harness.OpenCodeExecutable()
	require.NoError(t, err)
	version, err := exec.Command(realOpenCode, "--version").CombinedOutput()
	require.NoErrorf(t, err, "opencode --version: %s", version)
	require.Contains(t, string(version), openCodeDarwinProxyPinnedVersion,
		"Darwin cooperation evidence must execute pinned OpenCode 1.18.6")

	realHome, err := os.UserHomeDir()
	require.NoError(t, err)
	smokeBase := filepath.Join(realHome, ".cache")
	require.NoError(t, os.MkdirAll(smokeBase, 0o700))
	root, err := os.MkdirTemp(smokeBase, "tclaude-oc-proxy-")
	require.NoError(t, err)
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home := filepath.Join(root, "home")
	cwd := filepath.Join(root, "workspace")
	for _, path := range []string{home, cwd} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	removeOpenCodeDarwinProxyAmbientEnv(t)

	probeMarker := filepath.Join(cwd, "proxy-probe-passed")
	probeBinary := filepath.Join(cwd, "proxy-probe")
	copyOpenCodeDarwinLayerSmokeExecutable(t, os.Args[0], probeBinary)
	wrapper := filepath.Join(cwd, "opencode")
	wrapperScript := "#!/bin/sh\n" +
		clcommon.ShellQuoteArg(probeBinary) +
		" -test.run '^TestOpenCodeDarwinProxyProbeHelper$' -test.v >/dev/null 2>&1 &\n" +
		"exec " + clcommon.ShellQuoteArg(realOpenCode) + " \"$@\"\n"
	require.NoError(t, os.WriteFile(wrapper, []byte(wrapperScript), 0o700))
	t.Setenv("PATH", cwd+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(openCodeDarwinProxyProbeEnv, "1")
	t.Setenv(openCodeDarwinProxyMarkerEnv, probeMarker)

	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	agentSocket := filepath.Join(home, ".tclaude", "api", "agentd.sock")
	t.Setenv(agentipc.SocketEnv, agentSocket)
	stopAgentd := startOpenCodeDarwinLayerSmokeAgentd(t, tclaudeBinary, agentSocket)
	t.Cleanup(stopAgentd)

	config := map[string]any{
		"enabled_providers": []string{"test"},
		"provider": map[string]any{
			"test": map[string]any{
				"npm":       "@ai-sdk/openai-compatible",
				"whitelist": []string{"test-model"},
				"models": map[string]any{
					"test-model": map[string]any{
						"id": "test-model", "name": "Darwin proxy smoke model",
						"limit": map[string]int{"context": 100_000, "output": 10_000},
					},
				},
				"options": map[string]string{
					"baseURL": "https://" + openCodeDarwinProxyOrigin + "/v1",
					"apiKey":  "invalid-ci-evidence-key",
				},
			},
		},
	}
	configJSON, err := json.Marshal(config)
	require.NoError(t, err)
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.Network = &sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList, Engine: sandboxpolicy.NetworkEngineProxy,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			Host: openCodeDarwinProxyOrigin, Ports: []int{443},
		}},
	}
	snapshot.Effective.Environment = []sandboxpolicy.EnvironmentEntry{{
		Name: "OPENCODE_CONFIG_CONTENT", Value: string(configJSON),
	}}

	agentID := db.NewAgentID()
	allocation, err := allocatePrivateOpenCodeState(agentID)
	require.NoError(t, err)
	supervisorHome := filepath.Join(allocation.StateRoot, openCodeFilteredHomeBase)
	writeOpenCodeDarwinProxyDebugConfig(t, supervisorHome)
	spec, err := openCodeTclaudeLayerLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer), cwd, nil, &snapshot, agentID)
	require.NoError(t, err)
	require.NotNil(t, spec)
	require.Equal(t, session.TclaudeLayerLaunchSpecVersion, spec.Version,
		"Darwin keeps the authenticated loopback control transport")
	require.Equal(t, sandboxpolicy.NetworkEngineProxy, spec.Contract.NetworkEngine)
	permissionJSON, err := openCodePermissionJSONForLaunch(
		cwd, harness.OpenCodeSandboxTclaudeLayer, harness.OpenCodeApprovalDeny,
		harness.OpenCodeToolsAllow, &snapshot)
	require.NoError(t, err)

	return openCodeDarwinProxySmokeFixture{
		cwd: cwd, spec: spec, permissionJSON: permissionJSON,
		supervisorHome: supervisorHome,
		serverLogDir:   filepath.Join(allocation.StateRoot, "data", "opencode", "log"),
		probeMarker:    probeMarker,
	}
}

func TestOpenCodeDarwinProxyProbeHelper(t *testing.T) {
	if os.Getenv(openCodeDarwinProxyProbeEnv) != "1" {
		t.Skip("OpenCode Darwin proxy probe helper")
	}
	marker := strings.TrimSpace(os.Getenv(openCodeDarwinProxyMarkerEnv))
	require.NotEmpty(t, marker)
	rawProxy := strings.TrimSpace(os.Getenv("HTTPS_PROXY"))
	require.NotEmpty(t, rawProxy,
		"the production launcher must inject HTTPS_PROXY into the managed server boundary")
	parsed, err := url.Parse(rawProxy)
	require.NoError(t, err)
	status, err := openCodeDarwinProxyHTTPConnect(parsed.Host,
		net.JoinHostPort(openCodeDarwinProxyDenied, "443"))
	require.NoError(t, err)
	require.Equal(t, 403, status)
	require.NoError(t, os.WriteFile(marker, []byte("refused"), 0o600))
}

func openCodeDarwinProxyHTTPConnect(proxyEndpoint, target string) (int, error) {
	connection, err := net.DialTimeout("tcp4", proxyEndpoint, 5*time.Second)
	if err != nil {
		return 0, err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(connection,
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return 0, err
	}
	var version string
	var status int
	_, err = fmt.Fscanf(connection, "%s %d", &version, &status)
	return status, err
}

type openCodeDarwinProxyDecision struct {
	Message  string `json:"msg"`
	Carriage string `json:"carriage"`
	Host     string `json:"host"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Verdict  string `json:"verdict"`
}

func readOpenCodeDarwinProxyDecisions(
	t *testing.T,
	home string,
) []openCodeDarwinProxyDecision {
	t.Helper()
	paths := []string{
		filepath.Join(home, ".tclaude", "data", "output.log"),
		filepath.Join(home, ".tclaude", "output.log"),
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		decisions := []openCodeDarwinProxyDecision{}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, session.ProxyNetworkDecisionMessage) {
				continue
			}
			var decision openCodeDarwinProxyDecision
			require.NoErrorf(t, json.Unmarshal([]byte(line), &decision),
				"unparseable Darwin proxy decision: %s", line)
			decisions = append(decisions, decision)
		}
		if len(decisions) > 0 {
			return decisions
		}
	}
	return nil
}

func openCodeDarwinProxyDecisionsInclude(
	decisions []openCodeDarwinProxyDecision,
	carriage, host string,
	port int,
	verdict string,
) bool {
	for _, decision := range decisions {
		if decision.Carriage == carriage && decision.Host == host &&
			decision.Port == port && decision.Verdict == verdict {
			return true
		}
	}
	return false
}

func formatOpenCodeDarwinProxyDecisions(decisions []openCodeDarwinProxyDecision) string {
	lines := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		host := decision.Host
		if host == "" {
			host = decision.Address
		}
		lines = append(lines, fmt.Sprintf("%s %s -> %s",
			decision.Carriage, net.JoinHostPort(host, strconv.Itoa(decision.Port)),
			decision.Verdict))
	}
	return strings.Join(lines, "\n")
}

func writeOpenCodeDarwinProxyDebugConfig(t *testing.T, home string) {
	t.Helper()
	directory := filepath.Join(home, ".tclaude", "data")
	require.NoError(t, os.MkdirAll(directory, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "config.json"),
		[]byte(`{"log_level":"debug"}`), 0o600))
}

func removeOpenCodeDarwinProxyAmbientEnv(t *testing.T) {
	t.Helper()
	for _, entry := range session.ProxyNetworkCarriage("127.0.0.1:1") {
		name := entry.Name
		previous, present := os.LookupEnv(name)
		if !present {
			continue
		}
		require.NoError(t, os.Unsetenv(name))
		t.Cleanup(func() { _ = os.Setenv(name, previous) })
	}
}
