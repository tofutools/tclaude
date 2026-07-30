//go:build linux

package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"golang.org/x/sys/unix"
)

const (
	openCodeLayerSmokeEnv         = "TCLAUDE_OPENCODE_LAYER_SMOKE"
	openCodeLayerSmokeTclaudeEnv  = "TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY"
	openCodeLayerSmokeSessionID   = "opencode-layer-smoke"
	openCodeLayerSmokeAttachProbe = 5 * time.Second
	openCodeLayerSmokeShellRetry  = 250 * time.Millisecond
	openCodeLayerSmokeShellWait   = 60 * time.Second
	openCodeFilteredToolHelperEnv = "TCLAUDE_OPENCODE_FILTERED_TOOL_HELPER"
)

// The deny fixture is the same live adjacent-target namespace the Claude/Codex
// gateway smoke consumes, so the CI job provisions it under the same names.
const (
	openCodeFilteredAllowedAddrEnv    = "TCLAUDE_FILTERED_ALLOWED_ADDR"
	openCodeFilteredAdjacentAddrEnv   = "TCLAUDE_FILTERED_ADJACENT_ADDR"
	openCodeFilteredAllowedAddr6Env   = "TCLAUDE_FILTERED_ALLOWED_ADDR6"
	openCodeFilteredAdjacentAddr6Env  = "TCLAUDE_FILTERED_ADJACENT_ADDR6"
	openCodeFilteredAllowedPrefix6Env = "TCLAUDE_FILTERED_ALLOWED_PREFIX6"
	openCodeFilteredAllowedPortEnv    = "TCLAUDE_FILTERED_ALLOWED_PORT"
	openCodeFilteredDeniedPortEnv     = "TCLAUDE_FILTERED_DENIED_PORT"
	// Both names resolve to the adjacent fixture address, so a denied name and
	// an allowed name deliberately share one destination address.
	openCodeFilteredDenyHost    = "exact-host.filtered.test"
	openCodeFilteredAllowedHost = "sibling.filtered.test"
	openCodeFilteredProbeWait   = 900 * time.Millisecond
	// Mirrors the broker's unexported filteredNetworkDNSHostMappingTTL. Every
	// negative-lease assertion refreshes the lease first rather than relying on
	// this value, which is used only to observe expiry.
	openCodeFilteredDNSMappingTTL = 2 * time.Second
	openCodeFilteredDenyHelperTag = "opencode-filtered-deny-helper PASS"
)

// TestOpenCodeTclaudeLayerExecutorSmoke is the real integration proof for the
// server-authoritative topology. It launches the actual OpenCode server as a
// child of bubblewrap, attaches the actual TUI outside that boundary, verifies
// the persisted permission suffix, drives OpenCode's real bash tool endpoint,
// and requires agentd to resolve the tool subprocess to the exact stable agent
// identity through the recorded wrapper ancestry.
func TestOpenCodeTclaudeLayerExecutorSmoke(t *testing.T) {
	runOpenCodeTclaudeLayerExecutorSmoke(t, false)
}

// TestOpenCodeFilteredNetworkExecutorSmoke is the M3 activation boundary. It
// keeps the real server and attach client on the inherited Unix control relay,
// proves the inspected explicit-provider config with a real model request, and
// drives real bash-tool TCP/UDP traffic through the packet-enforced gateway.
func TestOpenCodeFilteredNetworkExecutorSmoke(t *testing.T) {
	runOpenCodeTclaudeLayerExecutorSmoke(t, true)
}

func runOpenCodeTclaudeLayerExecutorSmoke(t *testing.T, filtered bool) {
	if os.Getenv(openCodeLayerSmokeEnv) != "1" {
		t.Skip("set TCLAUDE_OPENCODE_LAYER_SMOKE=1 on an unsandboxed Linux host with bubblewrap and OpenCode")
	}
	posture := sandboxpolicy.NetworkIsolatedWithAgentd
	if filtered {
		posture = sandboxpolicy.NetworkFiltered
	}
	_, _, err := session.ResolveTclaudeLayerServer(posture)
	require.NoError(t, err)
	openCodeExecutable, err := harness.OpenCodeExecutable()
	require.NoError(t, err)
	tclaudeBinary := strings.TrimSpace(os.Getenv(openCodeLayerSmokeTclaudeEnv))
	require.NotEmpty(t, tclaudeBinary)
	tclaudeBinary, err = filepath.Abs(tclaudeBinary)
	require.NoError(t, err)
	previousRelayExecutable := openCodeRelayExecutable
	openCodeRelayExecutable = func() (string, error) { return tclaudeBinary, nil }
	t.Cleanup(func() { openCodeRelayExecutable = previousRelayExecutable })

	// OpenCode can finish asynchronous dependency-cache writes just after its
	// server process exits. testing.T.TempDir performs one immediate RemoveAll,
	// which races those final writes on CI. Own this directory's cleanup so all
	// registered process cleanups run first, then require the tree to become
	// quiescent and removable within a bounded window.
	home, err := os.MkdirTemp("", "toc-*")
	require.NoError(t, err)
	home, err = filepath.EvalSymlinks(home)
	require.NoError(t, err)
	t.Cleanup(func() {
		var absentSince time.Time
		require.Eventuallyf(t, func() bool {
			if _, err := os.Stat(home); err == nil {
				absentSince = time.Time{}
			}
			if err := os.RemoveAll(home); err != nil {
				absentSince = time.Time{}
				return false
			}
			_, err := os.Stat(home)
			if !errors.Is(err, os.ErrNotExist) {
				absentSince = time.Time{}
				return false
			}
			if absentSince.IsZero() {
				absentSince = time.Now()
				return false
			}
			return time.Since(absentSince) >= 100*time.Millisecond
		}, 5*time.Second, 50*time.Millisecond,
			"OpenCode smoke home remained active after process teardown")
	})
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	root := filepath.Join(home, "fixture")
	cwd := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	ambientData := filepath.Join(home, "data", "opencode")
	ambientCache := filepath.Join(home, "cache", "opencode")
	ambientConfig := filepath.Join(home, "config", "opencode")
	ambientState := filepath.Join(home, "state", "opencode")
	install := filepath.Join(home, ".opencode")
	for _, path := range []string{
		cwd, outside, ambientData, ambientCache, ambientConfig, ambientState, install,
	} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	identityProbeBinary := filepath.Join(cwd, "tclaude-agent-probe")
	copyOpenCodeLayerSmokeExecutable(t, tclaudeBinary, identityProbeBinary)
	networkProbeBinary := filepath.Join(cwd, "opencode-network-probe")
	if filtered {
		testBinary, executableErr := os.Executable()
		require.NoError(t, executableErr)
		copyOpenCodeLayerSmokeExecutable(t, testBinary, networkProbeBinary)
	}
	for _, path := range []string{
		filepath.Join(ambientData, "ambient-data-marker"),
		filepath.Join(ambientCache, "ambient-cache-marker"),
		filepath.Join(ambientState, "ambient-state-marker"),
		filepath.Join(ambientConfig, "shared-config-marker"),
		filepath.Join(install, "shared-install-marker"),
	} {
		require.NoError(t, os.WriteFile(path, []byte("marker"), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(ambientConfig, ".gitignore"),
		[]byte("node_modules\npackage.json\npackage-lock.json\n.gitignore\n"), 0o600))

	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	agentSocket := filepath.Join(home, ".tclaude", "api", "agentd.sock")
	t.Setenv(agentipc.SocketEnv, agentSocket)
	stopAgentd := startOpenCodeLayerSmokeAgentd(t, tclaudeBinary, agentSocket)
	t.Cleanup(stopAgentd)

	snapshot := sandboxpolicy.EmptySnapshot()
	if filtered {
		snapshot.Effective.Network = &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
		}
	} else {
		snapshot.Effective.NetworkAccess = sandboxpolicy.NetworkAccessNone
	}
	snapshot.Effective.Filesystem = []sandboxpolicy.FilesystemGrant{
		{Path: outside, Access: sandboxpolicy.AccessDeny},
	}
	snapshot.Effective.Environment = []sandboxpolicy.EnvironmentEntry{{
		Name: "TCLAUDE_OPENCODE_EXECUTOR_SMOKE", Value: "frozen-profile-value",
	}}
	smokeAgentID := db.NewAgentID()
	allocation, err := allocatePrivateOpenCodeState(smokeAgentID)
	require.NoError(t, err)
	siblingAgentID := db.NewAgentID()
	siblingAllocation, err := allocatePrivateOpenCodeState(siblingAgentID)
	require.NoError(t, err)
	siblingControlPath := filepath.Join(siblingAllocation.StateRoot, "control.sock")
	siblingControl, _, _, err := opencodeapi.CreateUnixListener(siblingControlPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = siblingControl.Close()
		_ = os.Remove(siblingControlPath)
	})
	siblingMarker := filepath.Join(siblingAllocation.StateRoot, "sibling-marker")
	require.NoError(t, os.WriteFile(siblingMarker, []byte("sibling"), 0o600))
	var filteredFixture *openCodeFilteredSmokeFixture
	if filtered {
		filteredFixture = newOpenCodeFilteredSmokeFixture(t, cwd)
		snapshot.Effective.Network.Allow, snapshot.Effective.Network.Deny =
			openCodeFilteredNetworkRules(filteredFixture)
		snapshot.Effective.Environment = append(
			snapshot.Effective.Environment,
			filteredFixture.environment...,
		)
		resolved, resolveErr := session.ResolveTclaudeLayerModelTransport(
			harness.MustGet(harness.OpenCodeName),
			session.ModelTransportLaunchContext{
				Model:       "test/test-model",
				Cwd:         cwd,
				Environment: snapshot.Effective.Environment,
			},
		)
		require.NoError(t, resolveErr)
		require.Equal(t, filteredFixture.modelBaseURL, resolved.BaseURL)
		hostileEnvironment := append(
			[]sandboxpolicy.EnvironmentEntry(nil),
			snapshot.Effective.Environment...,
		)
		for index := range hostileEnvironment {
			if hostileEnvironment[index].Name == "OPENCODE_CONFIG_CONTENT" {
				hostileEnvironment[index].Value = filteredFixture.hostileModelConfig
			}
		}
		_, hostileErr := session.ResolveTclaudeLayerModelTransport(
			harness.MustGet(harness.OpenCodeName),
			session.ModelTransportLaunchContext{
				Model:       "test/test-model",
				Cwd:         cwd,
				Environment: hostileEnvironment,
			},
		)
		require.ErrorContains(t, hostileErr, "may not override model.provider",
			"the pinned activation gate must reject an opaque model adapter")
		_, validateErr := session.ValidateTclaudeLayerNetwork(
			harness.MustGet(harness.OpenCodeName), snapshot.Effective, resolved)
		require.NoError(t, validateErr)

		accountAgentID := db.NewAgentID()
		accountAllocation, allocationErr :=
			allocatePrivateOpenCodeState(accountAgentID)
		require.NoError(t, allocationErr)
		plantOpenCodeFilteredActiveAccount(
			t,
			accountAllocation.StateRoot,
			fmt.Sprintf("http://%s:%d",
				sandboxpolicy.FilteredNetworkHostLoopbackName,
				filteredFixture.modelPort),
		)
		_, accountErr := openCodeTclaudeLayerLaunchSpec(
			string(sandboxpolicy.ImplementationTclaudeLayer),
			cwd, nil, &snapshot, accountAgentID)
		require.ErrorContains(t, accountErr, "active persistent account/org",
			"remote account config must refuse before the server can fetch it")
		assert.Zero(t, filteredFixture.accountRequests.Load(),
			"account refusal must happen before remote provider-config traffic")
	}
	var spec *session.TclaudeLayerLaunchSpec
	if filtered {
		spec, err = openCodeTclaudeLayerLaunchSpec(
			string(sandboxpolicy.ImplementationTclaudeLayer),
			cwd, nil, &snapshot, smokeAgentID)
	} else {
		spec, err = openCodeUnixRelayLaunchSpec(
			string(sandboxpolicy.ImplementationTclaudeLayer), cwd, nil, &snapshot,
			smokeAgentID)
	}
	require.NoError(t, err)
	require.NotNil(t, spec)
	permissionJSON, err := openCodePermissionJSONForLaunch(
		cwd,
		harness.OpenCodeSandboxTclaudeLayer,
		harness.OpenCodeApprovalDeny,
		harness.OpenCodeToolsAllow,
		&snapshot,
	)
	require.NoError(t, err)
	launch, err := startOpenCodeRuntime(
		openCodeLayerSmokeSessionID, cwd, "OpenCode layer smoke", "", permissionJSON, spec)
	if err != nil {
		logOpenCodeLayerSmokeServerLogs(t,
			filepath.Join(allocation.StateRoot, "data", "opencode", "log"))
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = stopOpenCodeRuntime(openCodeLayerSmokeSessionID) })
	require.NotEmpty(t, launch.ConvID)

	now := time.Now()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:                    openCodeLayerSmokeSessionID,
		ConvID:                launch.ConvID,
		Harness:               harness.OpenCodeName,
		SandboxMode:           harness.OpenCodeSandboxTclaudeLayer,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		Cwd:                   cwd,
		Status:                session.StatusWorking,
		CreatedAt:             now,
		UpdatedAt:             now,
	}))
	expectedAgentID, _, err := db.EnsureAgentForConvWithID(
		launch.ConvID, smokeAgentID, "smoke")
	require.NoError(t, err)

	runtime, err := db.GetOpenCodeRuntime(openCodeLayerSmokeSessionID)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	assert.Equal(t, db.OpenCodeTransportUnixRelay, runtime.Transport)
	assert.Equal(t, string(sandboxpolicy.ImplementationTclaudeLayer),
		runtime.SandboxImplementation)
	require.NoError(t, ensureOpenCodeSessionPermission(*runtime),
		"the actual server must retain the compiled permission suffix")
	hostTCP, tcpErr := net.DialTimeout("tcp", strings.TrimPrefix(
		runtime.ServerURL, "http://"), 250*time.Millisecond)
	if hostTCP != nil {
		_ = hostTCP.Close()
	}
	require.Error(t, tcpErr,
		"the internal OpenCode listener must not be reachable through host TCP")
	if filtered {
		require.NoError(t, sendOpenCodePrompt(
			launch, cwd, "reply with provider-ok", "test/test-model", ""))
		require.Eventually(t, func() bool {
			return filteredFixture.modelRequests.Load() > 0
		}, 15*time.Second, 25*time.Millisecond,
			"the real OpenCode server did not reach its inspected options.baseURL")
		assert.Zero(t, filteredFixture.modelsRequests.Load(),
			"models fetch must stay disabled")
		assert.Zero(t, filteredFixture.authRequests.Load(),
			"stored auth must stay replaced by the empty launch authority")
		assert.NoFileExists(t, filteredFixture.pluginMarker,
			"pure mode must prevent external plugin execution")
	}

	stopAttach := startOpenCodeLayerSmokeAttach(
		t, tclaudeBinary, openCodeExecutable, *runtime, launch.ConvID, cwd,
		spec.Contract.Environment)
	t.Cleanup(stopAttach)

	networkCommand := ""
	expectedConfigHome := filepath.Join(allocation.StateRoot, "config")
	expectedHome := home
	filteredConfigWriteChecks := ""
	if filtered {
		networkCommand = fmt.Sprintf(
			"%s=%s %s -test.run=^TestOpenCodeFilteredNetworkToolHelper$; ",
			openCodeFilteredToolHelperEnv,
			clcommon.ShellQuoteArg(filteredFixture.helperConfig),
			clcommon.ShellQuoteArg(networkProbeBinary),
		)
		expectedConfigHome = filepath.Join(
			allocation.StateRoot, openCodeFilteredConfigBase)
		expectedHome = filepath.Join(
			allocation.StateRoot, openCodeFilteredHomeBase)
		filteredConfigWriteChecks =
			"if printf hostile > \"$XDG_CONFIG_HOME/opencode/opencode.json\"; then exit 101; fi; " +
				"if printf hostile > \"$HOME/.opencode/opencode.json\"; then exit 102; fi; "
	}
	command := fmt.Sprintf(
		"set -eu; test \"$TCLAUDE_OPENCODE_EXECUTOR_SMOKE\" = frozen-profile-value; "+
			"test \"$XDG_DATA_HOME\" = %s; test \"$XDG_CACHE_HOME\" = %s; "+
			"test \"$XDG_CONFIG_HOME\" = %s; test \"$XDG_STATE_HOME\" = %s; test \"$HOME\" = %s; "+
			"%s"+
			"printf executor-ok > %s; printf state-ok > \"$XDG_STATE_HOME/opencode/tool-state\"; "+
			"if printf blocked > %s; then exit 97; fi; "+
			"for hidden in %s %s %s %s; do if test -r \"$hidden\"; then exit 98; fi; done; "+
			"test -r %s; test -r %s; "+
			"if printf planted > %s; then exit 99; fi; "+
			"if printf planted > %s; then exit 100; fi; "+
			"test ! -S %s; test ! -e %s; "+
			"%s%s agent whoami",
		clcommon.ShellQuoteArg(filepath.Join(allocation.StateRoot, "data")),
		clcommon.ShellQuoteArg(filepath.Join(allocation.StateRoot, "cache")),
		clcommon.ShellQuoteArg(expectedConfigHome),
		clcommon.ShellQuoteArg(filepath.Join(allocation.StateRoot, "state")),
		clcommon.ShellQuoteArg(expectedHome),
		filteredConfigWriteChecks,
		clcommon.ShellQuoteArg(filepath.Join(cwd, "tool-written")),
		clcommon.ShellQuoteArg(filepath.Join(outside, "blocked")),
		clcommon.ShellQuoteArg(siblingMarker),
		clcommon.ShellQuoteArg(filepath.Join(ambientData, "ambient-data-marker")),
		clcommon.ShellQuoteArg(filepath.Join(ambientCache, "ambient-cache-marker")),
		clcommon.ShellQuoteArg(filepath.Join(ambientState, "ambient-state-marker")),
		clcommon.ShellQuoteArg(filepath.Join(ambientConfig, "shared-config-marker")),
		clcommon.ShellQuoteArg(filepath.Join(install, "shared-install-marker")),
		clcommon.ShellQuoteArg(filepath.Join(ambientConfig, "config-write-blocked")),
		clcommon.ShellQuoteArg(filepath.Join(install, "install-write-blocked")),
		clcommon.ShellQuoteArg(runtime.ControlSocketPath),
		clcommon.ShellQuoteArg(siblingControlPath),
		networkCommand,
		clcommon.ShellQuoteArg(identityProbeBinary),
	)
	output := runOpenCodeLayerSmokeShell(t, *runtime, command)
	if filtered {
		require.Containsf(t, output, openCodeFilteredDenyHelperTag,
			"the real bash tool must report the deny boundary it executed: %s", output)
	}
	require.FileExists(t, filepath.Join(cwd, "tool-written"))
	_, statErr := os.Stat(filepath.Join(outside, "blocked"))
	require.ErrorIs(t, statErr, os.ErrNotExist,
		"the real OpenCode bash tool must remain inside the server's mount boundary")
	require.FileExists(t, filepath.Join(allocation.StateRoot, "data", "opencode", "opencode.db"))
	require.FileExists(t, filepath.Join(allocation.StateRoot, "state", "opencode", "tool-state"))

	var identityLine string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "agt_") {
			identityLine = strings.TrimSpace(line)
		}
	}
	require.NotEmptyf(t, identityLine, "tool output did not contain managed identity: %q", output)
	assert.Equal(t, expectedAgentID, strings.Fields(identityLine)[0],
		"agentd must resolve the exact managed identity through the wrapped server ancestry")

	if filtered {
		plantOpenCodeFilteredActiveAccount(
			t,
			allocation.StateRoot,
			fmt.Sprintf("http://%s:%d",
				sandboxpolicy.FilteredNetworkHostLoopbackName,
				filteredFixture.modelPort),
		)
		_, replayErr := openCodeRuntimeSandboxSpec(*runtime)
		require.ErrorContains(t, replayErr, "active persistent account/org",
			"persisted replay preflight must name the hostile account authority")
		stopOpenCodeProcess(*runtime, nil)
		require.Eventually(t, func() bool {
			return !session.IsProcessAlive(runtime.PID)
		}, 5*time.Second, 25*time.Millisecond,
			"filtered OpenCode server did not stop for persisted replay proof")
		assert.False(t, reconcileOpenCodeRuntime(runtime.SessionID),
			"persisted filtered replay must refuse newly active account/org authority")
		assert.Zero(t, filteredFixture.accountRequests.Load(),
			"persisted replay refusal must happen before remote provider-config traffic")
	}
}

type openCodeFilteredSmokeFixture struct {
	modelPort          int
	allowedPort        int
	deniedPort         int
	deny               openCodeFilteredDenyFixture
	modelBaseURL       string
	environment        []sandboxpolicy.EnvironmentEntry
	helperConfig       string
	hostileModelConfig string
	pluginMarker       string
	modelRequests      atomic.Int32
	modelsRequests     atomic.Int32
	authRequests       atomic.Int32
	accountRequests    atomic.Int32
}

// openCodeFilteredDenyFixture names the live adjacent targets the deny cases
// need. The loopback echo pairs cannot carry them: a DNS deny installs a
// negative lease on the answered address, and the host-loopback address is the
// same destination the inspected model route depends on.
type openCodeFilteredDenyFixture struct {
	allowedAddr    string
	adjacentAddr   string
	allowedAddr6   string
	adjacentAddr6  string
	allowedPrefix6 string
	coveringPrefix string
	allowedPort    int
	deniedPort     int
}

type openCodeFilteredToolHelperConfig struct {
	Allowed string                       `json:"allowed"`
	Denied  string                       `json:"denied"`
	Deny    *openCodeFilteredDenyProbeIR `json:"deny,omitempty"`
}

// openCodeFilteredDenyProbeIR is the flattened address plan the bash-tool
// helper replays inside the confined server's namespace.
type openCodeFilteredDenyProbeIR struct {
	AllowedAddr   string `json:"allowed_addr"`
	AdjacentAddr  string `json:"adjacent_addr"`
	AllowedAddr6  string `json:"allowed_addr6"`
	AdjacentAddr6 string `json:"adjacent_addr6"`
	AllowedPort   int    `json:"allowed_port"`
	DeniedPort    int    `json:"denied_port"`
}

func newOpenCodeFilteredDenyFixture(t *testing.T) openCodeFilteredDenyFixture {
	t.Helper()
	fixture := openCodeFilteredDenyFixture{
		allowedAddr:    requireOpenCodeFilteredEnv(t, openCodeFilteredAllowedAddrEnv),
		adjacentAddr:   requireOpenCodeFilteredEnv(t, openCodeFilteredAdjacentAddrEnv),
		allowedAddr6:   requireOpenCodeFilteredEnv(t, openCodeFilteredAllowedAddr6Env),
		adjacentAddr6:  requireOpenCodeFilteredEnv(t, openCodeFilteredAdjacentAddr6Env),
		allowedPrefix6: requireOpenCodeFilteredEnv(t, openCodeFilteredAllowedPrefix6Env),
		allowedPort:    requireOpenCodeFilteredPort(t, openCodeFilteredAllowedPortEnv),
		deniedPort:     requireOpenCodeFilteredPort(t, openCodeFilteredDeniedPortEnv),
	}
	fixture.coveringPrefix = openCodeFilteredCoveringPrefix(
		t, fixture.allowedAddr, fixture.adjacentAddr)
	return fixture
}

// requireOpenCodeFilteredEnv refuses a degraded run rather than skipping the
// deny cases. Silent omission would let the smoke keep reporting a pass while
// covering strictly less than the activation record claims.
func requireOpenCodeFilteredEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	require.NotEmptyf(t, value,
		"%s is required; the filtered smoke's deny cases need the live adjacent "+
			"target fixture", name)
	return value
}

func requireOpenCodeFilteredPort(t *testing.T, name string) int {
	t.Helper()
	value, err := strconv.Atoi(requireOpenCodeFilteredEnv(t, name))
	require.NoError(t, err)
	require.Greater(t, value, 0)
	require.LessOrEqual(t, value, 65535)
	return value
}

func openCodeFilteredCoveringPrefix(t *testing.T, first, second string) string {
	t.Helper()
	a, err := netip.ParseAddr(first)
	require.NoError(t, err)
	b, err := netip.ParseAddr(second)
	require.NoError(t, err)
	require.Equal(t, a.BitLen(), b.BitLen())
	for bits := a.BitLen(); bits >= 0; bits-- {
		prefix := netip.PrefixFrom(a, bits).Masked()
		if prefix.Contains(b) {
			return prefix.String()
		}
	}
	t.Fatal("fixture addresses have no covering prefix")
	return ""
}

// openCodeFilteredNetworkRules authors one launch policy that carries both the
// allow surface the activation smoke already proved and the deny surface this
// boundary adds. Deny precedence is exercised in both overlap directions: IPv4
// narrows a broad allow, IPv6 widens over a narrow one.
func openCodeFilteredNetworkRules(
	fixture *openCodeFilteredSmokeFixture,
) (allow, deny []sandboxpolicy.NetworkAllowEntry) {
	denyFixture := fixture.deny
	allow = []sandboxpolicy.NetworkAllowEntry{
		{
			Loopback: true,
			Ports: []int{
				fixture.modelPort,
				fixture.allowedPort,
			},
		},
		{CIDR: denyFixture.coveringPrefix},
		{CIDR: denyFixture.allowedPrefix6},
		{CIDR: denyFixture.allowedAddr6 + "/128"},
		{
			Host:  openCodeFilteredAllowedHost,
			Ports: []int{denyFixture.allowedPort},
		},
	}
	deny = []sandboxpolicy.NetworkAllowEntry{
		{
			CIDR:  denyFixture.allowedAddr + "/32",
			Ports: []int{denyFixture.deniedPort},
		},
		{
			CIDR:  denyFixture.allowedPrefix6,
			Ports: []int{denyFixture.deniedPort},
		},
		{Host: openCodeFilteredDenyHost},
	}
	return allow, deny
}

func newOpenCodeFilteredSmokeFixture(
	t *testing.T,
	cwd string,
) *openCodeFilteredSmokeFixture {
	t.Helper()
	fixture := &openCodeFilteredSmokeFixture{}
	fixture.allowedPort = newOpenCodeFilteredEchoPair(t)
	fixture.deniedPort = newOpenCodeFilteredEchoPair(t)
	fixture.deny = newOpenCodeFilteredDenyFixture(t)

	modelServer := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/v1/chat/completions":
			fixture.modelRequests.Add(1)
			require.Equal(t, http.MethodPost, request.Method)
			require.Equal(t, "Bearer test-key", request.Header.Get("Authorization"))
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer,
				"data: {\"id\":\"chatcmpl-filtered\",\"object\":\"chat.completion.chunk\","+
					"\"created\":1,\"model\":\"test-model\",\"choices\":[{\"index\":0,"+
					"\"delta\":{\"role\":\"assistant\",\"content\":\"provider-ok\"},"+
					"\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(writer,
				"data: {\"id\":\"chatcmpl-filtered\",\"object\":\"chat.completion.chunk\","+
					"\"created\":1,\"model\":\"test-model\",\"choices\":[{\"index\":0,"+
					"\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(writer, "data: [DONE]\n\n")
		case "/models.json":
			fixture.modelsRequests.Add(1)
			http.Error(writer, "model metadata fetch must be disabled",
				http.StatusTeapot)
		case "/.well-known/opencode":
			fixture.authRequests.Add(1)
			http.Error(writer, "stored well-known auth must be replaced",
				http.StatusTeapot)
		case "/api/config":
			fixture.accountRequests.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer,
				`{"config":{"provider":{"test":{"options":{"baseURL":"https://opaque.invalid"}}}}}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(modelServer.Close)
	modelURL, err := url.Parse(modelServer.URL)
	require.NoError(t, err)
	fixture.modelPort, err = strconv.Atoi(modelURL.Port())
	require.NoError(t, err)
	fixture.modelBaseURL = fmt.Sprintf(
		"http://%s:%d/v1",
		sandboxpolicy.FilteredNetworkHostLoopbackName, fixture.modelPort)

	config := map[string]any{
		"enabled_providers": []string{"test"},
		"provider": map[string]any{
			"test": map[string]any{
				"npm":       "@ai-sdk/openai-compatible",
				"whitelist": []string{"test-model"},
				"models": map[string]any{
					"test-model": map[string]any{
						"id":   "test-model",
						"name": "Filtered smoke model",
						"limit": map[string]int{
							"context": 100_000,
							"output":  10_000,
						},
					},
				},
				"options": map[string]string{
					"baseURL": fixture.modelBaseURL,
					"apiKey":  "test-key",
				},
			},
		},
	}
	configJSON, err := json.Marshal(config)
	require.NoError(t, err)
	var hostileConfig map[string]any
	require.NoError(t, json.Unmarshal(configJSON, &hostileConfig))
	hostileProvider := hostileConfig["provider"].(map[string]any)["test"].(map[string]any)
	hostileModel := hostileProvider["models"].(map[string]any)["test-model"].(map[string]any)
	hostileModel["provider"] = map[string]string{
		"npm": "file:///tmp/opaque-provider.js",
	}
	hostileModelJSON, err := json.Marshal(hostileConfig)
	require.NoError(t, err)
	fixture.hostileModelConfig = string(hostileModelJSON)
	fixture.environment = []sandboxpolicy.EnvironmentEntry{
		{Name: "OPENCODE_CONFIG_CONTENT", Value: string(configJSON)},
		{
			Name: "OPENCODE_MODELS_URL",
			Value: fmt.Sprintf("http://%s:%d/models.json",
				sandboxpolicy.FilteredNetworkHostLoopbackName, fixture.modelPort),
		},
	}

	helperJSON, err := json.Marshal(openCodeFilteredToolHelperConfig{
		Allowed: net.JoinHostPort(sandboxpolicy.FilteredNetworkHostLoopbackName,
			strconv.Itoa(fixture.allowedPort)),
		Denied: net.JoinHostPort(sandboxpolicy.FilteredNetworkHostLoopbackName,
			strconv.Itoa(fixture.deniedPort)),
		Deny: &openCodeFilteredDenyProbeIR{
			AllowedAddr:   fixture.deny.allowedAddr,
			AdjacentAddr:  fixture.deny.adjacentAddr,
			AllowedAddr6:  fixture.deny.allowedAddr6,
			AdjacentAddr6: fixture.deny.adjacentAddr6,
			AllowedPort:   fixture.deny.allowedPort,
			DeniedPort:    fixture.deny.deniedPort,
		},
	})
	require.NoError(t, err)
	fixture.helperConfig = string(helperJSON)

	// Plant a hostile value in every loader source that the filtered launch
	// promises to suppress. The real OpenCode server must still start, route
	// exclusively through the inline provider, skip model/auth discovery, and
	// leave the external plugin marker absent.
	projectConfig := filepath.Join(cwd, "opencode.json")
	require.NoError(t, os.WriteFile(projectConfig,
		[]byte("{ this project config must not parse"), 0o600))
	customConfig := filepath.Join(cwd, "hostile-custom-config.json")
	require.NoError(t, os.WriteFile(customConfig,
		[]byte("{ this custom config must not parse"), 0o600))
	customConfigDir := filepath.Join(cwd, "hostile-config-dir")
	require.NoError(t, os.MkdirAll(customConfigDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(customConfigDir, "opencode.json"),
		[]byte("{ this custom config directory must not parse"), 0o600))
	t.Setenv("OPENCODE_CONFIG", customConfig)
	t.Setenv("OPENCODE_CONFIG_DIR", customConfigDir)

	fixture.pluginMarker = filepath.Join(cwd, "hostile-plugin-ran")
	pluginPath := filepath.Join(cwd, "hostile-plugin.js")
	pluginSource := fmt.Sprintf(
		"await Bun.write(%q, \"ran\");\nexport default async () => ({});\n",
		fixture.pluginMarker)
	require.NoError(t, os.WriteFile(pluginPath, []byte(pluginSource), 0o600))
	ambientConfig := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "opencode")
	globalConfigJSON, err := json.Marshal(map[string]any{
		"plugin": []string{"file://" + pluginPath},
		"provider": map[string]any{
			"test": map[string]any{
				"models": map[string]any{
					"test-model": map[string]any{
						"provider": map[string]string{
							"npm": "file://" + pluginPath,
						},
					},
				},
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(ambientConfig, "opencode.json"), globalConfigJSON, 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(os.Getenv("HOME"), ".opencode", "opencode.json"),
		globalConfigJSON, 0o600))

	ambientData := filepath.Join(os.Getenv("XDG_DATA_HOME"), "opencode")
	authJSON, err := json.Marshal(map[string]any{
		fmt.Sprintf("http://%s:%d",
			sandboxpolicy.FilteredNetworkHostLoopbackName,
			fixture.modelPort): map[string]string{
			"type":  "wellknown",
			"key":   "TEST_TOKEN",
			"token": "stored-token-must-not-load",
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(ambientData, "auth.json"), authJSON, 0o600))
	return fixture
}

func newOpenCodeFilteredEchoPair(t *testing.T) int {
	t.Helper()
	tcpListener, err := net.ListenTCP("tcp4", &net.TCPAddr{
		IP: net.ParseIP("127.0.0.1"),
	})
	require.NoError(t, err)
	port := tcpListener.Addr().(*net.TCPAddr).Port
	udpConnection, err := net.ListenUDP("udp4", &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: port,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = tcpListener.Close()
		_ = udpConnection.Close()
	})
	go func() {
		for {
			connection, acceptErr := tcpListener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = connection.Close() }()
				_, _ = io.Copy(connection, connection)
			}()
		}
	}()
	go func() {
		buffer := make([]byte, 2048)
		for {
			n, remote, readErr := udpConnection.ReadFromUDP(buffer)
			if readErr != nil {
				return
			}
			_, _ = udpConnection.WriteToUDP(buffer[:n], remote)
		}
	}()
	return port
}

// TestOpenCodeFilteredNetworkToolHelper runs as the real OpenCode bash tool,
// inside the filtered server's namespace. The environment gate prevents a
// normal package test from turning into an accidental network integration.
func TestOpenCodeFilteredNetworkToolHelper(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv(openCodeFilteredToolHelperEnv))
	if raw == "" {
		t.Skip("helper executes only through TestOpenCodeFilteredNetworkExecutorSmoke")
	}
	var config openCodeFilteredToolHelperConfig
	require.NoError(t, json.Unmarshal([]byte(raw), &config))

	connection, err := net.DialTimeout("tcp4", config.Allowed, 2*time.Second)
	require.NoError(t, err)
	require.NoError(t, connection.SetDeadline(time.Now().Add(2*time.Second)))
	payload := []byte("opencode-filtered-tcp")
	_, err = connection.Write(payload)
	require.NoError(t, err)
	reply := make([]byte, len(payload))
	_, err = io.ReadFull(connection, reply)
	require.NoError(t, err)
	require.Equal(t, payload, reply)
	require.NoError(t, connection.Close())

	remote, err := net.ResolveUDPAddr("udp4", config.Allowed)
	require.NoError(t, err)
	udpConnection, err := net.DialUDP("udp4", nil, remote)
	require.NoError(t, err)
	require.NoError(t, udpConnection.SetDeadline(time.Now().Add(2*time.Second)))
	payload = []byte("opencode-filtered-udp")
	_, err = udpConnection.Write(payload)
	require.NoError(t, err)
	reply = make([]byte, len(payload))
	_, err = io.ReadFull(udpConnection, reply)
	require.NoError(t, err)
	require.Equal(t, payload, reply)
	require.NoError(t, udpConnection.Close())

	deniedTCP, err := net.DialTimeout("tcp4", config.Denied, 2*time.Second)
	if deniedTCP != nil {
		_ = deniedTCP.Close()
	}
	require.Error(t, err, "unauthorised TCP port must be denied")

	deniedRemote, err := net.ResolveUDPAddr("udp4", config.Denied)
	require.NoError(t, err)
	deniedUDP, err := net.DialUDP("udp4", nil, deniedRemote)
	require.NoError(t, err)
	defer deniedUDP.Close()
	require.NoError(t, deniedUDP.SetDeadline(time.Now().Add(500*time.Millisecond)))
	_, writeErr := deniedUDP.Write([]byte("must-not-arrive"))
	if writeErr != nil {
		require.ErrorIs(t, writeErr, syscall.EPERM,
			"immediate UDP denial must be the packet policy rejection")
	} else {
		_, readErr := deniedUDP.Read(make([]byte, 64))
		require.Error(t, readErr, "unauthorised UDP port returned traffic")
	}
	fmt.Println("opencode-filtered-network-helper PASS")

	require.NotNil(t, config.Deny,
		"the deny probe plan is mandatory for this boundary")
	runOpenCodeFilteredDenyProbes(t, *config.Deny)
	fmt.Println(openCodeFilteredDenyHelperTag)
}

// runOpenCodeFilteredDenyProbes proves deny precedence from inside the real
// OpenCode bash tool. The static cases cover both overlap directions, and the
// DNS case proves a denied name cuts an address the CIDR allow otherwise
// permits.
func runOpenCodeFilteredDenyProbes(
	t *testing.T,
	plan openCodeFilteredDenyProbeIR,
) {
	t.Helper()
	allowed4 := net.JoinHostPort(plan.AllowedAddr, strconv.Itoa(plan.AllowedPort))
	denied4 := net.JoinHostPort(plan.AllowedAddr, strconv.Itoa(plan.DeniedPort))
	adjacent4 := net.JoinHostPort(plan.AdjacentAddr, strconv.Itoa(plan.DeniedPort))
	allowed6 := net.JoinHostPort(plan.AllowedAddr6, strconv.Itoa(plan.AllowedPort))
	denied6 := net.JoinHostPort(plan.AllowedAddr6, strconv.Itoa(plan.DeniedPort))
	adjacent6 := net.JoinHostPort(plan.AdjacentAddr6, strconv.Itoa(plan.AllowedPort))

	// IPv4: a narrow port-scoped deny defeats the broad covering allow, and is
	// not widened onto the rest of that allow.
	openCodeFilteredTCPRoundTrip(t, "tcp4", allowed4)
	openCodeFilteredUDPRoundTrip(t, "udp4", allowed4)
	openCodeFilteredTCPDenied(t, "tcp4", denied4)
	openCodeFilteredUDPDenied(t, "udp4", denied4)
	openCodeFilteredTCPRoundTrip(t, "tcp4", adjacent4)

	// IPv6: the overlap runs the other way. A broad prefix deny defeats the
	// narrow /128 allow authored for the same address.
	openCodeFilteredTCPRoundTrip(t, "tcp6", allowed6)
	openCodeFilteredUDPRoundTrip(t, "udp6", allowed6)
	openCodeFilteredTCPDenied(t, "tcp6", denied6)
	openCodeFilteredUDPDenied(t, "udp6", denied6)
	openCodeFilteredTCPRoundTrip(t, "tcp6", adjacent6)

	// The allowed name answers and its address is reachable before any denied
	// name is observed.
	openCodeFilteredDNSAllowed(t, openCodeFilteredAllowedHost, "ip4")
	openCodeFilteredTCPRoundTrip(t, "tcp4", net.JoinHostPort(
		openCodeFilteredAllowedHost, strconv.Itoa(plan.AllowedPort)))

	// The denied name is refused at the broker, and the negative lease it
	// installs defeats both the covering CIDR allow and the earlier positive
	// lease on the shared address. Each assertion refreshes the lease first so
	// it measures the cut rather than racing the fixture TTL.
	openCodeFilteredDNSDenied(t, openCodeFilteredDenyHost, "ip4")
	openCodeFilteredDNSDenied(t, openCodeFilteredDenyHost, "ip6")
	openCodeFilteredDNSDenied(t, openCodeFilteredDenyHost, "ip4")
	openCodeFilteredTCPDenied(t, "tcp4", adjacent4)
	openCodeFilteredDNSDenied(t, openCodeFilteredDenyHost, "ip6")
	openCodeFilteredTCPDenied(t, "tcp6", adjacent6)

	// Negative authority is TTL-bound: expiry restores the CIDR baseline.
	time.Sleep(openCodeFilteredDNSMappingTTL + time.Second)
	openCodeFilteredTCPRoundTrip(t, "tcp4", adjacent4)
}

func openCodeFilteredTCPRoundTrip(t *testing.T, network, address string) {
	t.Helper()
	connection, err := net.DialTimeout(network, address, openCodeFilteredProbeWait)
	require.NoErrorf(t, err, "%s allow %s", network, address)
	defer func() { _ = connection.Close() }()
	require.NoError(t, connection.SetDeadline(
		time.Now().Add(openCodeFilteredProbeWait)))
	payload := []byte("opencode-deny-tcp")
	_, err = connection.Write(payload)
	require.NoError(t, err)
	reply := make([]byte, len(payload))
	_, err = io.ReadFull(connection, reply)
	require.NoErrorf(t, err, "%s allow %s", network, address)
	assert.Equal(t, payload, reply)
}

func openCodeFilteredTCPDenied(t *testing.T, network, address string) {
	t.Helper()
	connection, err := net.DialTimeout(network, address, openCodeFilteredProbeWait)
	if err == nil {
		_ = connection.Close()
	}
	require.Errorf(t, err, "%s deny %s", network, address)
}

func openCodeFilteredUDPRoundTrip(t *testing.T, network, address string) {
	t.Helper()
	remote, err := net.ResolveUDPAddr(network, address)
	require.NoError(t, err)
	connection, err := net.DialUDP(network, nil, remote)
	require.NoError(t, err)
	defer func() { _ = connection.Close() }()
	require.NoError(t, connection.SetDeadline(
		time.Now().Add(openCodeFilteredProbeWait)))
	payload := []byte("opencode-deny-udp")
	_, err = connection.Write(payload)
	require.NoError(t, err)
	reply := make([]byte, len(payload))
	_, err = io.ReadFull(connection, reply)
	require.NoErrorf(t, err, "%s allow %s", network, address)
	assert.Equal(t, payload, reply)
}

func openCodeFilteredUDPDenied(t *testing.T, network, address string) {
	t.Helper()
	remote, err := net.ResolveUDPAddr(network, address)
	require.NoError(t, err)
	connection, err := net.DialUDP(network, nil, remote)
	require.NoError(t, err)
	defer func() { _ = connection.Close() }()
	require.NoError(t, connection.SetDeadline(
		time.Now().Add(openCodeFilteredProbeWait)))
	_, err = connection.Write([]byte("must-not-arrive"))
	if err != nil {
		require.ErrorIsf(t, err, syscall.EPERM, "%s deny %s", network, address)
		return
	}
	_, err = connection.Read(make([]byte, 64))
	require.Errorf(t, err, "%s deny %s", network, address)
}

func openCodeFilteredDNSAllowed(t *testing.T, host, network string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(
		context.Background(), openCodeFilteredProbeWait)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, network, host)
	require.NoErrorf(t, err, "DNS allow %s (%s)", host, network)
	require.NotEmptyf(t, addresses, "DNS allow %s (%s)", host, network)
}

func openCodeFilteredDNSDenied(t *testing.T, host, network string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(
		context.Background(), openCodeFilteredProbeWait)
	defer cancel()
	_, err := net.DefaultResolver.LookupNetIP(ctx, network, host)
	require.Errorf(t, err, "DNS deny %s (%s)", host, network)
}

func copyOpenCodeLayerSmokeExecutable(t *testing.T, source, destination string) {
	t.Helper()
	sourceFile, err := os.Open(source)
	require.NoError(t, err)
	defer sourceFile.Close()
	destinationFile, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	require.NoError(t, err)
	_, err = io.Copy(destinationFile, sourceFile)
	require.NoError(t, err)
	require.NoError(t, destinationFile.Close())
}

func logOpenCodeLayerSmokeServerLogs(t *testing.T, dir string) {
	t.Helper()
	dirFile, err := openOpenCodeLayerSmokeLogDir(dir)
	if err != nil {
		t.Logf("read OpenCode smoke log directory %s: %v", dir, err)
		return
	}
	defer dirFile.Close()
	entries, err := dirFile.ReadDir(-1)
	if err != nil {
		t.Logf("enumerate OpenCode smoke log directory %s: %v", dir, err)
		return
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		raw, readErr := readOpenCodeLayerSmokeLogTailAt(
			int(dirFile.Fd()), entry.Name())
		if readErr != nil {
			t.Logf("read OpenCode smoke log %s: %v", path, readErr)
			continue
		}
		t.Logf("OpenCode smoke log %s:\n%s", path, raw)
	}
}

func openOpenCodeLayerSmokeLogDir(path string) (*os.File, error) {
	fd, err := unix.Open(
		path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func readOpenCodeLayerSmokeLogTailAt(dirFD int, name string) ([]byte, error) {
	fd, err := unix.Openat(
		dirFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("not a regular file")
	}
	const limit = int64(64 << 10)
	if stat.Size > limit {
		if _, err := file.Seek(stat.Size-limit, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(io.LimitReader(file, limit))
}

func TestReadOpenCodeLayerSmokeLogTailRefusesSpecialFilesAndBounds(t *testing.T) {
	dir := t.TempDir()
	large := filepath.Join(dir, "large.log")
	require.NoError(t, os.WriteFile(large, []byte(
		strings.Repeat("a", 64<<10)+strings.Repeat("b", 64<<10)), 0o600))
	dirFile, err := openOpenCodeLayerSmokeLogDir(dir)
	require.NoError(t, err)
	defer dirFile.Close()
	raw, err := readOpenCodeLayerSmokeLogTailAt(int(dirFile.Fd()), "large.log")
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("b", 64<<10), string(raw))

	symlink := filepath.Join(dir, "symlink.log")
	require.NoError(t, os.Symlink(large, symlink))
	_, err = readOpenCodeLayerSmokeLogTailAt(int(dirFile.Fd()), "symlink.log")
	require.Error(t, err)

	fifo := filepath.Join(dir, "fifo.log")
	require.NoError(t, unix.Mkfifo(fifo, 0o600))
	_, err = readOpenCodeLayerSmokeLogTailAt(int(dirFile.Fd()), "fifo.log")
	require.ErrorContains(t, err, "not a regular file")

	dirSymlink := filepath.Join(t.TempDir(), "log")
	require.NoError(t, os.Symlink(dir, dirSymlink))
	_, err = openOpenCodeLayerSmokeLogDir(dirSymlink)
	require.Error(t, err)
}

func startOpenCodeLayerSmokeAgentd(t *testing.T, tclaudeBinary, socket string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, tclaudeBinary, "agentd", "serve",
		"--socket", socket, "--no-tray", "--no-print-human-token")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	var stopped bool
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		_ = cmd.Wait()
	}
	t.Cleanup(stop)
	require.Eventuallyf(t, func() bool {
		return agentipc.SocketReachable(socket)
	}, 15*time.Second, 25*time.Millisecond, "agentd did not become reachable")
	return stop
}

func startOpenCodeLayerSmokeAttach(
	t *testing.T,
	tclaudeBinary string,
	executable string,
	runtime db.OpenCodeRuntime,
	convID, cwd string,
	environment []sandboxpolicy.EnvironmentEntry,
) func() {
	t.Helper()
	cmd := exec.Command(tclaudeBinary,
		opencodeapi.UnixAttachShimMode,
		strconv.Itoa(runtime.PID),
		runtime.ControlSocketPath,
		strconv.FormatInt(runtime.ControlSocketDevice, 10),
		strconv.FormatInt(runtime.ControlSocketInode, 10),
		runtime.ServerURL,
		"--",
		executable, "attach", opencodeapi.AttachURLPlaceholder,
		"--dir", cwd, "--session", convID)
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"OPENCODE_SERVER_USERNAME="+openCodeServerUsername,
		"OPENCODE_SERVER_PASSWORD="+runtime.Password,
	)
	for _, entry := range environment {
		cmd.Env = append(cmd.Env, entry.Name+"="+entry.Value)
	}
	terminal, err := pty.Start(cmd)
	require.NoError(t, err)
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, terminal)
		close(copied)
	}()
	var stopped bool
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = terminal.Close()
		select {
		case <-copied:
		case <-time.After(time.Second):
		}
	}
	t.Cleanup(stop)
	require.Eventuallyf(t, func() bool {
		for _, pid := range openCodeLayerSmokeProcessTree(cmd.Process.Pid) {
			raw, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
			if readErr == nil && strings.Contains(strings.ToLower(string(raw)), "opencode") {
				return true
			}
		}
		return false
	}, openCodeLayerSmokeAttachProbe, 25*time.Millisecond,
		"OpenCode attach did not start behind the Unix shim")
	rawEnvironment, err := os.ReadFile(
		filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "environ"))
	require.NoError(t, err)
	for _, entry := range environment {
		assert.Contains(t, string(rawEnvironment), entry.Name+"="+entry.Value+"\x00",
			"attach and server must receive the same private XDG allocation")
	}
	return stop
}

func runOpenCodeLayerSmokeShell(
	t *testing.T,
	runtime db.OpenCodeRuntime,
	command string,
) string {
	t.Helper()
	body, err := requestOpenCodeLayerSmokeShell(
		runtime, command, openCodeLayerSmokeShellWait, openCodeLayerSmokeShellRetry)
	require.NoError(t, err)
	var result struct {
		Parts []struct {
			Type  string `json:"type"`
			State struct {
				Status string `json:"status"`
				Output string `json:"output"`
			} `json:"state"`
		} `json:"parts"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	for _, part := range result.Parts {
		if part.Type == "tool" {
			require.Equal(t, "completed", part.State.Status)
			return part.State.Output
		}
	}
	t.Fatalf("OpenCode shell response contained no tool part: %s", body)
	return ""
}

func requestOpenCodeLayerSmokeShell(
	runtime db.OpenCodeRuntime,
	command string,
	timeout time.Duration,
	retryInterval time.Duration,
) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return requestOpenCodeLayerSmokeShellWithContext(
		ctx, runtime, command, timeout, retryInterval)
}

func requestOpenCodeLayerSmokeShellWithContext(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	command string,
	timeout time.Duration,
	retryInterval time.Duration,
) ([]byte, error) {
	endpoint := runtime.ServerURL + "/session/" + url.PathEscape(runtime.ConvID) +
		"/shell?directory=" + url.QueryEscape(runtime.Cwd)
	// The preceding model-request assertion proves prompt processing started,
	// not that OpenCode has released the session for the shell request.
	var lastBusyBody []byte
	for {
		request, err := openCodeRequest(http.MethodPost, endpoint, runtime, map[string]any{
			"agent":   "build",
			"command": command,
		})
		if err != nil {
			return nil, err
		}
		request = request.WithContext(ctx)
		response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
		if err != nil {
			if ctx.Err() != nil && lastBusyBody != nil {
				return nil, fmt.Errorf(
					"OpenCode shell remained busy for %s; last response: %s",
					timeout, lastBusyBody)
			}
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		_ = response.Body.Close()
		if readErr != nil {
			if ctx.Err() != nil && lastBusyBody != nil {
				return nil, fmt.Errorf(
					"OpenCode shell remained busy for %s; last response: %s",
					timeout, lastBusyBody)
			}
			return nil, readErr
		}
		if response.StatusCode == http.StatusOK {
			if ctx.Err() != nil {
				if lastBusyBody != nil {
					return nil, fmt.Errorf(
						"OpenCode shell remained busy for %s; last response: %s",
						timeout, lastBusyBody)
				}
				return nil, ctx.Err()
			}
			return body, nil
		}
		if response.StatusCode != http.StatusConflict ||
			!openCodeLayerSmokeSessionBusy(body) {
			return nil, fmt.Errorf(
				"OpenCode shell response: status %d: %s", response.StatusCode, body)
		}
		lastBusyBody = body
		timer := time.NewTimer(retryInterval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, fmt.Errorf(
				"OpenCode shell remained busy for %s; last response: %s",
				timeout, lastBusyBody)
		}
	}
}

func openCodeLayerSmokeSessionBusy(body []byte) bool {
	var failure struct {
		Tag string `json:"_tag"`
	}
	return json.Unmarshal(body, &failure) == nil && failure.Tag == "SessionBusyError"
}

func TestRequestOpenCodeLayerSmokeShellRetriesSessionBusy(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		if requests.Add(1) <= 2 {
			writer.WriteHeader(http.StatusConflict)
			_, _ = writer.Write([]byte(`{"_tag":"SessionBusyError","message":"busy"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"parts":[{"type":"tool","state":{"status":"completed","output":"ok"}}]}`))
	}))
	t.Cleanup(server.Close)

	body, err := requestOpenCodeLayerSmokeShell(db.OpenCodeRuntime{
		ConvID:    "conv",
		ServerURL: server.URL,
		Password:  "password",
		PID:       os.Getpid(),
		Cwd:       t.TempDir(),
	}, "true", time.Second, time.Millisecond)
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"parts":[{"type":"tool","state":{"status":"completed","output":"ok"}}]}`,
		string(body))
	assert.Equal(t, int32(3), requests.Load())
}

func TestRequestOpenCodeLayerSmokeShellBusyTimeoutIncludesLastBody(t *testing.T) {
	const busyBody = `{"_tag":"SessionBusyError","message":"still busy"}`
	var requests atomic.Int32
	secondRequest := make(chan struct{})
	secondRelease := make(chan struct{})
	releaseSecond := sync.OnceFunc(func() { close(secondRelease) })
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		if requests.Add(1) == 1 {
			writer.WriteHeader(http.StatusConflict)
			_, _ = writer.Write([]byte(busyBody))
			return
		}
		close(secondRequest)
		<-secondRelease
		time.Sleep(100 * time.Millisecond)
		_, _ = writer.Write([]byte(`{"parts":[]}`))
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(releaseSecond)
	runtime := db.OpenCodeRuntime{
		ConvID:    "conv",
		ServerURL: server.URL,
		Password:  "password",
		PID:       os.Getpid(),
		Cwd:       t.TempDir(),
	}
	result := make(chan error, 1)
	go func() {
		_, err := requestOpenCodeLayerSmokeShellWithContext(
			ctx, runtime, "true", 10*time.Millisecond, time.Millisecond)
		result <- err
	}()
	// Start the timeout only after request two reaches the server. Runner
	// scheduling before that boundary is not behavior this assertion covers.
	select {
	case <-secondRequest:
	case <-time.After(time.Second):
		t.Fatal("second OpenCode shell request did not reach the test server")
	}
	time.Sleep(10 * time.Millisecond)
	cancel()
	releaseSecond()

	var err error
	select {
	case err = <-result:
	case <-time.After(time.Second):
		t.Fatal("OpenCode shell request did not stop after context cancellation")
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), busyBody)
	assert.Equal(t, int32(2), requests.Load())
}

func TestRequestOpenCodeLayerSmokeShellRejectsOtherStatusesImmediately(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "different conflict",
			status: http.StatusConflict,
			body:   `{"_tag":"OtherError","message":"not retryable"}`,
		},
		{
			name:   "server error",
			status: http.StatusInternalServerError,
			body:   `{"_tag":"SessionBusyError","message":"wrong status"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter,
				_ *http.Request,
			) {
				requests.Add(1)
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)

			_, err := requestOpenCodeLayerSmokeShell(db.OpenCodeRuntime{
				ConvID:    "conv",
				ServerURL: server.URL,
				Password:  "password",
				PID:       os.Getpid(),
				Cwd:       t.TempDir(),
			}, "true", time.Second, time.Millisecond)
			require.Error(t, err)
			assert.Contains(t, err.Error(), strconv.Itoa(test.status))
			assert.Equal(t, int32(1), requests.Load())
		})
	}
}

func openCodeLayerSmokeProcessTree(rootPID int) []int {
	result := []int{rootPID}
	seen := map[int]bool{rootPID: true}
	for cursor := 0; cursor < len(result) && len(result) < 64; cursor++ {
		pid := result[cursor]
		children, err := os.ReadFile(filepath.Join(
			"/proc", strconv.Itoa(pid), "task", strconv.Itoa(pid), "children"))
		if err != nil {
			continue
		}
		for _, raw := range strings.Fields(string(children)) {
			child, err := strconv.Atoi(raw)
			if err == nil && child > 1 && !seen[child] {
				seen[child] = true
				result = append(result, child)
			}
		}
	}
	return result
}
