//go:build linux

package session

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
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
	filteredGatewayTclaudeBinaryEnv  = "TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY"
	filteredGatewayConnectionTimeout = 900 * time.Millisecond
)

// TestTclaudeLayerFilteredNetworkSmoke is the named executing CI boundary for
// the M2b Claude/Codex tclaude-layer cells. The runner supplies two live
// adjacent targets from a separate network namespace, so allowed CIDR+port,
// denied port, denied adjacent CIDR, TCP, and UDP are all observable outcomes.
// It also exercises synthetic host loopback and kills the supervised pasta
// process to prove fail-closed sandbox teardown.
func TestTclaudeLayerFilteredNetworkSmoke(t *testing.T) {
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
				hostAllowedPort, hostDeniedPort, "", false,
			)
			output, runErr := cmd.CombinedOutput()
			require.NoErrorf(t, runErr, "%s filtered smoke output:\n%s", harnessName, output)
			require.NoError(t, ctx.Err())
		})
	}

	readyPath := filepath.Join(helperDir, "fail-closed-ready")
	logPath := filepath.Join(root, "fail-closed.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", wrapped[harness.DefaultName])
	cmd.Env = filteredSmokeHelperEnv(
		os.Environ(), allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6,
		allowedPort, deniedPort,
		hostAllowedPort, hostDeniedPort, readyPath, true,
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

func TestFilteredNetworkGatewayHelper(t *testing.T) {
	if os.Getenv(filteredGatewayHelperEnv) != "1" {
		t.Skip("filtered-network smoke helper")
	}
	status, err := os.ReadFile("/proc/self/status")
	require.NoError(t, err)
	assert.Contains(t, string(status), "\nCapEff:\t0000000000000000\n")
	assert.Contains(t, string(status), "\nCapPrm:\t0000000000000000\n")
	assert.Contains(t, string(status), "\nNoNewPrivs:\t1\n")

	hosts, err := os.ReadFile("/etc/hosts")
	require.NoError(t, err)
	assert.Contains(t, string(hosts),
		sandboxpolicy.FilteredNetworkLoopbackIPv4+" "+
			sandboxpolicy.FilteredNetworkHostLoopbackName)

	allowedAddr := requireFilteredSmokeEnv(t, filteredGatewayAllowedAddrEnv)
	adjacentAddr := requireFilteredSmokeEnv(t, filteredGatewayAdjacentAddrEnv)
	allowedAddr6 := requireFilteredSmokeEnv(t, filteredGatewayAllowedAddr6Env)
	adjacentAddr6 := requireFilteredSmokeEnv(t, filteredGatewayAdjacentAddr6Env)
	allowedPort := requireFilteredSmokePort(t, filteredGatewayAllowedPortEnv)
	deniedPort := requireFilteredSmokePort(t, filteredGatewayDeniedPortEnv)
	loopbackPort := requireFilteredSmokePort(t, filteredGatewayLoopbackPortEnv)
	loopbackDeniedPort := requireFilteredSmokePort(t, filteredGatewayLoopbackDenyEnv)

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

	if readyPath := strings.TrimSpace(os.Getenv(filteredGatewayReadyPathEnv)); readyPath != "" {
		require.NoError(t, os.WriteFile(readyPath, []byte("ready"), 0o600))
	}
	if os.Getenv(filteredGatewayHoldEnv) == "1" {
		time.Sleep(30 * time.Second)
	}
}

func filteredSmokeHelperEnv(
	base []string,
	allowedAddr, adjacentAddr, allowedAddr6, adjacentAddr6 string,
	allowedPort, deniedPort, loopbackPort, loopbackDeniedPort int,
	readyPath string,
	hold bool,
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
	return env
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
	require.NoError(t, err)
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
			resolved, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
			if err == nil && resolved == executable {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("did not find supervised %s below process %d", executable, rootPID)
	return 0
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
