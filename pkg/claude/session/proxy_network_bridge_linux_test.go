//go:build linux

package session

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"golang.org/x/sys/unix"
)

// proxyBridgeTestPlan renders a launch plan for one network rule set under one
// engine selection, through exactly the production render seam.
func proxyBridgeTestPlan(
	t *testing.T,
	rules sandboxpolicy.NetworkRules,
	engine sandboxpolicy.NetworkEngine,
) sandboxpolicy.MountPlan {
	t.Helper()
	plan, err := sandboxpolicy.RenderMountPlanWithEngine(
		sandboxpolicy.EffectiveProfile{Network: &rules}, engine)
	require.NoError(t, err)
	return plan
}

// proxyBridgeConstructedRootFixture satisfies the launch-contract precondition
// every constructed root shares: the canonical agentd socket path must exist,
// because the floor allowlists exactly it. HOME is redirected first so the
// fixture is hermetic rather than depending on the developer or the runner
// already having a daemon.
//
// The placeholder is an ordinary file rather than a listening socket. These
// tests assert on the ARGUMENTS a plan renders, and the argument builder asks
// only whether the path exists; binding a real socket would additionally have
// to fit the 108-byte sockaddr limit under an arbitrary temporary HOME, which
// is a constraint about the fixture rather than about the floor.
func proxyBridgeConstructedRootFixture(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	floor := sandboxpolicy.AgentdSocketFloor()
	require.NotEmpty(t, floor)
	require.NoError(t, os.MkdirAll(filepath.Dir(floor[0]), 0o700))
	placeholder, err := os.OpenFile(
		floor[0], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	require.NoError(t, placeholder.Close())
}

func proxyBridgeDiscriminatingRules() sandboxpolicy.NetworkRules {
	return sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Domain: "example.com", Ports: []int{443}},
		},
	}
}

// TestProxyNetworkFloorReusesTheIsolatedConstruction is the floor half of the
// milestone: a proxy-engine launch must build the isolated posture's namespace
// and must NOT acquire the packet posture's user namespace, uid-0 mapping or
// capabilities. Reverting the floor mapping fails this on the --unshare-user
// assertion.
func TestProxyNetworkFloorReusesTheIsolatedConstruction(t *testing.T) {
	proxyBridgeConstructedRootFixture(t)
	rules := proxyBridgeDiscriminatingRules()
	proxyArgs, err := bwrapArgs(nil,
		proxyBridgeTestPlan(t, rules, sandboxpolicy.NetworkEngineProxy))
	require.NoError(t, err)
	packetArgs, err := bwrapArgs(nil,
		proxyBridgeTestPlan(t, rules, sandboxpolicy.NetworkEnginePacket))
	require.NoError(t, err)
	isolatedArgs, err := bwrapArgs(nil, proxyBridgeTestPlan(t,
		sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
		sandboxpolicy.NetworkEngineProxy))
	require.NoError(t, err)

	assert.Contains(t, proxyArgs, "--unshare-net")
	assert.Contains(t, proxyArgs, "--unshare-pid")
	assert.NotContains(t, proxyArgs, "--unshare-user",
		"the proxy floor needs no user namespace")
	assert.NotContains(t, proxyArgs, "--uid")
	assert.NotContains(t, proxyArgs, "--gid")

	// The packet posture is unchanged by this milestone, and the proxy floor is
	// the isolated construction rather than a third variant of it.
	assert.Contains(t, packetArgs, "--unshare-user")
	assert.Contains(t, packetArgs, "--uid")
	assert.Equal(t, isolatedArgs, proxyArgs,
		"the proxy floor is the isolated posture's construction, unchanged")
}

// TestProxyNetworkFloorOmitsThePacketResolverFilesystem proves the proxy floor
// does not inherit the packet relay's private /run, which exists only so the
// in-namespace resolver can be rebound.
func TestProxyNetworkFloorOmitsThePacketResolverFilesystem(t *testing.T) {
	proxyBridgeConstructedRootFixture(t)
	rules := proxyBridgeDiscriminatingRules()
	proxyArgs, err := bwrapArgs(nil,
		proxyBridgeTestPlan(t, rules, sandboxpolicy.NetworkEngineProxy))
	require.NoError(t, err)
	packetArgs, err := bwrapArgs(nil,
		proxyBridgeTestPlan(t, rules, sandboxpolicy.NetworkEnginePacket))
	require.NoError(t, err)
	assert.True(t, proxyBridgeHasTmpfs(packetArgs, "/run"))
	assert.False(t, proxyBridgeHasTmpfs(proxyArgs, "/run"))
}

func proxyBridgeHasTmpfs(args []string, path string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--tmpfs" && args[i+1] == path {
			return true
		}
	}
	return false
}

// TestTclaudeLayerEnginePrefixStartsExactlyOneSupervisor proves the launch
// renders the supervisor for the engine the plan deploys, and only that one.
func TestTclaudeLayerEnginePrefixStartsExactlyOneSupervisor(t *testing.T) {
	rules := proxyBridgeDiscriminatingRules()

	proxyPrefix, err := tclaudeLayerEnginePrefix(
		proxyBridgeTestPlan(t, rules, sandboxpolicy.NetworkEngineProxy))
	require.NoError(t, err)
	assert.Contains(t, proxyPrefix, "--proxy-network-policy")
	assert.NotContains(t, proxyPrefix, "--filtered-network-policy")

	packetPrefix, err := tclaudeLayerEnginePrefix(
		proxyBridgeTestPlan(t, rules, sandboxpolicy.NetworkEnginePacket))
	require.NoError(t, err)
	assert.Contains(t, packetPrefix, "--filtered-network-policy")
	assert.NotContains(t, packetPrefix, "--proxy-network-policy")

	// A policy that needs no engine starts no supervisor under either
	// selection.
	loopbackPrefix, err := tclaudeLayerEnginePrefix(proxyBridgeTestPlan(t,
		sandboxpolicy.NetworkRules{
			Mode:  sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{Loopback: true}},
		},
		sandboxpolicy.NetworkEngineProxy))
	require.NoError(t, err)
	assert.NotContains(t, loopbackPrefix, "--proxy-network-policy")
}

// TestProxyNetworkRelayPolicyRoundTripsThroughTheEvaluator proves the encoded
// policy the supervisor receives is the compiled policy the plan carries.
func TestProxyNetworkRelayPolicyRoundTrips(t *testing.T) {
	plan := proxyBridgeTestPlan(t,
		proxyBridgeDiscriminatingRules(), sandboxpolicy.NetworkEngineProxy)
	encoded, err := encodeProxyNetworkRelayPolicy(plan)
	require.NoError(t, err)
	decoded, err := parseProxyNetworkRelayPolicy(encoded)
	require.NoError(t, err)
	assert.Equal(t, plan.FilteredNetwork.ProtocolContract, decoded.ProtocolContract)
	assert.Equal(t, plan.FilteredNetwork.DefaultVerdict, decoded.DefaultVerdict)
	assert.Equal(t, plan.FilteredNetwork.Rules, decoded.Rules)
	assert.Empty(t, decoded.DenyRules)
	// Re-encoding the decoded policy reproduces the wire form byte for byte,
	// which is the property that matters: the supervisor enforces the policy the
	// plan compiled, not a lossy copy of it.
	reencoded, err := encodeProxyNetworkRelayPolicy(sandboxpolicy.MountPlan{
		NetworkPosture:  plan.NetworkPosture,
		NetworkEngine:   plan.NetworkEngine,
		FilteredNetwork: &decoded,
	})
	require.NoError(t, err)
	assert.Equal(t, encoded, reencoded)
}

func TestProxyNetworkRelayPolicyRefusesMalformedInput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		encoded string
	}{
		{name: "not base64", encoded: "!!!!"},
		{name: "not JSON", encoded: proxyBridgeEncode("{")},
		{name: "unknown field", encoded: proxyBridgeEncode(`{"nope":1}`)},
		{name: "trailing data", encoded: proxyBridgeEncode(`{} {}`)},
		{name: "wrong protocol contract", encoded: proxyBridgeEncode(`{}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseProxyNetworkRelayPolicy(tc.encoded)
			assert.Error(t, err)
		})
	}
}

func proxyBridgeEncode(payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// TestProxyNetworkSandboxEnvReplacesInheritedProxyDiscovery is the env-injection
// half. Reverting the scrub leaves the inherited value in place and fails here.
func TestProxyNetworkSandboxEnvReplacesInheritedProxyDiscovery(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"HTTP_PROXY=http://attacker.invalid:3128",
		"http_proxy=http://attacker.invalid:3128",
		"HTTPS_PROXY=http://attacker.invalid:3128",
		"https_proxy=http://attacker.invalid:3128",
		"ALL_PROXY=socks5://attacker.invalid:1080",
		"all_proxy=socks5://attacker.invalid:1080",
		"NO_PROXY=localhost,10.0.0.0/8",
		"no_proxy=localhost,10.0.0.0/8",
		"HOME=/home/agent",
	}
	got := proxyNetworkSandboxEnv(environ, 41234)
	assert.NotContains(t, strings.Join(got, "\n"), "attacker.invalid",
		"an inherited proxy destination must never survive into the sandbox")
	for _, want := range []string{
		"PATH=/usr/bin",
		"HOME=/home/agent",
		"HTTP_PROXY=http://127.0.0.1:41234",
		"http_proxy=http://127.0.0.1:41234",
		"HTTPS_PROXY=http://127.0.0.1:41234",
		"https_proxy=http://127.0.0.1:41234",
		// The trailing h keeps name resolution at the proxy, where the authored
		// host and domain rows are evaluated.
		"ALL_PROXY=socks5h://127.0.0.1:41234",
		"all_proxy=socks5h://127.0.0.1:41234",
		"NO_PROXY=",
		"no_proxy=",
	} {
		assert.Contains(t, got, want)
	}
	for _, name := range proxyNetworkProxyVariables {
		assert.Equal(t, 1, proxyBridgeCountVariable(got, name),
			"%s must appear exactly once", name)
	}
}

func proxyBridgeCountVariable(environ []string, name string) int {
	count := 0
	for _, pair := range environ {
		if got, _, ok := strings.Cut(pair, "="); ok && got == name {
			count++
		}
	}
	return count
}

// TestAdoptProxyNetworkListenerRefusesDescriptorsOutsideTheContract proves the
// host adopts only what the bridge contract promises, and learns it from the
// kernel rather than from the sandbox's message.
func TestAdoptProxyNetworkListenerRefusesDescriptorsOutsideTheContract(t *testing.T) {
	t.Run("a loopback TCP listener is adopted", func(t *testing.T) {
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{
			IP: net.IPv4(127, 0, 0, 1),
		})
		require.NoError(t, err)
		file, err := listener.File()
		require.NoError(t, err)
		require.NoError(t, listener.Close())
		defer func() { _ = file.Close() }()
		adopted, err := adoptProxyNetworkListener(file)
		require.NoError(t, err)
		defer func() { _ = adopted.Close() }()
		address, ok := adopted.Addr().(*net.TCPAddr)
		require.True(t, ok)
		assert.True(t, address.IP.IsLoopback())
		assert.GreaterOrEqual(t, address.Port, proxyNetworkMinPort)
	})

	t.Run("a connected socket is refused", func(t *testing.T) {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		require.NoError(t, err)
		defer func() { _ = listener.Close() }()
		conn, err := net.Dial("tcp4", listener.Addr().String())
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		file, err := conn.(*net.TCPConn).File()
		require.NoError(t, err)
		defer func() { _ = file.Close() }()
		_, err = adoptProxyNetworkListener(file)
		assert.ErrorContains(t, err, "not listening")
	})

	t.Run("a datagram socket is refused", func(t *testing.T) {
		packet, err := net.ListenPacket("udp4", "127.0.0.1:0")
		require.NoError(t, err)
		defer func() { _ = packet.Close() }()
		file, err := packet.(*net.UDPConn).File()
		require.NoError(t, err)
		defer func() { _ = file.Close() }()
		_, err = adoptProxyNetworkListener(file)
		assert.ErrorContains(t, err, "not a stream socket")
	})

	t.Run("a non-loopback listener is refused", func(t *testing.T) {
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{})
		require.NoError(t, err)
		file, err := listener.File()
		require.NoError(t, err)
		require.NoError(t, listener.Close())
		defer func() { _ = file.Close() }()
		_, err = adoptProxyNetworkListener(file)
		assert.ErrorContains(t, err, "sandbox loopback")
	})

	t.Run("a regular file is refused", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "not-a-socket")
		require.NoError(t, err)
		defer func() { _ = file.Close() }()
		_, err = adoptProxyNetworkListener(file)
		assert.Error(t, err)
	})
}

// TestProxyNetworkBridgeServesThePolicyOnTheHandedOutListener exercises the
// complete host half against a real descriptor handoff: peer verification, the
// readiness token, adoption, and the proxy answering policy on the listener the
// peer bound. The peer here is the test process, which shares this process's
// network namespace, so the production /proc netns check passes for the same
// reason it does for a sandbox bootstrap.
func TestProxyNetworkBridgeServesThePolicyOnTheHandedOutListener(t *testing.T) {
	upstream := proxyBridgeLoopbackEcho(t)
	rules := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Loopback: true, Ports: []int{upstream}},
			// A loopback-only list needs no engine at all, so the policy must
			// discriminate for a proxy to be deployed for it.
			{Domain: "example.com", Ports: []int{443}},
		},
	}
	plan := proxyBridgeTestPlan(t, rules, sandboxpolicy.NetworkEngineProxy)
	encoded, err := encodeProxyNetworkRelayPolicy(plan)
	require.NoError(t, err)
	relay, err := prepareProxyNetworkRelay(encoded)
	require.NoError(t, err)
	defer relay.Close()
	require.True(t, relay.Active())

	endpoint := make(chan int, 1)
	handoffErr := make(chan error, 1)
	go func() { handoffErr <- proxyBridgeHandOutListener(relay.SyncDir, endpoint) }()

	require.NoError(t, relay.waitListenerReady(os.Getpid()))
	require.NoError(t, relay.releaseHarness())
	require.NoError(t, <-handoffErr)
	port := <-endpoint

	// An authored loopback port is carried.
	allowed := proxyBridgeConnect(t, port,
		net.JoinHostPort("127.0.0.1", fmt.Sprint(upstream)))
	assert.Equal(t, "HTTP/1.1 200", allowed)

	// An unauthored port on the same host is refused by the same proxy.
	refused := proxyBridgeConnect(t, port, "127.0.0.1:9")
	assert.NotEqual(t, "HTTP/1.1 200", refused)

	// Proxy exit is observable by the supervisor, which is what makes the
	// fail-closed teardown possible.
	require.NoError(t, relay.Server.Close())
	select {
	case <-relay.waitCh():
	case <-time.After(5 * time.Second):
		t.Fatal("proxy exit was not observed by the supervisor")
	}
}

// TestProxyNetworkBridgeRefusesAnUnverifiablePeer proves the handoff is gated
// on the peer's identity being provable against the sandbox this launch owns.
// A namespace identity the supervisor cannot read is refused rather than
// assumed, and a refused handoff starts no proxy — so a launch whose peer
// check fails ends with no network rather than an unfiltered one.
func TestProxyNetworkBridgeRefusesAnUnverifiablePeer(t *testing.T) {
	for _, tc := range []struct {
		name         string
		namespacePID func(t *testing.T) int
	}{
		{
			name:         "no namespace identity",
			namespacePID: func(*testing.T) int { return 0 },
		},
		{
			name: "namespace identity that cannot be read",
			namespacePID: func(t *testing.T) int {
				t.Helper()
				return proxyBridgeReapedPID(t)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := proxyBridgeTestPlan(t,
				proxyBridgeDiscriminatingRules(), sandboxpolicy.NetworkEngineProxy)
			encoded, err := encodeProxyNetworkRelayPolicy(plan)
			require.NoError(t, err)
			relay, err := prepareProxyNetworkRelay(encoded)
			require.NoError(t, err)
			defer relay.Close()

			endpoint := make(chan int, 1)
			go func() { _ = proxyBridgeHandOutListener(relay.SyncDir, endpoint) }()

			require.Error(t, relay.waitListenerReady(tc.namespacePID(t)))
			assert.Nil(t, relay.Server, "a refused handoff must not start a proxy")
		})
	}
}

// proxyBridgeReapedPID returns the PID of a process that has already exited and
// been reaped, so /proc/<pid>/ns/net cannot be read.
func proxyBridgeReapedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/true")
	require.NoError(t, cmd.Run())
	pid := cmd.Process.Pid
	if _, err := os.Readlink(
		filepath.Join("/proc", fmt.Sprint(pid), "ns/net")); err == nil {
		t.Skip("the reaped pid was reused before the check could run")
	}
	return pid
}

// proxyBridgeHandOutListener is the in-namespace bootstrap's exact wire
// behavior: bind an ephemeral loopback port, pass the descriptor with the
// readiness token, and wait for the release token.
func proxyBridgeHandOutListener(syncDir string, endpoint chan<- int) error {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return err
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return fmt.Errorf("no TCP address")
	}
	file, err := listener.File()
	_ = listener.Close()
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	sync, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{
		Name: filepath.Join(syncDir, "bootstrap.sock"),
		Net:  "unixpacket",
	})
	if err != nil {
		return err
	}
	defer func() { _ = sync.Close() }()
	if _, _, err := sync.WriteMsgUnix(
		[]byte(proxyNetworkBootstrapReady),
		unix.UnixRights(int(file.Fd())),
		nil,
	); err != nil {
		return err
	}
	if err := sync.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	buffer := make([]byte, 64)
	n, err := sync.Read(buffer)
	if err != nil {
		return err
	}
	if string(buffer[:n]) != proxyNetworkHarnessRelease {
		return fmt.Errorf("unexpected release token %q", buffer[:n])
	}
	endpoint <- address.Port
	return nil
}

// proxyBridgeConnect issues one HTTP CONNECT through the proxy and returns the
// status line.
func proxyBridgeConnect(t *testing.T, proxyPort int, target string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp4",
		net.JoinHostPort("127.0.0.1", fmt.Sprint(proxyPort)), 5*time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
	_, err = fmt.Fprintf(conn,
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	require.NoError(t, err)
	status, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	fields := strings.Fields(strings.TrimSpace(status))
	require.GreaterOrEqual(t, len(fields), 2)
	return fields[0] + " " + fields[1]
}

// proxyBridgeLoopbackEcho starts a host-loopback HTTP server and returns its
// port. It is the destination an authored loopback row reaches.
func proxyBridgeLoopbackEcho(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	address, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	return address.Port
}

func TestProxyNetworkBridgeIsInertWithoutAPolicy(t *testing.T) {
	relay, err := prepareProxyNetworkRelay("")
	require.NoError(t, err)
	defer relay.Close()
	assert.False(t, relay.Active())
	assert.Nil(t, relay.waitCh())
	assert.NoError(t, relay.waitListenerReady(os.Getpid()))
	assert.NoError(t, relay.releaseHarness())
	assert.Empty(t, relay.SetupArgs)
	assert.Empty(t, relay.Command)
}

// TestProxyNetworkRelayGateStaysClosedUntilTheProxyServes proves the ordering
// the supervision contract depends on: the release token cannot be sent before
// a proxy exists.
func TestProxyNetworkRelayGateStaysClosedUntilTheProxyServes(t *testing.T) {
	relay := &preparedProxyNetworkRelay{Sync: &net.UnixConn{}}
	assert.ErrorContains(t, relay.releaseHarness(),
		"opened before the proxy served")
}

func TestTclaudeLayerWinchRelayRefusesCombinedEngines(t *testing.T) {
	plan := proxyBridgeTestPlan(t,
		proxyBridgeDiscriminatingRules(), sandboxpolicy.NetworkEngineProxy)
	proxyPolicy, err := encodeProxyNetworkRelayPolicy(plan)
	require.NoError(t, err)
	packetPlan := proxyBridgeTestPlan(t,
		proxyBridgeDiscriminatingRules(), sandboxpolicy.NetworkEnginePacket)
	packetPolicy, err := encodeFilteredNetworkRelayPolicy(packetPlan)
	require.NoError(t, err)

	code, err := runTclaudeLayerWinchRelay(
		[]string{"/bin/true"}, nil, stackedRelayBindingOptions{
			ProxyPolicy:    proxyPolicy,
			FilteredPolicy: packetPolicy,
		})
	assert.Equal(t, 125, code)
	assert.ErrorContains(t, err, "cannot be combined")

	code, err = runTclaudeLayerWinchRelay(
		[]string{"/bin/true"}, nil, stackedRelayBindingOptions{
			ProxyPolicy:  proxyPolicy,
			ManifestPath: "/tmp/manifest.json",
		})
	assert.Equal(t, 125, code)
	assert.ErrorContains(t, err, "cannot be combined")
}

// TestTclaudeLayerUnixRelayRefusesTheProxyEngine proves the OpenCode
// inherited-descriptor path fails closed rather than rendering a proxy plan
// under the packet supervisor's fd contract.
func TestTclaudeLayerUnixRelayRefusesTheProxyEngine(t *testing.T) {
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName:   "claude",
		Cwd:           t.TempDir(),
		StateRoot:     t.TempDir(),
		Snapshot:      proxyBridgeSnapshot(proxyBridgeDiscriminatingRules()),
		NetworkEngine: sandboxpolicy.NetworkEngineProxy,
	})
	require.NoError(t, err)
	_, err = tclaudeLayerUnixRelayServerCommandArgs(spec, []string{"bwrap"})
	assert.ErrorContains(t, err, "does not support the proxy filtering engine")
	_, _, err = TclaudeLayerUnixRelayServerFDs(spec)
	assert.ErrorContains(t, err, "does not support the proxy filtering engine")
}

// TestBuildTclaudeLayerLaunchSpecRefusesAnInvalidEngine keeps the launch input
// inside the authored vocabulary.
func TestBuildTclaudeLayerLaunchSpecRefusesAnInvalidEngine(t *testing.T) {
	_, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName:   "claude",
		Cwd:           t.TempDir(),
		StateRoot:     t.TempDir(),
		Snapshot:      proxyBridgeSnapshot(proxyBridgeDiscriminatingRules()),
		NetworkEngine: sandboxpolicy.NetworkEngine("socks"),
	})
	assert.ErrorContains(t, err, "network.engine")
}

// TestTclaudeLayerLaunchSpecCarriesTheEngineToThePlan proves the engine
// survives the spec round trip production persists.
func TestTclaudeLayerLaunchSpecCarriesTheEngineToThePlan(t *testing.T) {
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName:   "claude",
		Cwd:           t.TempDir(),
		StateRoot:     t.TempDir(),
		Snapshot:      proxyBridgeSnapshot(proxyBridgeDiscriminatingRules()),
		NetworkEngine: sandboxpolicy.NetworkEngineProxy,
	})
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.NetworkEngineProxy, spec.Contract.NetworkEngine)
	_, _, _, _, _, plan, err := tclaudeLayerSpecRenderInput(spec)
	require.NoError(t, err)
	assert.True(t, tclaudeLayerPlanDeploysProxy(plan))
	assert.Equal(t, sandboxpolicy.NetworkIsolatedWithAgentd,
		tclaudeLayerPlanFloorPosture(plan))
}

func proxyBridgeSnapshot(
	rules sandboxpolicy.NetworkRules,
) *sandboxpolicy.Snapshot {
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.Network = &rules
	return &snapshot
}

// TestResolveTclaudeLayerForEngineProbesTheFloorItBuilds proves the
// prerequisite check follows the engine: the proxy engine must not be refused
// for a packet-gateway prerequisite it never uses.
func TestResolveTclaudeLayerForEngineProbesTheFloorItBuilds(t *testing.T) {
	previousLookPath := lookPathBwrap
	previousProbe := probeBwrap
	previousPidfd := probeTclaudeLayerPidfd
	t.Cleanup(func() {
		lookPathBwrap = previousLookPath
		probeBwrap = previousProbe
		probeTclaudeLayerPidfd = previousPidfd
	})
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeTclaudeLayerPidfd = func() error { return nil }
	var probed []sandboxpolicy.NetworkPosture
	probeBwrap = func(
		_ string,
		posture sandboxpolicy.NetworkPosture,
		_ sandboxpolicy.RootPosture,
	) error {
		probed = append(probed, posture)
		return nil
	}
	_, sandbox, err := ResolveTclaudeLayerForEngine(
		sandboxpolicy.NetworkFiltered,
		sandboxpolicy.RootConstructed,
		sandboxpolicy.NetworkEngineProxy,
	)
	require.NoError(t, err)
	assert.Equal(t,
		[]sandboxpolicy.NetworkPosture{sandboxpolicy.NetworkIsolatedWithAgentd},
		probed,
		"the proxy engine probes the floor it actually builds")
	assert.Contains(t, sandbox.Source, "filtering proxy",
		"disclosure names the mechanism that runs")
	assert.True(t, sandbox.FilteredNetwork)
	assert.NotContains(t, sandbox.Source, "pasta")

	probed = nil
	// The packet engine still probes the packet floor. Its pasta/nft
	// prerequisite may be absent on a developer host, which is itself the
	// point of the contrast: the proxy engine above resolved without it.
	_, _, _ = ResolveTclaudeLayerForEngine(
		sandboxpolicy.NetworkFiltered,
		sandboxpolicy.RootConstructed,
		sandboxpolicy.NetworkEnginePacket,
	)
	assert.Equal(t,
		[]sandboxpolicy.NetworkPosture{sandboxpolicy.NetworkFiltered}, probed)
	assert.Contains(t,
		TclaudeLayerLaunchOSSandbox(
			sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed).Source,
		"pasta")
}

func TestProxyNetworkBootstrapContractRefusesAnEmptyCommand(t *testing.T) {
	assert.ErrorContains(t, runTclaudeLayerProxyBootstrap(nil), "contract is invalid")
}

func TestTclaudeLayerFloorPostureMapsOnlyTheProxyEngine(t *testing.T) {
	for _, tc := range []struct {
		posture sandboxpolicy.NetworkPosture
		engine  sandboxpolicy.NetworkEngine
		want    sandboxpolicy.NetworkPosture
	}{
		{
			posture: sandboxpolicy.NetworkFiltered,
			engine:  sandboxpolicy.NetworkEngineProxy,
			want:    sandboxpolicy.NetworkIsolatedWithAgentd,
		},
		{
			posture: sandboxpolicy.NetworkFiltered,
			engine:  sandboxpolicy.NetworkEnginePacket,
			want:    sandboxpolicy.NetworkFiltered,
		},
		{
			posture: sandboxpolicy.NetworkFiltered,
			engine:  sandboxpolicy.NetworkEngineUnset,
			want:    sandboxpolicy.NetworkFiltered,
		},
		{
			posture: sandboxpolicy.NetworkHostOpen,
			engine:  sandboxpolicy.NetworkEngineProxy,
			want:    sandboxpolicy.NetworkHostOpen,
		},
	} {
		assert.Equal(t, tc.want,
			TclaudeLayerFloorPosture(tc.posture, tc.engine))
	}
}

// TestProxyNetworkSetupArgsGrantNoCapabilities keeps the in-namespace footprint
// to the sealed bootstrap and the readiness socket.
func TestProxyNetworkSetupArgsGrantNoCapabilities(t *testing.T) {
	plan := proxyBridgeTestPlan(t,
		proxyBridgeDiscriminatingRules(), sandboxpolicy.NetworkEngineProxy)
	encoded, err := encodeProxyNetworkRelayPolicy(plan)
	require.NoError(t, err)
	relay, err := prepareProxyNetworkRelay(encoded)
	require.NoError(t, err)
	defer relay.Close()
	assert.False(t, slices.Contains(relay.SetupArgs, "--cap-add"))
	assert.Len(t, relay.Files, 1, "the bootstrap image is the only sealed input")
	assert.Contains(t, relay.SetupArgs, proxyNetworkBootstrapSyncPath)
	assert.Contains(t, relay.Command, tclaudeLayerProxyBootstrapCommand)
}
