//go:build darwin

package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const darwinProxyLauncherHelperEnv = "TCLAUDE_DARWIN_PROXY_LAUNCHER_HELPER"

func TestResolveTclaudeLayerDarwinAcceptsFilteredSeatbeltCapability(t *testing.T) {
	oldStat := statDarwinSeatbelt
	oldProbe := probeDarwinSeatbelt
	t.Cleanup(func() {
		statDarwinSeatbelt = oldStat
		probeDarwinSeatbelt = oldProbe
	})
	executable, err := os.Stat(os.Args[0])
	require.NoError(t, err)
	statDarwinSeatbelt = func(path string) (os.FileInfo, error) {
		assert.Equal(t, darwinSeatbeltExecutable, path)
		return executable, nil
	}
	probed := false
	probeDarwinSeatbelt = func(path string) error {
		probed = true
		assert.Equal(t, darwinSeatbeltExecutable, path)
		return nil
	}

	binary, verdict, err := ResolveTclaudeLayer(
		sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed)
	require.NoError(t, err)
	assert.Equal(t, darwinSeatbeltExecutable, binary)
	assert.True(t, probed)
	assert.Equal(t, "on", verdict.State)
	assert.True(t, verdict.FilteredNetwork)
	assert.Contains(t, verdict.Source, "local access")
}

func TestResolveTclaudeLayerDarwinAcceptsIsolatedNetwork(t *testing.T) {
	oldStat := statDarwinSeatbelt
	oldProbe := probeDarwinSeatbelt
	t.Cleanup(func() {
		statDarwinSeatbelt = oldStat
		probeDarwinSeatbelt = oldProbe
	})

	executable, err := os.Stat(os.Args[0])
	require.NoError(t, err)
	statDarwinSeatbelt = func(path string) (os.FileInfo, error) {
		assert.Equal(t, darwinSeatbeltExecutable, path)
		return executable, nil
	}
	probed := false
	probeDarwinSeatbelt = func(path string) error {
		probed = true
		assert.Equal(t, darwinSeatbeltExecutable, path)
		return nil
	}

	binary, verdict, err := ResolveTclaudeLayer(
		sandboxpolicy.NetworkIsolatedWithAgentd, sandboxpolicy.RootConstructed)
	require.NoError(t, err)
	assert.Equal(t, darwinSeatbeltExecutable, binary)
	assert.True(t, probed)
	assert.Equal(t, "on", verdict.State)
	assert.Contains(t, verdict.Source, "isolated network")
}

func TestResolveTclaudeLayerDarwinRefusesMissingOrBrokenSeatbelt(t *testing.T) {
	oldStat := statDarwinSeatbelt
	oldProbe := probeDarwinSeatbelt
	t.Cleanup(func() {
		statDarwinSeatbelt = oldStat
		probeDarwinSeatbelt = oldProbe
	})

	statDarwinSeatbelt = func(string) (os.FileInfo, error) {
		return nil, errors.New("not found")
	}
	_, _, err := ResolveTclaudeLayer(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	require.ErrorContains(t, err, darwinSeatbeltExecutable)

	executable, statErr := os.Stat(os.Args[0])
	require.NoError(t, statErr)
	statDarwinSeatbelt = func(string) (os.FileInfo, error) {
		return executable, nil
	}
	probeDarwinSeatbelt = func(string) error {
		return errors.New("operation unexpectedly succeeded")
	}
	_, _, err = ResolveTclaudeLayer(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	require.ErrorContains(t, err, "deny-write capability")
}

func TestTclaudeLayerHostAvailabilityDarwinUsesSeatbeltCapability(t *testing.T) {
	oldStat := statDarwinSeatbelt
	oldProbe := probeDarwinSeatbelt
	t.Cleanup(func() {
		statDarwinSeatbelt = oldStat
		probeDarwinSeatbelt = oldProbe
	})

	executable, err := os.Stat(os.Args[0])
	require.NoError(t, err)
	statDarwinSeatbelt = func(path string) (os.FileInfo, error) {
		assert.Equal(t, darwinSeatbeltExecutable, path)
		return executable, nil
	}
	probed := false
	probeDarwinSeatbelt = func(path string) error {
		probed = true
		assert.Equal(t, darwinSeatbeltExecutable, path)
		return nil
	}
	require.NoError(t, TclaudeLayerHostAvailability())
	assert.True(t, probed, "availability must execute the same deny-write probe as launch")

	probeDarwinSeatbelt = func(string) error {
		return errors.New("deny probe failed")
	}
	require.ErrorContains(t, TclaudeLayerHostAvailability(), "deny-write capability")
}

func TestTclaudeLayerServerAvailabilityDarwinUsesSeatbeltCapability(t *testing.T) {
	oldStat := statDarwinSeatbelt
	oldProbe := probeDarwinSeatbelt
	t.Cleanup(func() {
		statDarwinSeatbelt = oldStat
		probeDarwinSeatbelt = oldProbe
	})

	executable, err := os.Stat(os.Args[0])
	require.NoError(t, err)
	statDarwinSeatbelt = func(path string) (os.FileInfo, error) {
		assert.Equal(t, darwinSeatbeltExecutable, path)
		return executable, nil
	}
	probed := false
	probeDarwinSeatbelt = func(path string) error {
		probed = true
		assert.Equal(t, darwinSeatbeltExecutable, path)
		return nil
	}
	require.NoError(t, TclaudeLayerServerHostAvailability())
	assert.True(t, probed,
		"OpenCode's server boundary must execute the same Seatbelt probe as launch")
}

func TestDarwinSeatbeltCapabilityProbeHasDeadline(t *testing.T) {
	oldRun := runDarwinSeatbeltProbe
	t.Cleanup(func() { runDarwinSeatbeltProbe = oldRun })
	t.Setenv("TMPDIR", "/private/var/folders/ab/runtime/T")

	runDarwinSeatbeltProbe = func(
		ctx context.Context,
		_, _, _ string,
	) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "the dashboard/launch capability predicate must be bounded")
		remaining := time.Until(deadline)
		assert.Greater(t, remaining, 4*time.Second)
		assert.LessOrEqual(t, remaining, darwinSeatbeltProbeTimeout)
		return nil, context.DeadlineExceeded
	}

	err := probeDarwinSeatbeltCapability(darwinSeatbeltExecutable)
	require.ErrorContains(t, err, "timed out after 5s")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestTclaudeLayerDarwinVerdictIsPlatformSpecificAndUnverified(t *testing.T) {
	hostOpen := TclaudeLayerLaunchOSSandbox(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	assert.Equal(t, "on", hostOpen.State)
	assert.Equal(t,
		"tclaude-layer (Seatbelt/sandbox-exec; host network)",
		hostOpen.Source,
	)
	assert.True(t, hostOpen.Unverified)
	assert.NotContains(t, hostOpen.Source, "bubblewrap")

	isolated := TclaudeLayerLaunchOSSandbox(
		sandboxpolicy.NetworkIsolatedWithAgentd, sandboxpolicy.RootConstructed)
	assert.Equal(t, "on", isolated.State)
	assert.Equal(t,
		"tclaude-layer (Seatbelt/sandbox-exec; isolated network; "+
			"host loopback/IDE bridge unavailable; agentd socket allowlisted)",
		isolated.Source,
	)
	assert.True(t, isolated.Unverified)

	local := TclaudeLayerLaunchOSSandbox(
		sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed)
	assert.Equal(t, "on", local.State)
	assert.True(t, local.FilteredNetwork)
	assert.Contains(t, local.Source, "real host loopback")
	assert.Contains(t, local.Source, "IDE bridge")
	assert.True(t, local.Unverified)

	openCode := TclaudeLayerLaunchOSSandboxForHarness(
		"opencode", sandboxpolicy.NetworkHostOpen,
		sandboxpolicy.RootHostInherited, sandboxpolicy.NetworkEngineUnset)
	assert.Equal(t, "on", openCode.State)
	assert.Contains(t, openCode.Source, "Seatbelt/sandbox-exec")
	assert.Contains(t, openCode.Source, "OpenCode tool-executing server confined")
	assert.Contains(t, openCode.Source,
		"mutable XDG privacy covers data/cache/state only")
	assert.Contains(t, openCode.Source, "config-base writes are not redirected")
	assert.True(t, openCode.Unverified)
}

func TestDarwinSeatbeltReadOnlyPathsRefusesSourceTargetProjection(t *testing.T) {
	const (
		source = "/Users/dev/.config/opencode"
		target = "/Users/dev/private/config/opencode"
	)
	got, err := darwinSeatbeltReadOnlyPaths([]TclaudeLayerReadOnlyBind{{
		Source: source,
		Target: source,
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{source}, got)

	_, err = darwinSeatbeltReadOnlyPaths([]TclaudeLayerReadOnlyBind{{
		Source: source,
		Target: target,
	}})
	require.ErrorContains(t, err, "darwin_seatbelt_path_projection")
	require.ErrorContains(t, err, source)
	require.ErrorContains(t, err, target)
}

func TestTclaudeLayerDarwinServerCommandUsesSeatbelt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", "/private/var/folders/ab/runtime/T")
	cwd := filepath.Join(home, "workspace")
	config := filepath.Join(home, ".config", "opencode")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	require.NoError(t, os.MkdirAll(config, 0o700))

	command, err := tclaudeLayerServerCommand(
		darwinSeatbeltExecutable,
		[]string{cwd},
		nil,
		nil,
		[]TclaudeLayerReadOnlyBind{{Source: config, Target: config}},
		sandboxpolicy.AgentdSocketFloor(),
		sandboxpolicy.MountPlan{NetworkPosture: sandboxpolicy.NetworkHostOpen},
		"opencode serve --hostname 127.0.0.1",
	)
	require.NoError(t, err)
	assert.Contains(t, command, darwinSeatbeltExecutable)
	assert.Contains(t, command, "opencode serve")
	assert.Contains(t, command, config)
}

func TestTclaudeLayerDarwinCommandCarriesFullAgentdSocketFloor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	command, err := tclaudeLayerCommand(
		darwinSeatbeltExecutable,
		nil,
		nil,
		nil,
		nil,
		sandboxpolicy.AgentdSocketFloor(),
		sandboxpolicy.MountPlan{NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd},
		"true",
	)
	require.NoError(t, err)
	for _, socket := range sandboxpolicy.AgentdSocketFloor() {
		want := socket
		if filepath.Clean(socket) == filepath.Clean(agentipc.CanonicalSocketPath()) {
			want = agentipc.CanonicalSocketDir()
		}
		assert.Containsf(t, command, want, "missing rendered agentd socket floor entry %s", want)
	}
}

func TestTclaudeLayerDarwinIsolatedCommandCarriesManagedServerPort(t *testing.T) {
	command, err := tclaudeLayerServerCommandWithLoopbackBind(
		darwinSeatbeltExecutable,
		nil,
		nil,
		nil,
		nil,
		sandboxpolicy.AgentdSocketFloor(),
		sandboxpolicy.MountPlan{NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd},
		43210,
		"true",
	)
	require.NoError(t, err)
	assert.Contains(t, command,
		`(deny network-bind (require-not (local tcp "localhost:43210")))`)
	assert.Contains(t, command,
		`(require-not (remote tcp "localhost:43210"))`)
}

func TestTclaudeLayerDarwinCommandDefersProxyFloorToLauncher(t *testing.T) {
	plan := darwinProxyLauncherTestPlan(t, 443)
	oldPrefix := darwinProxyLauncherPrefix
	darwinProxyLauncherPrefix = func() string {
		return "/usr/local/bin/tclaude session " + tclaudeLayerDarwinProxyLauncherCommand
	}
	t.Cleanup(func() { darwinProxyLauncherPrefix = oldPrefix })

	command, err := tclaudeLayerCommand(
		darwinSeatbeltExecutable, nil, nil, nil, nil, nil, plan, "true")
	require.NoError(t, err)
	assert.Contains(t, command, tclaudeLayerDarwinProxyLauncherCommand)
	assert.Contains(t, command, "--launch")
	assert.NotContains(t, command, " -p ",
		"the profile cannot be rendered before the launcher owns the actual endpoint")

	serverCommand, err := tclaudeLayerServerCommand(
		darwinSeatbeltExecutable, nil, nil, nil, nil, nil, plan, "true")
	require.NoError(t, err)
	encoded := strings.TrimPrefix(serverCommand,
		darwinProxyLauncherPrefix()+" --launch ")
	spec, err := decodeDarwinProxyLaunchSpec(encoded)
	require.NoError(t, err)
	assert.Equal(t, 2, spec.PreserveFDs,
		"the OpenCode server boundary keeps its launcher-owned descriptor pair")
	assert.Zero(t, spec.LoopbackBindPort,
		"the ordinary server renderer must not invent a listener exception")

	serverCommand, err = tclaudeLayerServerCommandWithLoopbackBind(
		darwinSeatbeltExecutable, nil, nil, nil, nil, nil, plan, 43210, "true")
	require.NoError(t, err)
	encoded = strings.TrimPrefix(serverCommand,
		darwinProxyLauncherPrefix()+" --launch ")
	spec, err = decodeDarwinProxyLaunchSpec(encoded)
	require.NoError(t, err)
	assert.Zero(t, spec.PreserveFDs,
		"the Darwin TCP control boundary has no inherited server descriptors")
	assert.Equal(t, 43210, spec.LoopbackBindPort,
		"the managed server control port must cross the deferred launcher boundary")
}

func TestDarwinProxyLauncherProductionPath(t *testing.T) {
	if os.Getenv(darwinProxyLauncherHelperEnv) == "1" {
		darwinProxyLauncherHelper(t)
		return
	}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "carried-by-darwin-filtering-proxy")
	}))
	t.Cleanup(origin.Close)
	originURL, err := url.Parse(origin.URL)
	require.NoError(t, err)
	_, rawPort, err := net.SplitHostPort(originURL.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(rawPort)
	require.NoError(t, err)

	t.Setenv(darwinProxyLauncherHelperEnv, "1")
	t.Setenv("TCLAUDE_DARWIN_PROXY_ORIGIN", origin.URL)
	spec := darwinProxyLaunchSpec{
		Binary: darwinSeatbeltExecutable,
		Plan:   darwinProxyLauncherTestPlan(t, port),
		HarnessCommand: clcommon.ShellQuoteArg(os.Args[0]) +
			" -test.run=^TestDarwinProxyLauncherProductionPath$ -test.v",
	}
	code, err := runDarwinProxyLauncher(spec)
	require.NoError(t, err)
	assert.Zero(t, code)
}

func TestDarwinProxyLauncherTreatsENODEVAsNonTerminal(t *testing.T) {
	oldGet := darwinProxyTerminalForegroundGroup
	oldSet := darwinProxySetTerminalForegroundGroup
	darwinProxyTerminalForegroundGroup = func(int) (int, error) {
		return 0, syscall.ENODEV
	}
	darwinProxySetTerminalForegroundGroup = func(int, int) error {
		t.Fatal("a non-terminal descriptor has no foreground group to set")
		return nil
	}
	t.Cleanup(func() {
		darwinProxyTerminalForegroundGroup = oldGet
		darwinProxySetTerminalForegroundGroup = oldSet
	})

	restore, err := darwinProxyGiveTerminalTo(4242)
	require.NoError(t, err)
	restore()
}

func darwinProxyLauncherTestPlan(t *testing.T, port int) sandboxpolicy.MountPlan {
	t.Helper()
	rules := sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeList,
		Engine: sandboxpolicy.NetworkEngineProxy,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			Loopback: true,
			Ports:    []int{port},
		}},
		Deny: []sandboxpolicy.NetworkAllowEntry{{Host: "blocked.invalid"}},
	}
	compiled, err := sandboxpolicy.CompileFilteredNetworkRules(rules)
	require.NoError(t, err)
	plan := sandboxpolicy.MountPlan{
		NetworkPosture:  sandboxpolicy.NetworkFiltered,
		NetworkEngine:   sandboxpolicy.NetworkEngineProxy,
		FilteredNetwork: &compiled,
	}
	require.True(t, tclaudeLayerPlanDeploysProxy(plan))
	return plan
}

func darwinProxyLauncherHelper(t *testing.T) {
	httpProxy := os.Getenv("HTTP_PROXY")
	require.NotEmpty(t, httpProxy)
	for _, name := range []string{"http_proxy", "HTTPS_PROXY", "https_proxy"} {
		assert.Equal(t, httpProxy, os.Getenv(name), name)
	}
	parsed, err := url.Parse(httpProxy)
	require.NoError(t, err)
	assert.Equal(t, "http", parsed.Scheme)
	for _, name := range []string{"ALL_PROXY", "all_proxy"} {
		assert.Equal(t, "socks5h://"+parsed.Host, os.Getenv(name), name)
	}
	assert.Empty(t, os.Getenv("NO_PROXY"))
	assert.Empty(t, os.Getenv("no_proxy"))

	transport := &http.Transport{Proxy: http.ProxyURL(parsed)}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get(os.Getenv("TCLAUDE_DARWIN_PROXY_ORIGIN"))
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	assert.Equal(t, "carried-by-darwin-filtering-proxy", string(body))
	fmt.Println("darwin-proxy-launcher: production path carried HTTP through the supervised endpoint")
}

func TestDarwinSeatbeltRuntimeTempDirRefusesNonstandardCarveout(t *testing.T) {
	t.Setenv("TMPDIR", "/Users/dev/operator-controlled")
	_, err := darwinSeatbeltRuntimeTempDir()
	require.ErrorContains(t, err, "only carves the standard /private/var/folders runtime tree")

	t.Setenv("TMPDIR", "/private/var/folders/ab/runtime/T")
	got, err := darwinSeatbeltRuntimeTempDir()
	require.NoError(t, err)
	assert.Equal(t, "/private/var/folders/ab/runtime/T", got)
}

func TestDarwinClaudeRuntimeScratchRootIsAutomaticAndHarnessScoped(t *testing.T) {
	base := t.TempDir()
	oldBase := darwinClaudeRuntimeTempBase
	darwinClaudeRuntimeTempBase = base
	t.Cleanup(func() { darwinClaudeRuntimeTempBase = oldBase })

	dirs, err := tclaudeLayerHarnessRuntimeWriteDirs(harness.DefaultName)
	require.NoError(t, err)
	require.Len(t, dirs, 2)
	canonicalBase, err := filepath.EvalSymlinks(base)
	require.NoError(t, err)
	assert.Equal(t, canonicalBase, dirs[0],
		"Claude's /tmp cwd bookkeeping needs the canonical temp root writable")
	claudeRuntimeDir := dirs[1]
	assert.Equal(t, filepath.Join(canonicalBase, fmt.Sprintf("claude-%d", os.Geteuid())), claudeRuntimeDir)
	info, err := os.Stat(claudeRuntimeDir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	dirs, err = tclaudeLayerHarnessRuntimeWriteDirs(harness.CodexName)
	require.NoError(t, err)
	assert.Empty(t, dirs, "non-Claude harnesses must not inherit Claude scratch authority")

	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "work")
	require.NoError(t, os.Mkdir(cwd, 0o700))
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.DefaultName,
		Cwd:         cwd,
	})
	require.NoError(t, err)
	assert.Contains(t, spec.Contract.WriteDirs, canonicalBase,
		"the canonical temp root must survive into the persisted launch contract")
	assert.Contains(t, spec.Contract.WriteDirs, filepath.Join(canonicalBase, fmt.Sprintf("claude-%d", os.Geteuid())),
		"the prepared root must survive into the persisted launch contract")

	require.NoError(t, os.Remove(claudeRuntimeDir))
	require.NoError(t, os.Symlink(t.TempDir(), claudeRuntimeDir))
	_, err = tclaudeLayerHarnessRuntimeWriteDirs(harness.DefaultName)
	require.ErrorContains(t, err, "must be a real directory owned by uid")
}
