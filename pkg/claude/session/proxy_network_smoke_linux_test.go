//go:build linux

package session

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
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
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The §8.1 tests 1–2 boundary for the proxy filtering engine. These are CI-only
// and must never be run locally: they build a real bubblewrap sandbox and prove
// enforcement by executing denials inside it.
//
// # Runner fixture contract
//
// The job supplies the same live fixture the packet smoke uses — two adjacent
// addresses reachable from a separate network namespace, an allowed and a
// denied port — through these variables:
//
//	TCLAUDE_FILTERED_ALLOWED_ADDR    an address with a listener on ALLOWED_PORT
//	TCLAUDE_FILTERED_ADJACENT_ADDR   an address OUTSIDE the allowed prefix
//	TCLAUDE_FILTERED_ALLOWED_PREFIX  a CIDR covering ALLOWED_ADDR only
//	TCLAUDE_FILTERED_ALLOWED_PORT    a port ALLOWED_ADDR accepts
//	TCLAUDE_FILTERED_DENIED_PORT     a port the policy narrows away
//
// and additionally maps these names in the HOST's /etc/hosts, because the proxy
// resolves names host-side:
//
//	allowed.proxy.tclaude.test   -> TCLAUDE_FILTERED_ALLOWED_ADDR
//	sibling.proxy.tclaude.test   -> TCLAUDE_FILTERED_ALLOWED_ADDR
//	denied.proxy.tclaude.test    -> TCLAUDE_FILTERED_ALLOWED_ADDR
//	private.proxy.tclaude.test   -> TCLAUDE_FILTERED_ADJACENT_ADDR
//
// The sibling name resolves to the SAME address as the allowed name on purpose:
// that is what makes the refusal of an unauthored name meaningful rather than a
// side effect of it pointing somewhere unreachable.
const (
	proxySmokeEnv             = "TCLAUDE_FILTERED_PROXY_SMOKE"
	proxySmokeFloorHelperEnv  = "TCLAUDE_FILTERED_PROXY_FLOOR_HELPER"
	proxySmokePolicyHelperEnv = "TCLAUDE_FILTERED_PROXY_POLICY_HELPER"
	proxySmokeHostAllowedEnv  = "TCLAUDE_FILTERED_PROXY_HOST_ALLOWED_PORT"
	proxySmokeHostDeniedEnv   = "TCLAUDE_FILTERED_PROXY_HOST_DENIED_PORT"

	proxySmokeAllowedHost = "allowed.proxy.tclaude.test"
	proxySmokeSiblingHost = "sibling.proxy.tclaude.test"
	proxySmokeDeniedHost  = "denied.proxy.tclaude.test"
	proxySmokePrivateHost = "private.proxy.tclaude.test"

	// proxySmokePublicResolver is a well-known public resolver. The floor smoke
	// proves the sandbox cannot reach it; no query is ever answered.
	proxySmokePublicResolver = "1.1.1.1:53"

	proxySmokeDialTimeout = 3 * time.Second
)

// proxySmokeFixture is the runner-supplied live fixture, read once and passed
// to both halves through the environment the sandbox inherits.
type proxySmokeFixture struct {
	AllowedAddr   string
	AdjacentAddr  string
	AllowedPrefix string
	AllowedPort   int
	DeniedPort    int
	HostAllowed   int
	HostDenied    int
}

// proxySmokeFixtureFromEnv reads the complete fixture. It is what the
// in-sandbox helper uses: every value, including the host-loopback ports the
// launching test chose, arrives through the environment the sandbox inherits.
func proxySmokeFixtureFromEnv(t *testing.T) proxySmokeFixture {
	t.Helper()
	fixture := proxySmokeRunnerFixture(t)
	fixture.HostAllowed = requireFilteredSmokePort(t, proxySmokeHostAllowedEnv)
	fixture.HostDenied = requireFilteredSmokePort(t, proxySmokeHostDeniedEnv)
	return fixture
}

// proxySmokeRunnerFixture reads only what the CI job supplies. The
// host-loopback halves are owned by the launching test, which starts those
// servers itself so the job never has to keep two more ports in step with the
// authored policy.
func proxySmokeRunnerFixture(t *testing.T) proxySmokeFixture {
	t.Helper()
	return proxySmokeFixture{
		AllowedAddr:   requireFilteredSmokeEnv(t, filteredGatewayAllowedAddrEnv),
		AdjacentAddr:  requireFilteredSmokeEnv(t, filteredGatewayAdjacentAddrEnv),
		AllowedPrefix: requireFilteredSmokeEnv(t, filteredGatewayAllowedPrefixEnv),
		AllowedPort:   requireFilteredSmokePort(t, filteredGatewayAllowedPortEnv),
		DeniedPort:    requireFilteredSmokePort(t, filteredGatewayDeniedPortEnv),
	}
}

// proxySmokeRules is the authored policy both smokes launch under. It is
// deliberately one policy: the floor smoke and the policy smoke must not be
// able to disagree about what was authored.
func proxySmokeRules(fixture proxySmokeFixture) sandboxpolicy.NetworkRules {
	return sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Host: proxySmokeAllowedHost, Ports: []int{fixture.AllowedPort}},
			// Authored so the deny row below has an overlapping allow to beat.
			{Host: proxySmokeDeniedHost, Ports: []int{fixture.AllowedPort}},
			// Authored so the private-destination refusal is proven by the
			// RESOLVED address rather than by the name being unauthorized.
			{Host: proxySmokePrivateHost, Ports: []int{fixture.AllowedPort}},
			// Clears the fixture address space for names that resolve into it,
			// and is itself the literal-target rule.
			{CIDR: fixture.AllowedPrefix, Ports: []int{fixture.AllowedPort}},
			{Loopback: true, Ports: []int{fixture.HostAllowed}},
		},
		Deny: []sandboxpolicy.NetworkAllowEntry{
			{Host: proxySmokeDeniedHost},
		},
	}
}

// proxySmokeCase is one policy question asked over both carriages.
type proxySmokeCase struct {
	Name    string
	Target  string
	Literal bool
	Allowed bool
}

// proxySmokePolicyCases is the single expectation list. Both carriages are
// asked exactly these questions and must answer identically; a per-carriage
// expectation list is exactly the drift this shape prevents.
func proxySmokePolicyCases(fixture proxySmokeFixture) []proxySmokeCase {
	return []proxySmokeCase{
		{
			Name:    "authored host on its authored port",
			Target:  net.JoinHostPort(proxySmokeAllowedHost, strconv.Itoa(fixture.AllowedPort)),
			Allowed: true,
		},
		{
			// Resolves to the SAME address as the authored name, and into the
			// authored CIDR. Only the name identity separates them.
			Name:    "sibling name is not the authored name",
			Target:  net.JoinHostPort(proxySmokeSiblingHost, strconv.Itoa(fixture.AllowedPort)),
			Allowed: false,
		},
		{
			Name:    "authored host on a narrowed-away port",
			Target:  net.JoinHostPort(proxySmokeAllowedHost, strconv.Itoa(fixture.DeniedPort)),
			Allowed: false,
		},
		{
			Name:    "deny row beats an overlapping allow",
			Target:  net.JoinHostPort(proxySmokeDeniedHost, strconv.Itoa(fixture.AllowedPort)),
			Allowed: false,
		},
		{
			Name:    "IP literal inside the authored CIDR",
			Target:  net.JoinHostPort(fixture.AllowedAddr, strconv.Itoa(fixture.AllowedPort)),
			Literal: true,
			Allowed: true,
		},
		{
			Name:    "IP literal outside the authored CIDR",
			Target:  net.JoinHostPort(fixture.AdjacentAddr, strconv.Itoa(fixture.AllowedPort)),
			Literal: true,
			Allowed: false,
		},
		{
			// The name is authored; its ANSWER is private space no authored
			// CIDR covers. This is the rebinding route the blocker closes.
			Name:    "authored name resolving into unauthorized private space",
			Target:  net.JoinHostPort(proxySmokePrivateHost, strconv.Itoa(fixture.AllowedPort)),
			Allowed: false,
		},
		{
			Name:    "authored loopback row reaches the host",
			Target:  net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.HostAllowed)),
			Literal: true,
			Allowed: true,
		},
		{
			Name:    "unauthored host loopback port",
			Target:  net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.HostDenied)),
			Literal: true,
			Allowed: false,
		},
	}
}

// TestTclaudeLayerProxyFloorSmoke is §8.1 test 1: the floor, proved by failure.
// Inside a real proxy-engine launch, every route out except the proxy must be
// absent, and each denial must be OBSERVED — a green run that found nothing to
// do would be indistinguishable from a broken one, so the helper reports each
// refusal it actually executed and this boundary requires every report.
func TestTclaudeLayerProxyFloorSmoke(t *testing.T) {
	if os.Getenv(proxySmokeEnv) != "1" {
		t.Skip("set TCLAUDE_FILTERED_PROXY_SMOKE=1 on the executing Linux CI boundary")
	}
	_, output := runProxySmokeLaunch(t,
		"^TestTclaudeLayerProxyFloorHelper$",
		[]string{proxySmokeFloorHelperEnv + "=1"})
	for _, marker := range []string{
		"proxy-floor: direct TCP to an allowed destination: refused",
		"proxy-floor: UDP to an allowed destination: refused",
		"proxy-floor: DNS to a public resolver: refused",
		"proxy-floor: ICMP echo: refused",
		"proxy-floor: local name resolution: refused",
		"proxy-floor: the proxy port answers: carried",
		"proxy-floor: proxy discovery is launcher-owned: verified",
	} {
		assert.Contains(t, output, marker,
			"the floor smoke must observe each denial executing")
	}
}

// TestTclaudeLayerProxyPolicySmoke is §8.1 test 2: the policy engine end to end
// through a real launch, with every case asked over BOTH carriages.
func TestTclaudeLayerProxyPolicySmoke(t *testing.T) {
	if os.Getenv(proxySmokeEnv) != "1" {
		t.Skip("set TCLAUDE_FILTERED_PROXY_SMOKE=1 on the executing Linux CI boundary")
	}
	fixture, output := runProxySmokeLaunch(t,
		"^TestTclaudeLayerProxyPolicyHelper$",
		[]string{
			proxySmokePolicyHelperEnv + "=1",
			// §4.6: an ambient proxy variable on the HOST must not become the
			// filtering proxy's upstream. It is set on the supervisor's own
			// environment, which is where a chaining implementation would read
			// it; the allowed cases below still have to succeed.
			"HTTP_PROXY=http://127.0.0.1:1",
			"HTTPS_PROXY=http://127.0.0.1:1",
			"ALL_PROXY=socks5://127.0.0.1:1",
		})
	for _, testCase := range proxySmokePolicyCases(fixture) {
		for _, carriage := range []string{"http", "socks5"} {
			verdict := "refused"
			if testCase.Allowed {
				verdict = "carried"
			}
			assert.Contains(t, output,
				fmt.Sprintf("proxy-case: %s/%s: %s",
					testCase.Name, carriage, verdict),
				"every case must be observed over both carriages")
		}
	}
	assert.Contains(t, output,
		"proxy-case: SOCKS5 UDP ASSOCIATE/socks5: command not supported",
		"the UDP exclusion must be legible rather than silent")
}

// runProxySmokeLaunch builds one real proxy-engine tclaude-layer launch and
// runs the named helper test inside it.
func runProxySmokeLaunch(
	t *testing.T,
	helperTest string,
	helperEnv []string,
) (proxySmokeFixture, string) {
	t.Helper()
	fixture := proxySmokeRunnerFixture(t)
	// The launching test owns both host-loopback halves: an allowed port the
	// authored loopback row reaches, and a LIVE denied port, so a refusal there
	// is the policy answering rather than nothing listening.
	fixture.HostAllowed = proxySmokeLoopbackServer(t)
	fixture.HostDenied = proxySmokeLoopbackServer(t)
	tclaudeBinary := strings.TrimSpace(os.Getenv(filteredGatewayTclaudeBinaryEnv))
	require.NotEmpty(t, tclaudeBinary)
	tclaudeBinary, err := filepath.Abs(tclaudeBinary)
	require.NoError(t, err)

	rules := proxySmokeRules(fixture)
	bwrapBinary, launchSandbox, err := ResolveTclaudeLayerForEngine(
		sandboxpolicy.NetworkFiltered,
		sandboxpolicy.RootConstructed,
		sandboxpolicy.NetworkEngineProxy,
	)
	require.NoError(t, err)
	// The proxy engine's prerequisites are bubblewrap and pidfds alone: a green
	// resolve here on a runner without pasta or nft is itself part of the
	// evidence, so record what the launch claims to run.
	t.Logf("proxy launch boundary: %s", launchSandbox.Source)

	previousRelay := tclaudeLayerRelayPrefix
	tclaudeLayerRelayPrefix = func() string {
		return clcommon.ShellQuoteArg(tclaudeBinary) +
			" session " + tclaudeLayerWinchRelayCommand
	}
	t.Cleanup(func() { tclaudeLayerRelayPrefix = previousRelay })

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	smokeBase := filepath.Join(home, ".cache")
	require.NoError(t, os.MkdirAll(smokeBase, 0o700))
	root, err := os.MkdirTemp(smokeBase, "tclaude-proxy-smoke-")
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
	helperBinary := filepath.Join(helperDir, "proxy-smoke-helper")
	copyTestBinary(t, os.Args[0], helperBinary)

	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.Network = &rules
	stateRoot := filepath.Join(smokeHome, "."+harness.DefaultName)
	require.NoError(t, os.MkdirAll(stateRoot, 0o700))
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName:   harness.DefaultName,
		Cwd:           helperDir,
		Snapshot:      &snapshot,
		StateRoot:     stateRoot,
		NetworkEngine: sandboxpolicy.NetworkEngineProxy,
	})
	require.NoError(t, err)
	command, err := WrapTclaudeLayerSpec(bwrapBinary, spec,
		clcommon.ShellQuoteArg(helperBinary)+" -test.run="+helperTest+" -test.v")
	require.NoError(t, err)
	require.Contains(t, command, "--proxy-network-policy",
		"the launch must start the proxy supervisor rather than the packet one")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Env = append(proxySmokeHelperEnv(os.Environ(), fixture), helperEnv...)
	output, runErr := cmd.CombinedOutput()
	require.NoErrorf(t, runErr, "proxy smoke output:\n%s", output)
	require.NoError(t, ctx.Err())
	return fixture, string(output)
}

// proxySmokeLoopbackServer starts a host-loopback TCP listener and returns its
// port.
func proxySmokeLoopbackServer(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	address, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	return address.Port
}

// proxySmokeHelperEnv passes the live fixture to the in-sandbox helper. The
// proxy discovery variables are deliberately NOT passed: the launcher owns
// them, and the floor smoke asserts the values the sandbox sees came from it.
func proxySmokeHelperEnv(
	environ []string,
	fixture proxySmokeFixture,
) []string {
	return append(append([]string(nil), environ...),
		filteredGatewayAllowedAddrEnv+"="+fixture.AllowedAddr,
		filteredGatewayAdjacentAddrEnv+"="+fixture.AdjacentAddr,
		filteredGatewayAllowedPrefixEnv+"="+fixture.AllowedPrefix,
		filteredGatewayAllowedPortEnv+"="+strconv.Itoa(fixture.AllowedPort),
		filteredGatewayDeniedPortEnv+"="+strconv.Itoa(fixture.DeniedPort),
		proxySmokeHostAllowedEnv+"="+strconv.Itoa(fixture.HostAllowed),
		proxySmokeHostDeniedEnv+"="+strconv.Itoa(fixture.HostDenied),
	)
}

// TestTclaudeLayerProxyFloorHelper runs INSIDE the sandbox. Every check here is
// a denial that must execute: it reports what it observed so the outer boundary
// can require the observation, and fails the launch if any refusal did not
// happen.
func TestTclaudeLayerProxyFloorHelper(t *testing.T) {
	if os.Getenv(proxySmokeFloorHelperEnv) != "1" {
		t.Skip("proxy-network floor smoke helper")
	}
	fixture := proxySmokeFixtureFromEnv(t)
	allowed := net.JoinHostPort(fixture.AllowedAddr, strconv.Itoa(fixture.AllowedPort))

	conn, err := net.DialTimeout("tcp", allowed, proxySmokeDialTimeout)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("direct TCP to %s succeeded inside the floor", allowed)
	}
	fmt.Printf("proxy-floor: direct TCP to an allowed destination: refused (%v)\n", err)

	udpRefusal, err := proxySmokeSendUDP(allowed)
	require.NoError(t, err)
	require.NotNil(t, udpRefusal, "a UDP datagram left the sandbox to %s", allowed)
	fmt.Printf("proxy-floor: UDP to an allowed destination: refused (%v)\n",
		udpRefusal)

	dnsUDPRefusal, err := proxySmokeSendUDP(proxySmokePublicResolver)
	require.NoError(t, err)
	require.NotNil(t, dnsUDPRefusal, "a DNS datagram left the sandbox")
	dnsConn, dnsErr := net.DialTimeout(
		"tcp", proxySmokePublicResolver, proxySmokeDialTimeout)
	if dnsErr == nil {
		_ = dnsConn.Close()
		t.Fatal("TCP DNS to a public resolver succeeded inside the floor")
	}
	fmt.Printf("proxy-floor: DNS to a public resolver: refused (%v)\n", dnsErr)

	icmpRefusal, err := proxySmokePingEcho(fixture.AllowedAddr)
	require.NoError(t, err)
	require.NotNil(t, icmpRefusal, "an ICMP echo left the sandbox")
	fmt.Printf("proxy-floor: ICMP echo: refused (%v)\n", icmpRefusal)

	// The namespace has no resolver at all, which is what makes socks5h and
	// CONNECT-by-name load-bearing rather than a preference. The CI job maps
	// these fixture names in the HOST's /etc/hosts, so this is a live check
	// that the floor replaced them rather than a check that nothing existed.
	hosts, err := os.ReadFile("/etc/hosts")
	require.NoError(t, err)
	assert.NotContains(t, string(hosts), "proxy.tclaude.test",
		"the floor must replace the host's name mappings")
	_, resolveErr := net.LookupHost(proxySmokeAllowedHost)
	require.Error(t, resolveErr)
	fmt.Printf("proxy-floor: local name resolution: refused (%v)\n", resolveErr)

	endpoint := proxySmokeProxyEndpoint(t)
	status, err := proxySmokeHTTPConnect(endpoint,
		net.JoinHostPort(proxySmokeAllowedHost, strconv.Itoa(fixture.AllowedPort)))
	require.NoError(t, err)
	require.Equal(t, 200, status,
		"the proxy port must be the one thing that answers")
	fmt.Println("proxy-floor: the proxy port answers: carried")

	// The injected discovery is the launcher's, in the exact spellings the
	// design fixes, and nothing was inherited.
	assert.Equal(t, "http://"+endpoint, os.Getenv("HTTP_PROXY"))
	assert.Equal(t, "http://"+endpoint, os.Getenv("http_proxy"))
	assert.Equal(t, "http://"+endpoint, os.Getenv("HTTPS_PROXY"))
	assert.Equal(t, "http://"+endpoint, os.Getenv("https_proxy"))
	assert.Equal(t, "socks5h://"+endpoint, os.Getenv("ALL_PROXY"))
	assert.Equal(t, "socks5h://"+endpoint, os.Getenv("all_proxy"))
	assert.Equal(t, "", os.Getenv("NO_PROXY"))
	assert.Equal(t, "", os.Getenv("no_proxy"))
	_, noProxySet := os.LookupEnv("NO_PROXY")
	assert.True(t, noProxySet, "NO_PROXY must be present and empty, not absent")
	fmt.Println("proxy-floor: proxy discovery is launcher-owned: verified")
}

// TestTclaudeLayerProxyPolicyHelper runs INSIDE the sandbox and asks every
// policy case over both carriages.
func TestTclaudeLayerProxyPolicyHelper(t *testing.T) {
	if os.Getenv(proxySmokePolicyHelperEnv) != "1" {
		t.Skip("proxy-network policy smoke helper")
	}
	fixture := proxySmokeFixtureFromEnv(t)
	endpoint := proxySmokeProxyEndpoint(t)
	for _, testCase := range proxySmokePolicyCases(fixture) {
		t.Run(testCase.Name, func(t *testing.T) {
			status, err := proxySmokeHTTPConnect(endpoint, testCase.Target)
			require.NoError(t, err, "the proxy must answer rather than hang up")
			socksReply, err := proxySmokeSOCKS5Connect(
				endpoint, testCase.Target, testCase.Literal)
			require.NoError(t, err)

			httpCarried := status == 200
			socksCarried := socksReply == 0
			// The two carriages are compared to each other before either is
			// compared to the expectation: a drift between them is a distinct
			// failure from a policy being wrong.
			require.Equalf(t, httpCarried, socksCarried,
				"carriages disagreed for %s (HTTP %d, SOCKS5 reply %d)",
				testCase.Target, status, socksReply)
			require.Equal(t, testCase.Allowed, httpCarried)
			if !testCase.Allowed {
				require.Equal(t, 403, status,
					"a refusal must be a legible policy answer")
				require.Equal(t, byte(2), socksReply,
					"SOCKS5 refusals use connection-not-allowed")
			}
			verdict := "refused"
			if testCase.Allowed {
				verdict = "carried"
			}
			fmt.Printf("proxy-case: %s/http: %s\n", testCase.Name, verdict)
			fmt.Printf("proxy-case: %s/socks5: %s\n", testCase.Name, verdict)
		})
	}

	reply, err := proxySmokeSOCKS5UDPAssociate(endpoint)
	require.NoError(t, err)
	require.Equal(t, byte(7), reply,
		"UDP ASSOCIATE is excluded deliberately and must say so")
	fmt.Println(
		"proxy-case: SOCKS5 UDP ASSOCIATE/socks5: command not supported")
}

// proxySmokeProxyEndpoint reads the launcher-injected discovery. Deriving the
// endpoint from the environment rather than from a test parameter means the
// smoke exercises the same discovery a harness would.
func proxySmokeProxyEndpoint(t *testing.T) string {
	t.Helper()
	value := os.Getenv("HTTP_PROXY")
	require.NotEmpty(t, value, "the launcher must inject proxy discovery")
	endpoint := strings.TrimPrefix(value, "http://")
	require.NotEqual(t, value, endpoint)
	return endpoint
}

// proxySmokeSendUDP reports the REFUSAL, not the attempt: it returns the error
// the floor produced, and a nil error means the datagram left the sandbox.
//
// The polarity is the whole point. A helper that returned an error for both
// outcomes would pass a `require.Error` assertion whether the floor held or
// leaked, which is exactly the vacuous shape these smokes exist to avoid.
func proxySmokeSendUDP(address string) (refusal error, err error) {
	conn, err := net.DialTimeout("udp", address, proxySmokeDialTimeout)
	if err != nil {
		return err, nil
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(proxySmokeDialTimeout)); err != nil {
		return nil, err
	}
	// An empty namespace has no route, so the send itself fails.
	if _, writeErr := conn.Write([]byte("tclaude-proxy-floor")); writeErr != nil {
		return writeErr, nil
	}
	return nil, nil
}

// TestProxySmokeSendUDPReportsTrafficThatLeaves guards the polarity the floor
// smoke's anti-vacuous property rests on. It needs no sandbox and no gate: on
// an ordinary host the datagram leaves, so the helper must report NO refusal.
// A helper that returned an error for both outcomes would satisfy the floor
// smoke whether the floor held or leaked, and would fail here.
func TestProxySmokeSendUDPReportsTrafficThatLeaves(t *testing.T) {
	refusal, err := proxySmokeSendUDP("127.0.0.1:9")
	require.NoError(t, err)
	assert.NoError(t, refusal,
		"a datagram that left the host must not be reported as a refusal")
}

// proxySmokePingEcho attempts an ICMP echo. Like proxySmokeSendUDP it returns
// the refusal: either the socket cannot be created or the echo cannot be sent,
// and a nil refusal means an echo left the sandbox.
func proxySmokePingEcho(address string) (refusal error, err error) {
	conn, err := net.Dial("ip4:icmp", address)
	if err != nil {
		return err, nil
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(proxySmokeDialTimeout)); err != nil {
		return nil, err
	}
	echo := []byte{8, 0, 0, 0, 0, 1, 0, 1}
	if _, writeErr := conn.Write(echo); writeErr != nil {
		return writeErr, nil
	}
	return nil, nil
}

// proxySmokeHTTPConnect asks one policy question over the HTTP CONNECT
// carriage and returns the status code the proxy answered with.
func proxySmokeHTTPConnect(endpoint, target string) (int, error) {
	conn, err := net.DialTimeout("tcp", endpoint, proxySmokeDialTimeout)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return 0, err
	}
	if _, err := fmt.Fprintf(conn,
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return 0, err
	}
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(strings.TrimSpace(status))
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed proxy status %q", status)
	}
	return strconv.Atoi(fields[1])
}

// proxySmokeSOCKS5Connect asks the identical question over the SOCKS5 carriage
// and returns the reply code. Literal targets are sent as an address type so
// the literal path is genuinely exercised rather than re-parsed from a name.
func proxySmokeSOCKS5Connect(
	endpoint, target string,
	literal bool,
) (byte, error) {
	conn, request, err := proxySmokeSOCKS5Handshake(endpoint)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return 0, err
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil {
		return 0, err
	}
	body := []byte{5, 1, 0}
	if literal {
		addr := net.ParseIP(host)
		if addr == nil {
			return 0, fmt.Errorf("literal target %q is not an address", host)
		}
		if v4 := addr.To4(); v4 != nil {
			body = append(body, 1)
			body = append(body, v4...)
		} else {
			body = append(body, 4)
			body = append(body, addr.To16()...)
		}
	} else {
		if len(host) > 255 {
			return 0, fmt.Errorf("name target %q is too long", host)
		}
		body = append(body, 3, byte(len(host)))
		body = append(body, host...)
	}
	body = binary.BigEndian.AppendUint16(body, uint16(portNumber))
	if _, err := conn.Write(body); err != nil {
		return 0, err
	}
	return proxySmokeSOCKS5Reply(request)
}

// proxySmokeSOCKS5UDPAssociate asks for the one command v1 excludes.
func proxySmokeSOCKS5UDPAssociate(endpoint string) (byte, error) {
	conn, request, err := proxySmokeSOCKS5Handshake(endpoint)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	body := []byte{5, 3, 0, 1, 0, 0, 0, 0}
	body = binary.BigEndian.AppendUint16(body, 0)
	if _, err := conn.Write(body); err != nil {
		return 0, err
	}
	return proxySmokeSOCKS5Reply(request)
}

func proxySmokeSOCKS5Handshake(
	endpoint string,
) (net.Conn, *bufio.Reader, error) {
	conn, err := net.DialTimeout("tcp", endpoint, proxySmokeDialTimeout)
	if err != nil {
		return nil, nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	reader := bufio.NewReader(conn)
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(reader, greeting); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if greeting[0] != 5 || greeting[1] != 0 {
		_ = conn.Close()
		return nil, nil, fmt.Errorf(
			"unexpected SOCKS5 greeting %v", greeting)
	}
	return conn, reader, nil
}

// proxySmokeSOCKS5Reply reads one reply and returns its REP byte, draining the
// bound address so a caller could continue on an accepted tunnel.
func proxySmokeSOCKS5Reply(reader *bufio.Reader) (byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, err
	}
	if header[0] != 5 {
		return 0, fmt.Errorf("unexpected SOCKS5 reply version %d", header[0])
	}
	var addrLen int
	switch header[3] {
	case 1:
		addrLen = 4
	case 4:
		addrLen = 16
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(reader, length); err != nil {
			return 0, err
		}
		addrLen = int(length[0])
	default:
		return 0, fmt.Errorf("unexpected SOCKS5 address type %d", header[3])
	}
	if _, err := io.ReadFull(reader, make([]byte, addrLen+2)); err != nil {
		return 0, err
	}
	return header[1], nil
}
