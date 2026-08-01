//go:build darwin

package session

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
)

const (
	seatbeltProxyFloorHelperEnv       = "TCLAUDE_SEATBELT_PROXY_FLOOR_HELPER"
	seatbeltProxyFloorEndpointEnv     = "TCLAUDE_SEATBELT_PROXY_FLOOR_ENDPOINT"
	seatbeltProxyFloorControlEnv      = "TCLAUDE_SEATBELT_PROXY_FLOOR_CONTROL"
	seatbeltProxyFloorSamePortEnv     = "TCLAUDE_SEATBELT_PROXY_FLOOR_SAME_PORT"
	seatbeltProxyFloorAgentdSocketEnv = "TCLAUDE_SEATBELT_PROXY_FLOOR_AGENTD_SOCKET"
	seatbeltProxyFloorHelperTest      = "^TestSeatbeltProxyFloorSmokeHelper$"
	seatbeltProxyFloorTimeout         = 2 * time.Second
)

// TestSeatbeltProxyFloorSmoke is §8.2 test 6. It activates the M3.1-generated
// profile around a real subprocess and proves that the proxy endpoint is the
// only IP route left: both proxy carriages work on its one listener, while a
// live second loopback port, a live same-port service on a non-loopback local
// address, external TCP, UDP, and listener creation all fail with Seatbelt's
// EPERM. The agentd AF_UNIX floor remains usable.
func TestSeatbeltProxyFloorSmoke(t *testing.T) {
	if os.Getenv("TCLAUDE_SANDBOX_V2_SMOKE") != "1" {
		t.Skip("set TCLAUDE_SANDBOX_V2_SMOKE=1 on macOS to exercise sandbox-exec")
	}

	binary, _, err := ResolveTclaudeLayerForEngine(
		sandboxpolicy.NetworkFiltered,
		sandboxpolicy.RootConstructed,
		sandboxpolicy.NetworkEngineProxy,
	)
	require.NoError(t, err)

	proxyListener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	proxyEndpoint := proxyListener.Addr().(*net.TCPAddr).AddrPort()

	controlListener := seatbeltProxyFloorEchoListener(t, "127.0.0.1:0")
	controlEndpoint := controlListener.Addr().String()
	controlPort := controlListener.Addr().(*net.TCPAddr).Port

	samePortListener, inventory := seatbeltProxyFloorSamePortListener(t, proxyEndpoint.Port())
	t.Logf("same-port/different-local-address probe selected %s; runner interfaces:\n%s",
		samePortListener.Addr(), inventory)

	rules := sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeList,
		Engine: sandboxpolicy.NetworkEngineProxy,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			Loopback: true,
			Ports:    []int{controlPort},
		}},
	}
	compiled, err := sandboxpolicy.CompileFilteredNetworkRules(rules)
	require.NoError(t, err)
	plan := sandboxpolicy.MountPlan{
		NetworkPosture:  sandboxpolicy.NetworkFiltered,
		NetworkEngine:   sandboxpolicy.NetworkEngineProxy,
		FilteredNetwork: &compiled,
	}
	require.True(t, tclaudeLayerPlanDeploysProxy(plan),
		"fixture must reach the generated proxy floor rather than the native loopback profile")

	var decisionMu sync.Mutex
	decisions := map[sandboxproxy.Carriage]int{}
	proxy, err := sandboxproxy.NewFromRuleSet(compiled, sandboxproxy.Config{
		OnDecision: func(carriage sandboxproxy.Carriage, _ sandboxproxy.Target, decision sandboxproxy.Decision) {
			if decision.Verdict == sandboxproxy.VerdictAllowed {
				decisionMu.Lock()
				decisions[carriage]++
				decisionMu.Unlock()
			}
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = proxy.Close() })
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- proxy.Serve(proxyListener) }()
	t.Cleanup(func() {
		_ = proxy.Close()
		select {
		case serveErr := <-proxyDone:
			assert.NoError(t, serveErr)
		case <-time.After(time.Second):
			t.Error("proxy server did not stop")
		}
	})

	home := t.TempDir()
	t.Setenv("HOME", home)
	agentdSocket := agentipc.CanonicalSocketPath()
	require.NotEmpty(t, agentdSocket)
	require.NoError(t, os.MkdirAll(filepath.Dir(agentdSocket), 0o700))
	agentdListener, err := net.Listen("unix", agentdSocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = agentdListener.Close() })
	seatbeltProxyFloorServeEcho(t, agentdListener)

	tmuxSocketDir := filepath.Join(home, "tmux")
	require.NoError(t, os.MkdirAll(tmuxSocketDir, 0o700))
	runtimeTempDir, err := filepath.EvalSymlinks(os.TempDir())
	require.NoError(t, err)
	profile, params, err := renderSeatbeltProfile(
		nil,
		[]string{agentdSocket},
		plan,
		proxyEndpoint,
		nil,
		tmuxSocketDir,
		runtimeTempDir,
		darwinSeatbeltLstatIdentity,
		nil,
	)
	require.NoError(t, err)

	args := []string{"-p", profile}
	for _, param := range params {
		args = append(args, "-D"+param.name+"="+param.path)
	}
	args = append(args, "--", os.Args[0], "-test.run="+seatbeltProxyFloorHelperTest, "-test.v")
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(),
		seatbeltProxyFloorHelperEnv+"=1",
		seatbeltProxyFloorEndpointEnv+"="+proxyEndpoint.String(),
		seatbeltProxyFloorControlEnv+"="+controlEndpoint,
		seatbeltProxyFloorSamePortEnv+"="+samePortListener.Addr().String(),
		seatbeltProxyFloorAgentdSocketEnv+"="+agentdSocket,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	require.NoErrorf(t, cmd.Run(), "Seatbelt proxy-floor helper output:\n%s", output.String())
	t.Logf("Seatbelt proxy-floor helper observations:\n%s", output.String())

	for _, marker := range []string{
		"seatbelt-proxy-floor: HTTP CONNECT carriage: carried",
		"seatbelt-proxy-floor: SOCKS5 carriage: carried",
		"seatbelt-proxy-floor: second loopback port: refused with EPERM",
		"seatbelt-proxy-floor: same port on non-loopback local address: refused with EPERM",
		"seatbelt-proxy-floor: external TCP: refused with EPERM",
		"seatbelt-proxy-floor: UDP send: refused with EPERM",
		"seatbelt-proxy-floor: network-bind: refused with EPERM",
		"seatbelt-proxy-floor: agentd AF_UNIX floor: carried",
	} {
		assert.Contains(t, output.String(), marker,
			"the smoke must report each operation it actually executed")
	}
	decisionMu.Lock()
	defer decisionMu.Unlock()
	assert.GreaterOrEqual(t, decisions[sandboxproxy.CarriageHTTP], 1,
		"the production proxy must observe the HTTP carriage")
	assert.GreaterOrEqual(t, decisions[sandboxproxy.CarriageSOCKS5], 1,
		"the production proxy must observe the SOCKS5 carriage")
}

func TestSeatbeltProxyFloorSmokeHelper(t *testing.T) {
	if os.Getenv(seatbeltProxyFloorHelperEnv) != "1" {
		t.Skip("Seatbelt proxy-floor helper subprocess")
	}
	proxyEndpoint := os.Getenv(seatbeltProxyFloorEndpointEnv)
	controlEndpoint := os.Getenv(seatbeltProxyFloorControlEnv)
	samePortEndpoint := os.Getenv(seatbeltProxyFloorSamePortEnv)
	agentdSocket := os.Getenv(seatbeltProxyFloorAgentdSocketEnv)

	seatbeltProxyFloorHTTPRoundTrip(t, proxyEndpoint, controlEndpoint)
	fmt.Println("seatbelt-proxy-floor: HTTP CONNECT carriage: carried")
	seatbeltProxyFloorSOCKS5RoundTrip(t, proxyEndpoint, controlEndpoint)
	fmt.Println("seatbelt-proxy-floor: SOCKS5 carriage: carried")

	seatbeltProxyFloorRequireEPERM(t, "second loopback TCP connect",
		func() error { return seatbeltProxyFloorDialAndClose("tcp4", controlEndpoint) })
	fmt.Println("seatbelt-proxy-floor: second loopback port: refused with EPERM")
	seatbeltProxyFloorRequireEPERM(t, "same-port non-loopback TCP connect",
		func() error { return seatbeltProxyFloorDialAndClose("tcp", samePortEndpoint) })
	fmt.Println("seatbelt-proxy-floor: same port on non-loopback local address: refused with EPERM")
	seatbeltProxyFloorRequireEPERM(t, "external TCP connect",
		func() error { return seatbeltProxyFloorDialAndClose("tcp4", "1.1.1.1:443") })
	fmt.Println("seatbelt-proxy-floor: external TCP: refused with EPERM")
	seatbeltProxyFloorRequireEPERM(t, "UDP send", func() error {
		conn, err := net.DialTimeout("udp4", "1.1.1.1:53", seatbeltProxyFloorTimeout)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetWriteDeadline(time.Now().Add(seatbeltProxyFloorTimeout))
		_, err = conn.Write([]byte("seatbelt-proxy-floor"))
		return err
	})
	fmt.Println("seatbelt-proxy-floor: UDP send: refused with EPERM")
	seatbeltProxyFloorRequireEPERM(t, "TCP listener creation", func() error {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err == nil {
			_ = listener.Close()
		}
		return err
	})
	fmt.Println("seatbelt-proxy-floor: network-bind: refused with EPERM")

	seatbeltProxyFloorEchoRoundTrip(t, "unix", agentdSocket, "agentd-floor")
	fmt.Println("seatbelt-proxy-floor: agentd AF_UNIX floor: carried")
}

func seatbeltProxyFloorRequireEPERM(t *testing.T, operation string, run func() error) {
	t.Helper()
	err := run()
	require.Error(t, err, "%s unexpectedly succeeded", operation)
	require.True(t, errors.Is(err, syscall.EPERM),
		"%s must fail with Seatbelt EPERM, got %v", operation, err)
}

func seatbeltProxyFloorDialAndClose(network, endpoint string) error {
	conn, err := net.DialTimeout(network, endpoint, seatbeltProxyFloorTimeout)
	if err == nil {
		err = conn.Close()
	}
	return err
}

func seatbeltProxyFloorHTTPRoundTrip(t *testing.T, proxyEndpoint, target string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", proxyEndpoint, seatbeltProxyFloorTimeout)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(seatbeltProxyFloorTimeout)))
	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	require.NoError(t, err)
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, status, " 200 ", "HTTP CONNECT status")
	for {
		line, readErr := reader.ReadString('\n')
		require.NoError(t, readErr)
		if line == "\r\n" {
			break
		}
	}
	seatbeltProxyFloorConnEcho(t, conn, reader, "http-connect")
}

func seatbeltProxyFloorSOCKS5RoundTrip(t *testing.T, proxyEndpoint, target string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", proxyEndpoint, seatbeltProxyFloorTimeout)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(seatbeltProxyFloorTimeout)))
	_, err = conn.Write([]byte{5, 1, 0})
	require.NoError(t, err)
	reader := bufio.NewReader(conn)
	greeting := make([]byte, 2)
	_, err = io.ReadFull(reader, greeting)
	require.NoError(t, err)
	require.Equal(t, []byte{5, 0}, greeting)
	host, rawPort, err := net.SplitHostPort(target)
	require.NoError(t, err)
	address := net.ParseIP(host).To4()
	require.NotNil(t, address)
	port, err := strconv.Atoi(rawPort)
	require.NoError(t, err)
	request := append([]byte{5, 1, 0, 1}, address...)
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	_, err = conn.Write(request)
	require.NoError(t, err)
	header := make([]byte, 4)
	_, err = io.ReadFull(reader, header)
	require.NoError(t, err)
	require.Equal(t, byte(5), header[0])
	require.Equal(t, byte(0), header[1], "SOCKS5 CONNECT reply")
	addrLen := 0
	switch header[3] {
	case 1:
		addrLen = 4
	case 4:
		addrLen = 16
	case 3:
		length, readErr := reader.ReadByte()
		require.NoError(t, readErr)
		addrLen = int(length)
	default:
		t.Fatalf("unexpected SOCKS5 address type %d", header[3])
	}
	_, err = io.ReadFull(reader, make([]byte, addrLen+2))
	require.NoError(t, err)
	seatbeltProxyFloorConnEcho(t, conn, reader, "socks5")
}

func seatbeltProxyFloorConnEcho(t *testing.T, conn net.Conn, reader io.Reader, nonce string) {
	t.Helper()
	_, err := conn.Write([]byte(nonce))
	require.NoError(t, err)
	echo := make([]byte, len(nonce))
	_, err = io.ReadFull(reader, echo)
	require.NoError(t, err)
	require.Equal(t, nonce, string(echo))
}

func seatbeltProxyFloorEchoRoundTrip(t *testing.T, network, endpoint, nonce string) {
	t.Helper()
	conn, err := net.DialTimeout(network, endpoint, seatbeltProxyFloorTimeout)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(seatbeltProxyFloorTimeout)))
	seatbeltProxyFloorConnEcho(t, conn, conn, nonce)
}

func seatbeltProxyFloorEchoListener(t *testing.T, endpoint string) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp4", endpoint)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	seatbeltProxyFloorServeEcho(t, listener)
	return listener
}

func seatbeltProxyFloorServeEcho(t *testing.T, listener net.Listener) {
	t.Helper()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
}

type seatbeltProxyFloorLocalAddress struct {
	interfaceName string
	flags         net.Flags
	address       netip.Addr
}

func seatbeltProxyFloorSamePortListener(t *testing.T, port uint16) (net.Listener, string) {
	t.Helper()
	interfaces, err := net.Interfaces()
	require.NoError(t, err)
	var observed []string
	var candidates []seatbeltProxyFloorLocalAddress
	for _, iface := range interfaces {
		addresses, addressErr := iface.Addrs()
		if addressErr != nil {
			observed = append(observed,
				fmt.Sprintf("%s flags=%s addresses=<error: %v>", iface.Name, iface.Flags, addressErr))
			continue
		}
		if len(addresses) == 0 {
			observed = append(observed,
				fmt.Sprintf("%s flags=%s addresses=<none>", iface.Name, iface.Flags))
		}
		for _, raw := range addresses {
			observed = append(observed,
				fmt.Sprintf("%s flags=%s address=%s", iface.Name, iface.Flags, raw.String()))
			prefix, parseErr := netip.ParsePrefix(raw.String())
			if parseErr != nil || iface.Flags&net.FlagUp == 0 {
				continue
			}
			address := prefix.Addr().Unmap()
			if !address.IsGlobalUnicast() || address.IsLoopback() || address.IsLinkLocalUnicast() {
				continue
			}
			candidates = append(candidates, seatbeltProxyFloorLocalAddress{
				interfaceName: iface.Name,
				flags:         iface.Flags,
				address:       address,
			})
		}
	}
	sort.Strings(observed)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].interfaceName != candidates[j].interfaceName {
			return candidates[i].interfaceName < candidates[j].interfaceName
		}
		return candidates[i].address.Less(candidates[j].address)
	})
	inventory := strings.Join(observed, "\n")
	var bindFailures []string
	for _, candidate := range candidates {
		network := "tcp6"
		if candidate.address.Is4() {
			network = "tcp4"
		}
		listener, listenErr := net.Listen(
			network,
			netip.AddrPortFrom(candidate.address, port).String(),
		)
		if listenErr == nil {
			t.Cleanup(func() { _ = listener.Close() })
			seatbeltProxyFloorServeEcho(t, listener)
			return listener, inventory
		}
		bindFailures = append(bindFailures, fmt.Sprintf(
			"%s %s flags=%s: %v",
			candidate.interfaceName, candidate.address, candidate.flags, listenErr))
	}
	t.Fatalf("no active non-loopback local address could bind the proxy's port %d; this runner cannot execute the required same-port/different-local-address probe\nrunner interfaces:\n%s\nbind attempts:\n%s",
		port, inventory, strings.Join(bindFailures, "\n"))
	return nil, inventory
}
