//go:build linux

package session

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const (
	filteredGatewaySmokeEnv          = "TCLAUDE_FILTERED_NETWORK_SMOKE"
	filteredGatewayHelperEnv         = "TCLAUDE_FILTERED_NETWORK_HELPER"
	filteredGatewayAllowedAddrEnv    = "TCLAUDE_FILTERED_ALLOWED_ADDR"
	filteredGatewayAdjacentAddrEnv   = "TCLAUDE_FILTERED_ADJACENT_ADDR"
	filteredGatewayAllowedPrefixEnv  = "TCLAUDE_FILTERED_ALLOWED_PREFIX"
	filteredGatewayAllowedAddr6Env   = "TCLAUDE_FILTERED_ALLOWED_ADDR6"
	filteredGatewayAdjacentAddr6Env  = "TCLAUDE_FILTERED_ADJACENT_ADDR6"
	filteredGatewayAllowedPrefix6Env = "TCLAUDE_FILTERED_ALLOWED_PREFIX6"
	filteredGatewayAllowedPortEnv    = "TCLAUDE_FILTERED_ALLOWED_PORT"
	filteredGatewayDeniedPortEnv     = "TCLAUDE_FILTERED_DENIED_PORT"
	filteredGatewayLoopbackPortEnv   = "TCLAUDE_FILTERED_LOOPBACK_PORT"
	filteredGatewayLoopbackDenyEnv   = "TCLAUDE_FILTERED_LOOPBACK_DENIED_PORT"
	filteredGatewayReadyPathEnv      = "TCLAUDE_FILTERED_READY_PATH"
	filteredGatewayHoldEnv           = "TCLAUDE_FILTERED_HOLD"
	filteredGatewayDNSSmokeEnv       = "TCLAUDE_FILTERED_DNS_HELPER"
	filteredGatewayDenySmokeEnv      = "TCLAUDE_FILTERED_DENY_HELPER"
	filteredGatewayLocalBaselineEnv  = "TCLAUDE_FILTERED_LOCAL_BASELINE"
	filteredGatewayTclaudeBinaryEnv  = "TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY"
	filteredGatewayConnectionTimeout = 900 * time.Millisecond
	filteredGatewayExactHost         = "exact-host.filtered.test"
	filteredGatewaySiblingHost       = "sibling.filtered.test"
	filteredGatewayExactDomain       = "exact-domain.filtered.test"
	filteredGatewayExactDomainChild  = "child.exact-domain.filtered.test"
	filteredGatewayTreeDomain        = "tree.filtered.test"
	filteredGatewayTreeDomainChild   = "child.tree.filtered.test"
	filteredGatewaySuffixConfusion   = "tree.filtered.test.attacker.invalid"
)

func TestFilteredSmokeExecutableNameMatchesPastaVariantsOnly(t *testing.T) {
	assert.True(t, filteredSmokeExecutableNameMatches("pasta", "pasta"))
	assert.True(t, filteredSmokeExecutableNameMatches("pasta.avx2", "pasta"))
	assert.False(t, filteredSmokeExecutableNameMatches("pasta-helper", "pasta"))
	assert.False(t, filteredSmokeExecutableNameMatches("passt", "pasta"))
}

// TestTclaudeLayerFilteredNetworkSmoke is the named executing CI boundary for
// the M2b Claude/Codex tclaude-layer cells. The runner supplies two live
// adjacent targets from a separate network namespace, so allowed CIDR+port,
// denied port, denied adjacent CIDR, TCP, and UDP are all observable outcomes.
// It also exercises synthetic host loopback and kills the supervised pasta
// process to prove fail-closed sandbox teardown.
func TestTclaudeLayerFilteredNetworkSmoke(t *testing.T) {
	runTclaudeLayerFilteredNetworkSmoke(t, "")
}

// TestTclaudeLayerFilteredNetworkDNSSmoke is the named executing M2c boundary.
// It runs the same real Claude/Codex tclaude-layer launch path as M2b, then
// proves exact-host versus sibling, exact-domain versus child,
// domain-with-subdomains, suffix confusion, port bounds, shared-IP reuse, and
// expiry of the kernel lease at the fixture DNS TTL.
func TestTclaudeLayerFilteredNetworkDNSSmoke(t *testing.T) {
	runTclaudeLayerFilteredNetworkSmoke(t, "allow-dns")
}

// TestTclaudeLayerFilteredNetworkDenySmoke is the named CI boundary for
// CIDR/port deny precedence in both overlap directions, across real Claude and
// Codex tclaude-layer launches and IPv4/IPv6 TCP/UDP.
func TestTclaudeLayerFilteredNetworkDenySmoke(t *testing.T) {
	runTclaudeLayerFilteredNetworkSmoke(t, "deny-static")
}

// TestTclaudeLayerFilteredNetworkDNSDenySmoke is the named CI boundary for
// DNS negative leases, deny-wins shared-IP ordering, established-flow cuts,
// port-scoped names, label boundaries, expiry, and refresh.
func TestTclaudeLayerFilteredNetworkDNSDenySmoke(t *testing.T) {
	runTclaudeLayerFilteredNetworkSmoke(t, "deny-dns")
}

func runTclaudeLayerFilteredNetworkSmoke(t *testing.T, smokeKind string) {
	t.Helper()
	dnsSmoke := smokeKind == "allow-dns" || smokeKind == "deny-dns"
	denySmoke := smokeKind == "deny-static" || smokeKind == "deny-dns"
	if os.Getenv(filteredGatewaySmokeEnv) != "1" {
		t.Skip("set TCLAUDE_FILTERED_NETWORK_SMOKE=1 on the executing Linux CI boundary")
	}
	tclaudeBinary := strings.TrimSpace(os.Getenv(filteredGatewayTclaudeBinaryEnv))
	require.NotEmpty(t, tclaudeBinary)
	tclaudeBinary, err := filepath.Abs(tclaudeBinary)
	require.NoError(t, err)

	allowedAddr := requireFilteredSmokeEnv(t, filteredGatewayAllowedAddrEnv)
	adjacentAddr := requireFilteredSmokeEnv(t, filteredGatewayAdjacentAddrEnv)
	allowedPrefix := requireFilteredSmokeEnv(t, filteredGatewayAllowedPrefixEnv)
	allowedAddr6 := requireFilteredSmokeEnv(t, filteredGatewayAllowedAddr6Env)
	adjacentAddr6 := requireFilteredSmokeEnv(t, filteredGatewayAdjacentAddr6Env)
	allowedPrefix6 := requireFilteredSmokeEnv(t, filteredGatewayAllowedPrefix6Env)
	allowedPort := requireFilteredSmokePort(t, filteredGatewayAllowedPortEnv)
	deniedPort := requireFilteredSmokePort(t, filteredGatewayDeniedPortEnv)

	bwrapBinary, _, err := ResolveTclaudeLayer(sandboxpolicy.NetworkFiltered)
	require.NoError(t, err)
	executables, err := resolveFilteredNetworkExecutables()
	require.NoError(t, err)

	previousRelay := tclaudeLayerRelayPrefix
	tclaudeLayerRelayPrefix = func() string {
		return clcommon.ShellQuoteArg(tclaudeBinary) +
			" session " + tclaudeLayerWinchRelayCommand
	}
	t.Cleanup(func() { tclaudeLayerRelayPrefix = previousRelay })

	hostAllowedPort := startFilteredSmokeLoopbackEcho(t)
	hostDeniedPort := startFilteredSmokeLoopbackEcho(t)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	smokeBase := filepath.Join(home, ".cache")
	require.NoError(t, os.MkdirAll(smokeBase, 0o700))
	root, err := os.MkdirTemp(smokeBase, "tclaude-filtered-smoke-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	smokeHome := filepath.Join(root, "home")
	helperDir := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(smokeHome, 0o700))
	require.NoError(t, os.MkdirAll(helperDir, 0o700))
	t.Setenv("HOME", smokeHome)
	prepareStackedSmokeControlPlane(t)
	helperBinary := filepath.Join(helperDir, "filtered-smoke-helper")
	copyTestBinary(t, os.Args[0], helperBinary)

	rules := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{CIDR: allowedPrefix, Ports: []int{allowedPort}},
			{CIDR: allowedPrefix6, Ports: []int{allowedPort}},
			{Loopback: true, Ports: []int{hostAllowedPort}},
		},
	}
	switch smokeKind {
	case "deny-static":
		rules = sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{
				{CIDR: filteredSmokeCoveringPrefix(t, allowedAddr, adjacentAddr)},
				{CIDR: allowedAddr6 + "/128"},
			},
			Deny: []sandboxpolicy.NetworkAllowEntry{
				{CIDR: allowedAddr + "/32", Ports: []int{deniedPort}},
				{CIDR: allowedPrefix6, Ports: []int{deniedPort}},
			},
		}
	case "deny-dns":
		rules = sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{
				{CIDR: filteredSmokeCoveringPrefix(t, allowedAddr, adjacentAddr)},
				{CIDR: filteredSmokeCoveringPrefix(t, allowedAddr6, adjacentAddr6)},
				{Host: filteredGatewaySiblingHost},
			},
			Deny: []sandboxpolicy.NetworkAllowEntry{
				{Host: filteredGatewayExactHost},
				{Host: filteredGatewaySiblingHost, Ports: []int{deniedPort}},
				{Domain: filteredGatewayExactDomain},
				{Domain: filteredGatewayTreeDomain, IncludeSubdomains: true},
				{CIDR: adjacentAddr + "/32", Ports: []int{deniedPort}},
			},
		}
	case "allow-dns":
		rules.Allow = append(rules.Allow,
			sandboxpolicy.NetworkAllowEntry{
				Host: filteredGatewayExactHost, Ports: []int{allowedPort},
			},
			sandboxpolicy.NetworkAllowEntry{
				Domain: filteredGatewayExactDomain, Ports: []int{allowedPort},
			},
			sandboxpolicy.NetworkAllowEntry{
				Domain: filteredGatewayTreeDomain, IncludeSubdomains: true,
				Ports: []int{allowedPort},
			},
		)
	}
	axes := sandboxpolicy.ResolvedAxes{Network: rules}
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.Network = &rules

	wrapped := make(map[string]string, 2)
	for _, harnessName := range []string{harness.DefaultName, harness.CodexName} {
		t.Run(harnessName, func(t *testing.T) {
			h := harness.MustGet(harnessName)
			notices, validateErr := ValidateTclaudeLayerNetwork(
				h,
				snapshot.Effective,
				harness.ResolvedModelTransport{
					Model:            "m2b-smoke",
					Provider:         "ci-smoke",
					BaseURL:          fmt.Sprintf("http://%s:%d/v1", allowedAddr, allowedPort),
					ProviderResolved: true,
				},
			)
			require.NoError(t, validateErr)
			require.Len(t, notices, 1)
			assert.NotContains(t, notices[0].Detail, "provisional until")

			launchCaps, capsErr := harness.ResolveAccessEnforcement(
				h,
				sandboxpolicy.ImplementationTclaudeLayer,
				axes,
				TclaudeLayerLaunchOSSandbox(sandboxpolicy.NetworkFiltered),
				"",
			)
			require.NoError(t, capsErr)
			rendered, _, planErr := harness.PlanAccessEnforcement(axes, launchCaps)
			require.NoError(t, planErr)
			assert.Equal(t, sandboxpolicy.AccessModeList, rendered.Network.Mode)
			if denySmoke {
				assert.Empty(t, rendered.Network.Deny,
					"PR1 keeps production deny capability cells dark")
			}

			stateRoot := filepath.Join(smokeHome, "."+harnessName)
			require.NoError(t, os.MkdirAll(stateRoot, 0o700))
			spec, specErr := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
				HarnessName: harnessName,
				Cwd:         helperDir,
				Snapshot:    &snapshot,
				StateRoot:   stateRoot,
			})
			require.NoError(t, specErr)
			command, wrapErr := WrapTclaudeLayerSpec(
				bwrapBinary,
				spec,
				clcommon.ShellQuoteArg(helperBinary)+" -test.run=^TestFilteredNetworkGatewayHelper$",
			)
			require.NoError(t, wrapErr)
			wrapped[harnessName] = command

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
			cmd.Env = filteredSmokeHelperEnv(
				os.Environ(), allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6,
				allowedPort, deniedPort,
				hostAllowedPort, hostDeniedPort, "", false, dnsSmoke,
			)
			if denySmoke {
				cmd.Env = append(cmd.Env,
					filteredGatewayDenySmokeEnv+"="+smokeKind)
			}
			output, runErr := cmd.CombinedOutput()
			require.NoErrorf(t, runErr, "%s filtered smoke output:\n%s", harnessName, output)
			require.NoError(t, ctx.Err())
		})
	}

	if dnsSmoke {
		return
	}
	if denySmoke {
		runFilteredGatewayFailClosedSmoke(
			t, wrapped[harness.DefaultName], helperDir, root, executables.Pasta,
			allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6,
			allowedPort, deniedPort, hostAllowedPort, hostDeniedPort, smokeKind,
		)
		return
	}
	runFilteredLocalPresetSmoke(
		t, bwrapBinary, helperBinary, helperDir, smokeHome,
		allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6,
		allowedPort, deniedPort, hostAllowedPort, hostDeniedPort,
		"local-access", harness.Default(),
		sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{
				Loopback: true,
			}},
		},
		harness.ResolvedModelTransport{
			Model:            "local/llama",
			Provider:         "ollama",
			BaseURL:          fmt.Sprintf("http://%s:%d/v1", sandboxpolicy.FilteredNetworkHostLoopbackName, hostAllowedPort),
			ProviderResolved: true,
		},
	)
	localModelAPIRules := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Loopback: true},
			{Domain: "api.anthropic.com", Ports: []int{443}},
			{Domain: "api.openai.com", Ports: []int{443}},
		},
	}
	runFilteredLocalPresetSmoke(
		t, bwrapBinary, helperBinary, helperDir, smokeHome,
		allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6,
		allowedPort, deniedPort, hostAllowedPort, hostDeniedPort,
		"local-model-apis-claude", harness.Default(), localModelAPIRules,
		harness.ResolvedModelTransport{
			Model: "claude-sonnet", Provider: "anthropic", ProviderResolved: true,
		},
	)
	runFilteredLocalPresetSmoke(
		t, bwrapBinary, helperBinary, helperDir, smokeHome,
		allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6,
		allowedPort, deniedPort, hostAllowedPort, hostDeniedPort,
		"local-model-apis-codex", harness.MustGet(harness.CodexName), localModelAPIRules,
		harness.ResolvedModelTransport{
			Model: "gpt-5.4", Provider: "openai", ProviderResolved: true,
		},
	)

	readyPath := filepath.Join(helperDir, "fail-closed-ready")
	logPath := filepath.Join(root, "fail-closed.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NotEmpty(t, wrapped[harness.DefaultName],
		"fail-closed phase requires the Claude wrapped launch command")
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", wrapped[harness.DefaultName])
	cmd.Env = filteredSmokeHelperEnv(
		os.Environ(), allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6,
		allowedPort, deniedPort,
		hostAllowedPort, hostDeniedPort, readyPath, true, false,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	require.NoError(t, cmd.Start())
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	waitForFilteredSmokeReady(t, readyPath, waitCh, logPath)
	pastaPID := waitForFilteredSmokeDescendant(t, cmd.Process.Pid, executables.Pasta)
	require.NoError(t, syscall.Kill(pastaPID, syscall.SIGKILL))
	select {
	case waitErr := <-waitCh:
		require.Error(t, waitErr, "gateway death must terminate the sandbox")
		var exitErr *exec.ExitError
		require.ErrorAs(t, waitErr, &exitErr)
		assert.Equal(t, 125, exitErr.ExitCode())
	case <-time.After(5 * time.Second):
		t.Fatal("filtered sandbox survived supervised pasta death")
	}
	require.NoError(t, logFile.Close())
	logData, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(logData), "sandbox terminated fail-closed")
}

func runFilteredGatewayFailClosedSmoke(
	t *testing.T,
	wrapped, helperDir, root, pastaExecutable string,
	allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6 string,
	allowedPort, deniedPort, hostAllowedPort, hostDeniedPort int,
	smokeKind string,
) {
	t.Helper()
	readyPath := filepath.Join(helperDir, "deny-fail-closed-ready")
	logPath := filepath.Join(root, "deny-fail-closed.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NotEmpty(t, wrapped)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", wrapped)
	cmd.Env = append(filteredSmokeHelperEnv(
		os.Environ(), allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6,
		allowedPort, deniedPort, hostAllowedPort, hostDeniedPort,
		readyPath, true, false,
	), filteredGatewayDenySmokeEnv+"="+smokeKind)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	require.NoError(t, cmd.Start())
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	waitForFilteredSmokeReady(t, readyPath, waitCh, logPath)
	pastaPID := waitForFilteredSmokeDescendant(t, cmd.Process.Pid, pastaExecutable)
	require.NoError(t, syscall.Kill(pastaPID, syscall.SIGKILL))
	select {
	case waitErr := <-waitCh:
		require.Error(t, waitErr, "gateway death must terminate the sandbox")
		var exitErr *exec.ExitError
		require.ErrorAs(t, waitErr, &exitErr)
		assert.Equal(t, 125, exitErr.ExitCode())
	case <-time.After(5 * time.Second):
		t.Fatal("filtered deny sandbox survived supervised pasta death")
	}
	require.NoError(t, logFile.Close())
	logData, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(logData), "sandbox terminated fail-closed")
}

func runFilteredLocalPresetSmoke(
	t *testing.T,
	bwrapBinary, helperBinary, helperDir, smokeHome string,
	allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6 string,
	allowedPort, deniedPort, hostAllowedPort, hostDeniedPort int,
	name string,
	h *harness.Harness,
	rules sandboxpolicy.NetworkRules,
	transport harness.ResolvedModelTransport,
) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		snapshot := sandboxpolicy.EmptySnapshot()
		snapshot.Effective.Network = &rules
		notices, err := ValidateTclaudeLayerNetwork(h, snapshot.Effective, transport)
		require.NoError(t, err)
		require.Len(t, notices, 1)
		assert.Equal(t, sandboxpolicy.AccessNoticeEffectLaunchGated, notices[0].Effect)
		assert.Equal(t, sandboxpolicy.AccessNoticeReasonFilteredModelTraffic, notices[0].Reason)

		axes := sandboxpolicy.ResolvedAxes{Network: rules}
		launchCaps, err := harness.ResolveAccessEnforcement(
			h,
			sandboxpolicy.ImplementationTclaudeLayer,
			axes,
			TclaudeLayerLaunchOSSandbox(sandboxpolicy.NetworkFiltered),
			"",
		)
		require.NoError(t, err)
		rendered, _, err := harness.PlanAccessEnforcement(axes, launchCaps)
		require.NoError(t, err)
		assert.Equal(t, rules, rendered.Network)

		stateRoot := filepath.Join(smokeHome, "."+name)
		require.NoError(t, os.MkdirAll(stateRoot, 0o700))
		spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
			HarnessName: h.Name,
			Cwd:         helperDir,
			Snapshot:    &snapshot,
			StateRoot:   stateRoot,
		})
		require.NoError(t, err)
		command, err := WrapTclaudeLayerSpec(
			bwrapBinary,
			spec,
			clcommon.ShellQuoteArg(helperBinary)+" -test.run=^TestFilteredNetworkGatewayHelper$",
		)
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		cmd.Env = append(filteredSmokeHelperEnv(
			os.Environ(), allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6,
			allowedPort, deniedPort,
			hostAllowedPort, hostDeniedPort, "", false, false,
		), filteredGatewayLocalBaselineEnv+"=1")
		output, runErr := cmd.CombinedOutput()
		require.NoErrorf(t, runErr, "%s filtered smoke output:\n%s", name, output)
		require.NoError(t, ctx.Err())
	})
}

func TestFilteredNetworkGatewayHelper(t *testing.T) {
	if os.Getenv(filteredGatewayHelperEnv) != "1" {
		t.Skip("filtered-network smoke helper")
	}
	status, err := os.ReadFile("/proc/self/status")
	require.NoError(t, err)
	assert.Contains(t, string(status), "\nCapEff:\t0000000000000000\n")
	assert.Contains(t, string(status), "\nCapPrm:\t0000000000000000\n")
	assert.Contains(t, string(status), "\nCapInh:\t0000000000000000\n")
	assert.Contains(t, string(status), "\nCapAmb:\t0000000000000000\n")
	assert.Contains(t, string(status), "\nNoNewPrivs:\t1\n")

	hosts, err := os.ReadFile("/etc/hosts")
	require.NoError(t, err)
	assert.Contains(t, string(hosts),
		sandboxpolicy.FilteredNetworkLoopbackIPv4+" "+
			sandboxpolicy.FilteredNetworkHostLoopbackName)
	dnsSmoke := os.Getenv(filteredGatewayDNSSmokeEnv) == "1"
	if dnsSmoke {
		assert.NotContains(t, string(hosts), filteredGatewayExactHost,
			"host fixture aliases must resolve through the broker, not /etc/hosts")
		resolv, readErr := os.ReadFile("/etc/resolv.conf")
		require.NoError(t, readErr)
		assert.Contains(t, string(resolv),
			"nameserver "+sandboxpolicy.FilteredNetworkDNSIPv4)
	}

	allowedAddr := requireFilteredSmokeEnv(t, filteredGatewayAllowedAddrEnv)
	adjacentAddr := requireFilteredSmokeEnv(t, filteredGatewayAdjacentAddrEnv)
	allowedAddr6 := requireFilteredSmokeEnv(t, filteredGatewayAllowedAddr6Env)
	adjacentAddr6 := requireFilteredSmokeEnv(t, filteredGatewayAdjacentAddr6Env)
	allowedPort := requireFilteredSmokePort(t, filteredGatewayAllowedPortEnv)
	deniedPort := requireFilteredSmokePort(t, filteredGatewayDeniedPortEnv)
	loopbackPort := requireFilteredSmokePort(t, filteredGatewayLoopbackPortEnv)
	loopbackDeniedPort := requireFilteredSmokePort(t, filteredGatewayLoopbackDenyEnv)
	switch os.Getenv(filteredGatewayDenySmokeEnv) {
	case "deny-static":
		runFilteredNetworkStaticDenyHelper(
			t, allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6,
			allowedPort, deniedPort)
		finishFilteredNetworkDenyHelper(t)
		return
	case "deny-dns":
		runFilteredNetworkDNSDenyHelper(
			t, adjacentAddr, adjacentAddr6, allowedPort, deniedPort)
		finishFilteredNetworkDenyHelper(t)
		return
	}

	if os.Getenv(filteredGatewayLocalBaselineEnv) == "1" {
		for _, port := range []int{loopbackPort, loopbackDeniedPort} {
			synthetic := net.JoinHostPort(
				sandboxpolicy.FilteredNetworkHostLoopbackName,
				strconv.Itoa(port),
			)
			filteredSmokeTCPRoundTrip(t, "tcp4", synthetic)
			filteredSmokeUDPRoundTrip(t, "udp4", synthetic)
			filteredSmokeTCPRoundTrip(t, "tcp6", synthetic)
			filteredSmokeUDPRoundTrip(t, "udp6", synthetic)
		}
		filteredSmokeTCPDenied(t, "tcp4", net.JoinHostPort(
			"127.0.0.1", strconv.Itoa(loopbackPort)))
		filteredSmokeUDPDenied(t, "udp4", net.JoinHostPort(
			"127.0.0.1", strconv.Itoa(loopbackPort)))
		filteredSmokeTCPDenied(t, "tcp6", net.JoinHostPort(
			"::1", strconv.Itoa(loopbackPort)))
		filteredSmokeUDPDenied(t, "udp6", net.JoinHostPort(
			"::1", strconv.Itoa(loopbackPort)))
		for _, endpoint := range []struct {
			network string
			address string
		}{
			{"tcp4", net.JoinHostPort(allowedAddr, strconv.Itoa(allowedPort))},
			{"tcp4", net.JoinHostPort(adjacentAddr, strconv.Itoa(allowedPort))},
			{"tcp6", net.JoinHostPort(allowedAddr6, strconv.Itoa(allowedPort))},
			{"tcp6", net.JoinHostPort(adjacentAddr6, strconv.Itoa(allowedPort))},
		} {
			filteredSmokeTCPDenied(t, endpoint.network, endpoint.address)
			filteredSmokeUDPDenied(t, "udp"+strings.TrimPrefix(endpoint.network, "tcp"), endpoint.address)
		}
		filteredSmokeDNSDenied(t, filteredGatewayExactHost, "ip4")
		return
	}

	filteredSmokeTCPRoundTrip(t, "tcp4", net.JoinHostPort(allowedAddr, strconv.Itoa(allowedPort)))
	filteredSmokeUDPRoundTrip(t, "udp4", net.JoinHostPort(allowedAddr, strconv.Itoa(allowedPort)))
	filteredSmokeTCPDenied(t, "tcp4", net.JoinHostPort(allowedAddr, strconv.Itoa(deniedPort)))
	filteredSmokeUDPDenied(t, "udp4", net.JoinHostPort(allowedAddr, strconv.Itoa(deniedPort)))
	filteredSmokeTCPDenied(t, "tcp4", net.JoinHostPort(adjacentAddr, strconv.Itoa(allowedPort)))
	filteredSmokeUDPDenied(t, "udp4", net.JoinHostPort(adjacentAddr, strconv.Itoa(allowedPort)))
	filteredSmokeTCPRoundTrip(t, "tcp6", net.JoinHostPort(allowedAddr6, strconv.Itoa(allowedPort)))
	filteredSmokeUDPRoundTrip(t, "udp6", net.JoinHostPort(allowedAddr6, strconv.Itoa(allowedPort)))
	filteredSmokeTCPDenied(t, "tcp6", net.JoinHostPort(allowedAddr6, strconv.Itoa(deniedPort)))
	filteredSmokeUDPDenied(t, "udp6", net.JoinHostPort(allowedAddr6, strconv.Itoa(deniedPort)))
	filteredSmokeTCPDenied(t, "tcp6", net.JoinHostPort(adjacentAddr6, strconv.Itoa(allowedPort)))
	filteredSmokeUDPDenied(t, "udp6", net.JoinHostPort(adjacentAddr6, strconv.Itoa(allowedPort)))

	synthetic := net.JoinHostPort(
		sandboxpolicy.FilteredNetworkHostLoopbackName,
		strconv.Itoa(loopbackPort),
	)
	filteredSmokeTCPRoundTrip(t, "tcp4", synthetic)
	filteredSmokeUDPRoundTrip(t, "udp4", synthetic)
	filteredSmokeTCPRoundTrip(t, "tcp6", synthetic)
	filteredSmokeUDPRoundTrip(t, "udp6", synthetic)
	filteredSmokeTCPDenied(t, "tcp4", net.JoinHostPort(
		sandboxpolicy.FilteredNetworkHostLoopbackName,
		strconv.Itoa(loopbackDeniedPort),
	))
	filteredSmokeUDPDenied(t, "udp4", net.JoinHostPort(
		sandboxpolicy.FilteredNetworkHostLoopbackName,
		strconv.Itoa(loopbackDeniedPort),
	))
	filteredSmokeTCPDenied(t, "tcp6", net.JoinHostPort(
		sandboxpolicy.FilteredNetworkHostLoopbackName,
		strconv.Itoa(loopbackDeniedPort),
	))
	filteredSmokeUDPDenied(t, "udp6", net.JoinHostPort(
		sandboxpolicy.FilteredNetworkHostLoopbackName,
		strconv.Itoa(loopbackDeniedPort),
	))
	filteredSmokeTCPDenied(t, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(loopbackPort)))
	filteredSmokeUDPDenied(t, "udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(loopbackPort)))
	filteredSmokeTCPDenied(t, "tcp6", net.JoinHostPort("::1", strconv.Itoa(loopbackPort)))
	filteredSmokeUDPDenied(t, "udp6", net.JoinHostPort("::1", strconv.Itoa(loopbackPort)))

	if dnsSmoke {
		runFilteredNetworkDNSHelper(
			t, adjacentAddr, adjacentAddr6, allowedPort, deniedPort)
	}

	if readyPath := strings.TrimSpace(os.Getenv(filteredGatewayReadyPathEnv)); readyPath != "" {
		require.NoError(t, os.WriteFile(readyPath, []byte("ready"), 0o600))
	}
	if os.Getenv(filteredGatewayHoldEnv) == "1" {
		time.Sleep(30 * time.Second)
	}
}

func runFilteredNetworkStaticDenyHelper(
	t *testing.T,
	allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6 string,
	allowedPort, deniedPort int,
) {
	t.Helper()
	for _, tc := range []struct {
		network string
		address string
		allowed bool
	}{
		{"tcp4", net.JoinHostPort(allowedAddr, strconv.Itoa(allowedPort)), true},
		{"udp4", net.JoinHostPort(allowedAddr, strconv.Itoa(allowedPort)), true},
		{"tcp4", net.JoinHostPort(allowedAddr, strconv.Itoa(deniedPort)), false},
		{"udp4", net.JoinHostPort(allowedAddr, strconv.Itoa(deniedPort)), false},
		{"tcp4", net.JoinHostPort(adjacentAddr, strconv.Itoa(deniedPort)), true},
		{"udp4", net.JoinHostPort(adjacentAddr, strconv.Itoa(deniedPort)), true},
		{"tcp6", net.JoinHostPort(allowedAddr6, strconv.Itoa(allowedPort)), true},
		{"udp6", net.JoinHostPort(allowedAddr6, strconv.Itoa(allowedPort)), true},
		{"tcp6", net.JoinHostPort(allowedAddr6, strconv.Itoa(deniedPort)), false},
		{"udp6", net.JoinHostPort(allowedAddr6, strconv.Itoa(deniedPort)), false},
		{"tcp6", net.JoinHostPort(adjacentAddr6, strconv.Itoa(allowedPort)), false},
		{"udp6", net.JoinHostPort(adjacentAddr6, strconv.Itoa(allowedPort)), false},
	} {
		if tc.allowed && strings.HasPrefix(tc.network, "tcp") {
			filteredSmokeTCPRoundTrip(t, tc.network, tc.address)
		} else if tc.allowed {
			filteredSmokeUDPRoundTrip(t, tc.network, tc.address)
		} else if strings.HasPrefix(tc.network, "tcp") {
			filteredSmokeTCPDenied(t, tc.network, tc.address)
		} else {
			filteredSmokeUDPDenied(t, tc.network, tc.address)
		}
	}
}

func runFilteredNetworkDNSDenyHelper(
	t *testing.T,
	adjacentAddr, adjacentAddr6 string,
	allowedPort, deniedPort int,
) {
	t.Helper()
	adjacent4Allowed := net.JoinHostPort(
		adjacentAddr, strconv.Itoa(allowedPort))
	adjacent6Allowed := net.JoinHostPort(
		adjacentAddr6, strconv.Itoa(allowedPort))

	// A CIDR baseline permits the shared destination before any negative lease.
	filteredSmokeTCPRoundTrip(t, "tcp4", adjacent4Allowed)
	filteredSmokeUDPRoundTrip(t, "udp4", adjacent4Allowed)
	filteredSmokeTCPRoundTrip(t, "tcp6", adjacent6Allowed)
	filteredSmokeUDPRoundTrip(t, "udp6", adjacent6Allowed)

	// The sibling is positively named but denied only on one port. DNS still
	// answers; nft remains the TCP/UDP port authority.
	filteredSmokeDNSAllowed(t, filteredGatewaySiblingHost, "ip4")
	filteredSmokeDNSAllowed(t, filteredGatewaySiblingHost, "ip6")
	filteredSmokeTCPRoundTrip(t, "tcp4", net.JoinHostPort(
		filteredGatewaySiblingHost, strconv.Itoa(allowedPort)))
	filteredSmokeUDPRoundTrip(t, "udp6", net.JoinHostPort(
		filteredGatewaySiblingHost, strconv.Itoa(allowedPort)))
	filteredSmokeTCPDenied(t, "tcp4", net.JoinHostPort(
		filteredGatewaySiblingHost, strconv.Itoa(deniedPort)))
	filteredSmokeUDPDenied(t, "udp6", net.JoinHostPort(
		filteredGatewaySiblingHost, strconv.Itoa(deniedPort)))

	established := filteredSmokeOpenTCPEcho(t, "tcp4", adjacent4Allowed)
	defer func() { _ = established.Close() }()

	// A denied name on the same address installs negative A and AAAA leases.
	// They defeat both the CIDR allow and the earlier positive sibling lease.
	filteredSmokeDNSDenied(t, filteredGatewayExactHost, "ip4")
	filteredSmokeDNSDenied(t, filteredGatewayExactHost, "ip6")
	filteredSmokeTCPDenied(t, "tcp4", adjacent4Allowed)
	filteredSmokeUDPDenied(t, "udp4", adjacent4Allowed)
	filteredSmokeTCPDenied(t, "tcp6", adjacent6Allowed)
	filteredSmokeUDPDenied(t, "udp6", adjacent6Allowed)
	filteredSmokeTCPEchoDeniedOnConnection(t, established)

	// Exact domains do not deny children; subdomain rules are label-bound.
	filteredSmokeDNSDenied(t, filteredGatewayExactDomain, "ip4")
	filteredSmokeDNSAllowed(t, filteredGatewayExactDomainChild, "ip4")
	filteredSmokeDNSDenied(t, filteredGatewayTreeDomain, "ip4")
	filteredSmokeDNSDenied(t, filteredGatewayTreeDomainChild, "ip4")
	filteredSmokeDNSAllowed(t, filteredGatewaySuffixConfusion, "ip4")

	// Negative authority is observation- and TTL-bound. Expiry restores the
	// CIDR baseline, and a fresh denied lookup refreshes the cut.
	time.Sleep(filteredNetworkDNSHostMappingTTL + time.Second)
	filteredSmokeTCPRoundTrip(t, "tcp4", adjacent4Allowed)
	filteredSmokeTCPRoundTrip(t, "tcp6", adjacent6Allowed)
	filteredSmokeDNSDenied(t, filteredGatewayExactHost, "ip4")
	filteredSmokeTCPDenied(t, "tcp4", adjacent4Allowed)
}

func finishFilteredNetworkDenyHelper(t *testing.T) {
	t.Helper()
	if readyPath := strings.TrimSpace(os.Getenv(filteredGatewayReadyPathEnv)); readyPath != "" {
		require.NoError(t, os.WriteFile(readyPath, []byte("ready"), 0o600))
	}
	if os.Getenv(filteredGatewayHoldEnv) == "1" {
		time.Sleep(30 * time.Second)
	}
}

func filteredSmokeCoveringPrefix(t *testing.T, first, second string) string {
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
	t.Fatal("addresses have no covering prefix")
	return ""
}

func filteredSmokeHelperEnv(
	base []string,
	allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6 string,
	allowedPort, deniedPort, loopbackPort, loopbackDeniedPort int,
	readyPath string,
	hold bool,
	dnsSmoke bool,
) []string {
	env := append([]string(nil), base...)
	env = append(env,
		filteredGatewayHelperEnv+"=1",
		filteredGatewayAllowedAddrEnv+"="+allowedAddr,
		filteredGatewayAdjacentAddrEnv+"="+adjacentAddr,
		filteredGatewayAllowedAddr6Env+"="+allowedAddr6,
		filteredGatewayAdjacentAddr6Env+"="+adjacentAddr6,
		filteredGatewayAllowedPortEnv+"="+strconv.Itoa(allowedPort),
		filteredGatewayDeniedPortEnv+"="+strconv.Itoa(deniedPort),
		filteredGatewayLoopbackPortEnv+"="+strconv.Itoa(loopbackPort),
		filteredGatewayLoopbackDenyEnv+"="+strconv.Itoa(loopbackDeniedPort),
		filteredGatewayReadyPathEnv+"="+readyPath,
	)
	if hold {
		env = append(env, filteredGatewayHoldEnv+"=1")
	}
	if dnsSmoke {
		env = append(env, filteredGatewayDNSSmokeEnv+"=1")
	}
	return env
}

func runFilteredNetworkDNSHelper(
	t *testing.T,
	adjacentAddr, adjacentAddr6 string,
	allowedPort, deniedPort int,
) {
	t.Helper()
	adjacent4Allowed := net.JoinHostPort(
		adjacentAddr, strconv.Itoa(allowedPort))
	adjacent6Allowed := net.JoinHostPort(
		adjacentAddr6, strconv.Itoa(allowedPort))

	// No IP membership exists until an authored DNS identity is resolved.
	filteredSmokeTCPDenied(t, "tcp4", adjacent4Allowed)
	filteredSmokeTCPDenied(t, "tcp6", adjacent6Allowed)
	filteredSmokeDNSDenied(t, filteredGatewaySiblingHost, "ip4")
	filteredSmokeDNSDenied(t, filteredGatewayExactDomainChild, "ip4")
	filteredSmokeDNSDenied(t, filteredGatewaySuffixConfusion, "ip4")

	exactEndpoint := net.JoinHostPort(
		filteredGatewayExactHost, strconv.Itoa(allowedPort))
	filteredSmokeTCPRoundTrip(t, "tcp4", exactEndpoint)
	filteredSmokeUDPRoundTrip(t, "udp4", exactEndpoint)
	filteredSmokeTCPRoundTrip(t, "tcp6", exactEndpoint)
	filteredSmokeUDPRoundTrip(t, "udp6", exactEndpoint)
	filteredSmokeTCPDenied(t, "tcp4", net.JoinHostPort(
		filteredGatewayExactHost, strconv.Itoa(deniedPort)))

	filteredSmokeTCPRoundTrip(t, "tcp4", net.JoinHostPort(
		filteredGatewayExactDomain, strconv.Itoa(allowedPort)))
	filteredSmokeTCPRoundTrip(t, "tcp4", net.JoinHostPort(
		filteredGatewayTreeDomain, strconv.Itoa(allowedPort)))
	filteredSmokeTCPRoundTrip(t, "tcp4", net.JoinHostPort(
		filteredGatewayTreeDomainChild, strconv.Itoa(allowedPort)))

	// This is the named shared-IP residual: once one allowed DNS identity has
	// leased the address, an IP literal can reuse it until expiry.
	filteredSmokeTCPRoundTrip(t, "tcp4", adjacent4Allowed)
	filteredSmokeTCPRoundTrip(t, "tcp6", adjacent6Allowed)

	// Keep one admitted flow open across expiry. New flows lose membership at
	// the TTL, but this established connection remains usable.
	connection := filteredSmokeOpenTCPEcho(t, "tcp4", exactEndpoint)
	defer func() { _ = connection.Close() }()
	time.Sleep(filteredNetworkDNSHostMappingTTL + time.Second)
	filteredSmokeTCPEchoOnConnection(t, connection)
	filteredSmokeTCPDenied(t, "tcp4", adjacent4Allowed)
	filteredSmokeTCPDenied(t, "tcp6", adjacent6Allowed)

	// A fresh answer—not a broker timer—refreshes membership for new flows.
	filteredSmokeTCPRoundTrip(t, "tcp4", exactEndpoint)
	filteredSmokeTCPRoundTrip(t, "tcp4", adjacent4Allowed)
}

func filteredSmokeDNSDenied(t *testing.T, host, network string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(
		context.Background(), filteredGatewayConnectionTimeout)
	defer cancel()
	_, err := net.DefaultResolver.LookupNetIP(ctx, network, host)
	require.Errorf(t, err, "DNS deny %s (%s)", host, network)
}

func filteredSmokeDNSAllowed(t *testing.T, host, network string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(
		context.Background(), filteredGatewayConnectionTimeout)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, network, host)
	require.NoErrorf(t, err, "DNS allow %s (%s)", host, network)
	require.NotEmpty(t, addresses, "DNS allow %s (%s)", host, network)
}

func filteredSmokeOpenTCPEcho(
	t *testing.T,
	network, address string,
) net.Conn {
	t.Helper()
	connection, err := net.DialTimeout(
		network, address, filteredGatewayConnectionTimeout)
	require.NoErrorf(t, err, "%s allow %s", network, address)
	filteredSmokeTCPEchoOnConnection(t, connection)
	return connection
}

func filteredSmokeTCPEchoOnConnection(t *testing.T, connection net.Conn) {
	t.Helper()
	require.NoError(t, connection.SetDeadline(
		time.Now().Add(filteredGatewayConnectionTimeout)))
	payload := []byte("tclaude-filtered-established")
	_, err := connection.Write(payload)
	require.NoError(t, err)
	reply := make([]byte, len(payload))
	_, err = io.ReadFull(connection, reply)
	require.NoError(t, err)
	assert.Equal(t, payload, reply)
}

func filteredSmokeTCPEchoDeniedOnConnection(
	t *testing.T,
	connection net.Conn,
) {
	t.Helper()
	require.NoError(t, connection.SetDeadline(
		time.Now().Add(filteredGatewayConnectionTimeout)))
	_, _ = connection.Write([]byte("must-be-cut"))
	buffer := make([]byte, 64)
	_, err := connection.Read(buffer)
	require.Error(t, err,
		"a fresh negative DNS lease must cut matching established TCP authority")
}

func requireFilteredSmokeEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	require.NotEmpty(t, value, "%s is required", name)
	return value
}

func requireFilteredSmokePort(t *testing.T, name string) int {
	t.Helper()
	value, err := strconv.Atoi(requireFilteredSmokeEnv(t, name))
	require.NoError(t, err)
	require.Greater(t, value, 0)
	require.LessOrEqual(t, value, 65535)
	return value
}

func startFilteredSmokeLoopbackEcho(t *testing.T) int {
	t.Helper()
	tcp4, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	require.NoError(t, err)
	port := tcp4.Addr().(*net.TCPAddr).Port
	tcp6, err := net.ListenTCP("tcp6", &net.TCPAddr{
		IP: net.ParseIP("::1"), Port: port,
	})
	require.NoError(t, err)
	udp4, err := net.ListenUDP("udp4", &net.UDPAddr{
		IP: net.ParseIP("127.0.0.1"), Port: port,
	})
	require.NoError(t, err)
	udp6, err := net.ListenUDP("udp6", &net.UDPAddr{
		IP: net.ParseIP("::1"), Port: port,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = tcp4.Close()
		_ = tcp6.Close()
		_ = udp4.Close()
		_ = udp6.Close()
	})
	for _, listener := range []*net.TCPListener{tcp4, tcp6} {
		go runFilteredSmokeTCPEcho(listener)
	}
	for _, connection := range []*net.UDPConn{udp4, udp6} {
		go runFilteredSmokeUDPEcho(connection)
	}
	return port
}

func runFilteredSmokeTCPEcho(listener *net.TCPListener) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = connection.Close() }()
			_, _ = io.Copy(connection, connection)
		}()
	}
}

func runFilteredSmokeUDPEcho(connection *net.UDPConn) {
	buffer := make([]byte, 2048)
	for {
		n, remote, err := connection.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		_, _ = connection.WriteToUDP(buffer[:n], remote)
	}
}

func filteredSmokeTCPRoundTrip(t *testing.T, network, address string) {
	t.Helper()
	connection, err := net.DialTimeout(network, address, filteredGatewayConnectionTimeout)
	require.NoErrorf(t, err, "%s allow %s", network, address)
	defer func() { _ = connection.Close() }()
	require.NoError(t, connection.SetDeadline(time.Now().Add(filteredGatewayConnectionTimeout)))
	payload := []byte("tclaude-filtered-tcp")
	_, err = connection.Write(payload)
	require.NoError(t, err)
	reply := make([]byte, len(payload))
	_, err = io.ReadFull(connection, reply)
	require.NoError(t, err)
	assert.Equal(t, payload, reply)
}

func filteredSmokeTCPDenied(t *testing.T, network, address string) {
	t.Helper()
	connection, err := net.DialTimeout(network, address, filteredGatewayConnectionTimeout)
	if err == nil {
		_ = connection.Close()
	}
	require.Errorf(t, err, "%s deny %s", network, address)
}

func filteredSmokeUDPRoundTrip(t *testing.T, network, address string) {
	t.Helper()
	remote, err := net.ResolveUDPAddr(network, address)
	require.NoError(t, err)
	connection, err := net.DialUDP(network, nil, remote)
	require.NoError(t, err)
	defer func() { _ = connection.Close() }()
	require.NoError(t, connection.SetDeadline(time.Now().Add(filteredGatewayConnectionTimeout)))
	payload := []byte("tclaude-filtered-udp")
	_, err = connection.Write(payload)
	require.NoError(t, err)
	reply := make([]byte, len(payload))
	_, err = io.ReadFull(connection, reply)
	require.NoErrorf(t, err, "%s allow %s", network, address)
	assert.Equal(t, payload, reply)
}

func filteredSmokeUDPDenied(t *testing.T, network, address string) {
	t.Helper()
	remote, err := net.ResolveUDPAddr(network, address)
	require.NoError(t, err)
	connection, err := net.DialUDP(network, nil, remote)
	require.NoError(t, err)
	defer func() { _ = connection.Close() }()
	require.NoError(t, connection.SetDeadline(time.Now().Add(filteredGatewayConnectionTimeout)))
	_, err = connection.Write([]byte("must-time-out"))
	if err != nil {
		require.ErrorIsf(t, err, syscall.EPERM, "%s deny %s", network, address)
		return
	}
	buffer := make([]byte, 64)
	_, err = connection.Read(buffer)
	require.Errorf(t, err, "%s deny %s", network, address)
}

func waitForFilteredSmokeReady(
	t *testing.T,
	readyPath string,
	waitCh <-chan error,
	logPath string,
) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			return
		}
		select {
		case waitErr := <-waitCh:
			logData, _ := os.ReadFile(logPath)
			t.Fatalf("filtered helper exited before fail-closed checkpoint: %v\n%s",
				waitErr, logData)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("filtered helper did not reach fail-closed checkpoint")
}

func waitForFilteredSmokeDescendant(t *testing.T, rootPID int, executable string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, pid := range filteredSmokeDescendants(rootPID) {
			if filteredSmokeProcessIsExecutable(pid, executable) {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("did not find supervised %s below process %d", executable, rootPID)
	return 0
}

func filteredSmokeProcessIsExecutable(pid int, executable string) bool {
	procDir := filepath.Join("/proc", strconv.Itoa(pid))
	if resolved, err := os.Readlink(filepath.Join(procDir, "exe")); err == nil {
		return resolved == executable || resolved == executable+".avx2"
	}
	// pasta clears PR_SET_DUMPABLE during self-isolation, after which the
	// ptrace-gated /proc/<pid>/exe link is deliberately unreadable. The
	// kernel comm name remains visible. This fallback is still constrained to
	// descendants of the launched relay; killing it must then make that relay
	// report the gateway's fail-closed exit 125.
	comm, err := os.ReadFile(filepath.Join(procDir, "comm"))
	if err != nil {
		return false
	}
	return filteredSmokeExecutableNameMatches(
		strings.TrimSpace(string(comm)),
		filepath.Base(executable),
	)
}

func filteredSmokeExecutableNameMatches(name, expected string) bool {
	return name == expected || name == expected+".avx2"
}

func filteredSmokeDescendants(rootPID int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := make(map[int][]int)
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil {
			continue
		}
		status, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if readErr != nil {
			continue
		}
		for _, line := range bytes.Split(status, []byte{'\n'}) {
			if !bytes.HasPrefix(line, []byte("PPid:\t")) {
				continue
			}
			parent, parentErr := strconv.Atoi(strings.TrimSpace(
				string(bytes.TrimPrefix(line, []byte("PPid:\t")))))
			if parentErr == nil {
				children[parent] = append(children[parent], pid)
			}
			break
		}
	}
	out := []int{}
	queue := append([]int(nil), children[rootPID]...)
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		out = append(out, pid)
		queue = append(queue, children[pid]...)
	}
	return out
}
