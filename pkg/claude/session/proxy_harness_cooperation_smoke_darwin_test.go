//go:build darwin

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/common"
)

const (
	darwinProxyCooperationHelperEnv = "TCLAUDE_DARWIN_PROXY_COOPERATION_HELPER"
	darwinProxyCooperationMarker    = "darwin-proxy-cooperation: production launcher probe executed"
	darwinProxyCooperationDenied    = "undeclared.proxy.tclaude.test"
	darwinProxyClaudePinnedVersion  = "2.1.220"
	darwinProxyCodexPinnedVersion   = "0.145.0"
)

type darwinProxyCooperationScenario struct {
	name    string
	binary  string
	version string
	args    []string
	origin  string
	env     []string
	prepare func(*testing.T, string) []string
}

// TestPinnedProxyHarnessCooperationDarwin is TCL-827 §8.2 test 7. It launches
// the two pinned plain-CLI harnesses through the shipped tclaude Darwin proxy
// launcher with deliberately invalid credentials and reads the launcher's own
// CONNECT decision record. Real credentials never enter CI.
func TestPinnedProxyHarnessCooperationDarwin(t *testing.T) {
	if os.Getenv("TCLAUDE_SANDBOX_V2_SMOKE") != "1" {
		t.Skip("set TCLAUDE_SANDBOX_V2_SMOKE=1 on macOS to exercise sandbox-exec")
	}
	tclaudeBinary := strings.TrimSpace(os.Getenv("TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY"))
	require.NotEmpty(t, tclaudeBinary)
	tclaudeBinary, err := filepath.Abs(tclaudeBinary)
	require.NoError(t, err)

	claude := requirePinnedDarwinProxyHarness(t, "claude", darwinProxyClaudePinnedVersion)
	codex := requirePinnedDarwinProxyHarness(t, "codex", darwinProxyCodexPinnedVersion)
	scenarios := []darwinProxyCooperationScenario{
		{
			name: "claude", binary: claude, version: darwinProxyClaudePinnedVersion,
			args:   []string{"--print", "--model", "sonnet", "Reply with exactly ok."},
			origin: "api.anthropic.com",
			env:    []string{"ANTHROPIC_API_KEY=invalid-ci-evidence-key"},
		},
		{
			name: "codex", binary: codex, version: darwinProxyCodexPinnedVersion,
			args: append(darwinProxyCodexEndpointEvidenceArgs(),
				"exec", "--skip-git-repo-check", "--model", "gpt-5.4",
				"Reply with exactly ok."),
			origin: "api.openai.com",
			prepare: func(t *testing.T, workspace string) []string {
				codexHome := filepath.Join(workspace, ".codex")
				require.NoError(t, os.MkdirAll(codexHome, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(codexHome, "auth.json"),
					[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"invalid-ci-evidence-key"}`), 0o600))
				return []string{"CODEX_HOME=" + codexHome}
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			decisions, output := runDarwinProxyCooperationScenario(t, tclaudeBinary, scenario)
			assert.Contains(t, output, darwinProxyCooperationMarker,
				"the in-sandbox probe proves the shipped launcher injected the production endpoint")
			assert.Truef(t, darwinProxyDecisionsInclude(decisions, scenario.origin, 443, "allowed"),
				"%s %s did not reach %s through the Darwin proxy; decisions:\n%s",
				scenario.name, scenario.version, scenario.origin, formatDarwinProxyDecisions(decisions))
			assert.Truef(t, darwinProxyDecisionsIncludeRefusal(decisions, darwinProxyCooperationDenied),
				"the deliberate undeclared CONNECT was not refused and recorded; decisions:\n%s",
				formatDarwinProxyDecisions(decisions))
			for _, decision := range decisions {
				if decision.Host == scenario.origin || decision.Host == darwinProxyCooperationDenied ||
					decision.Kind == "literal" {
					continue
				}
				assert.NotEqualf(t, "allowed", decision.Verdict,
					"%s reached undeclared origin %s", scenario.name, decision.destination())
			}
		})
	}
}

func runDarwinProxyCooperationScenario(
	t *testing.T,
	tclaudeBinary string,
	scenario darwinProxyCooperationScenario,
) ([]darwinProxyDecisionRecord, string) {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(home, 0o700))
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	t.Setenv("HOME", home)

	rules := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList, Engine: sandboxpolicy.NetworkEngineProxy,
		Allow: []sandboxpolicy.NetworkAllowEntry{{Host: scenario.origin, Ports: []int{443}}},
	}
	compiled, err := sandboxpolicy.CompileFilteredNetworkRules(rules)
	require.NoError(t, err)
	plan := sandboxpolicy.MountPlan{
		NetworkPosture:  sandboxpolicy.NetworkFiltered,
		NetworkEngine:   sandboxpolicy.NetworkEngineProxy,
		FilteredNetwork: &compiled,
	}
	require.True(t, tclaudeLayerPlanDeploysProxy(plan))

	previousPrefix := darwinProxyLauncherPrefix
	darwinProxyLauncherPrefix = func() string {
		return clcommon.ShellQuoteArg(tclaudeBinary) +
			" --log-level debug session " + tclaudeLayerDarwinProxyLauncherCommand
	}
	t.Cleanup(func() { darwinProxyLauncherPrefix = previousPrefix })

	harnessCommand := clcommon.ShellQuoteArg(os.Args[0]) +
		" -test.run=^TestDarwinProxyCooperationProbeHelper$ -test.v; " +
		clcommon.ShellQuoteArg(scenario.binary) + " " + strings.Join(shellQuoteDarwinArgs(scenario.args), " ")
	command, err := tclaudeLayerCommand(
		darwinSeatbeltExecutable, []string{workspace}, nil, nil, nil, nil,
		plan, harnessCommand)
	require.NoError(t, err)
	require.Contains(t, command, tclaudeLayerDarwinProxyLauncherCommand)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), scenario.env...)
	cmd.Env = append(cmd.Env, darwinProxyCooperationHelperEnv+"=1")
	if scenario.prepare != nil {
		cmd.Env = append(cmd.Env, scenario.prepare(t, workspace)...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second

	var stopped atomic.Bool
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				decisions := readDarwinProxyDecisions(home)
				if darwinProxyDecisionsInclude(decisions, scenario.origin, 443, "allowed") &&
					darwinProxyDecisionsIncludeRefusal(decisions, darwinProxyCooperationDenied) {
					stopped.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	output, runErr := cmd.CombinedOutput()
	close(done)
	if !stopped.Load() {
		require.NoErrorf(t, ctx.Err(), "Darwin cooperation smoke timed out; output:\n%s", output)
		require.NoErrorf(t, runErr, "Darwin cooperation launch exited before evidence; output:\n%s", output)
	}
	decisions := readDarwinProxyDecisions(home)
	require.NotEmptyf(t, decisions,
		"the shipped launcher left no proxy decision record in %s; output:\n%s",
		common.OutputLogPath(), output)
	return decisions, string(output)
}

// TestPinnedProxyHarnessCooperationDarwinFailureControl proves the smoke's
// production-path marker is not decorative: the same probe run without the
// launcher must fail because no launcher-owned proxy endpoint was injected.
func TestPinnedProxyHarnessCooperationDarwinFailureControl(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestDarwinProxyCooperationProbeHelper$", "-test.v")
	cmd.Env = append(withoutDarwinProxyVariables(os.Environ()), darwinProxyCooperationHelperEnv+"=1")
	output, err := cmd.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(output), "must be launched through the production Darwin proxy launcher")
}

func TestDarwinProxyCooperationProbeHelper(t *testing.T) {
	if os.Getenv(darwinProxyCooperationHelperEnv) != "1" {
		t.Skip("Darwin proxy cooperation helper subprocess")
	}
	rawProxy := os.Getenv("HTTP_PROXY")
	require.NotEmpty(t, rawProxy, "must be launched through the production Darwin proxy launcher")
	parsed, err := url.Parse(rawProxy)
	require.NoError(t, err)
	require.Equal(t, "http", parsed.Scheme)
	status, err := darwinProxyHTTPConnect(parsed.Host,
		net.JoinHostPort(darwinProxyCooperationDenied, strconv.Itoa(443)))
	require.NoError(t, err)
	require.Equal(t, 403, status)
	fmt.Println(darwinProxyCooperationMarker)
}

func darwinProxyHTTPConnect(proxyEndpoint, target string) (int, error) {
	conn, err := net.DialTimeout("tcp4", proxyEndpoint, 5*time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return 0, err
	}
	var version string
	var status int
	_, err = fmt.Fscanf(conn, "%s %d", &version, &status)
	return status, err
}

type darwinProxyDecisionRecord struct {
	Carriage string `json:"carriage"`
	Kind     string `json:"target_kind"`
	Host     string `json:"host"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Verdict  string `json:"verdict"`
}

func readDarwinProxyDecisions(home string) []darwinProxyDecisionRecord {
	paths := []string{
		common.OutputLogPath(),
		filepath.Join(home, ".tclaude", "data", "output.log"),
		filepath.Join(home, ".tclaude", "output.log"),
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = lines[:len(lines)-1]
		}
		records := []darwinProxyDecisionRecord{}
		for _, line := range lines {
			if !strings.Contains(line, ProxyNetworkDecisionMessage) {
				continue
			}
			var record darwinProxyDecisionRecord
			if json.Unmarshal([]byte(line), &record) == nil {
				records = append(records, record)
			}
		}
		if len(records) > 0 {
			return records
		}
	}
	return nil
}

func darwinProxyDecisionsInclude(
	decisions []darwinProxyDecisionRecord, host string, port int, verdict string,
) bool {
	return slices.ContainsFunc(decisions, func(decision darwinProxyDecisionRecord) bool {
		return decision.Host == host && decision.Port == port && decision.Verdict == verdict
	})
}

func darwinProxyDecisionsIncludeRefusal(decisions []darwinProxyDecisionRecord, host string) bool {
	return slices.ContainsFunc(decisions, func(decision darwinProxyDecisionRecord) bool {
		return decision.Host == host && decision.Verdict != "allowed"
	})
}

func (r darwinProxyDecisionRecord) destination() string {
	host := r.Host
	if host == "" {
		host = r.Address
	}
	return net.JoinHostPort(host, strconv.Itoa(r.Port))
}

func formatDarwinProxyDecisions(decisions []darwinProxyDecisionRecord) string {
	lines := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		lines = append(lines, fmt.Sprintf("  %s %s -> %s",
			decision.Carriage, decision.destination(), decision.Verdict))
	}
	return strings.Join(lines, "\n")
}

func shellQuoteDarwinArgs(args []string) []string {
	quoted := make([]string, len(args))
	for index, arg := range args {
		quoted[index] = clcommon.ShellQuoteArg(arg)
	}
	return quoted
}

func withoutDarwinProxyVariables(env []string) []string {
	owned := map[string]struct{}{}
	for _, name := range proxyNetworkProxyVariables {
		owned[name] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, pair := range env {
		name, _, ok := strings.Cut(pair, "=")
		if ok {
			if _, remove := owned[name]; remove {
				continue
			}
		}
		out = append(out, pair)
	}
	return out
}

func requirePinnedDarwinProxyHarness(t *testing.T, name, version string) string {
	t.Helper()
	binary, err := exec.LookPath(name)
	require.NoError(t, err)
	output, err := exec.Command(binary, "--version").CombinedOutput()
	require.NoErrorf(t, err, "%s --version:\n%s", name, output)
	require.Contains(t, string(output), version,
		"Darwin cooperation evidence must execute the CI-pinned harness version")
	return binary
}

func darwinProxyCodexEndpointEvidenceArgs() []string {
	return []string{
		"--disable", "plugins",
		"--disable", "remote_plugin",
		"--disable", "plugin_sharing",
	}
}
