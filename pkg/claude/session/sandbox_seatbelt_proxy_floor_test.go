package session

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// The proxy floor's shape is the one M3.2's TestSeatbeltProxyFloorSmoke will
// assert against a running sandbox-exec: the proxy port reachable over both
// carriages on one listener, a second host-loopback port not reachable, an
// external TCP connect denied, a UDP send denied, no listener creatable, and
// AF_UNIX to the agentd floor still working. These tests are the generator-side
// half of that: they prove the emitted SBPL is the profile that would satisfy
// it, destination by destination, rather than that some profile was emitted.

const (
	proxyFloorAgentdSocket = "/Users/dev/.tclaude/api/agentd.sock"
	proxyFloorProxyPort    = 49871
	// proxyFloorControlPort stands in for §8.2's second host-loopback port: a
	// control server on the SAME interface as the proxy. It is what makes every
	// assertion below discriminating — a profile that opened "loopback" rather
	// than one port would pass a proxy-port check and still be wrong.
	proxyFloorControlPort = 49872
)

// proxyFloorNetworkRules is a rule set the native Seatbelt loopback rules
// cannot express, which is what makes it resolve to the proxy engine at all.
func proxyFloorNetworkRules() sandboxpolicy.NetworkRules {
	return sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeList,
		Engine: sandboxpolicy.NetworkEngineProxy,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Host: "api.anthropic.com", Ports: []int{443}},
		},
	}
}

func proxyFloorPlan(t *testing.T) sandboxpolicy.MountPlan {
	t.Helper()
	rules := proxyFloorNetworkRules()
	compiled, err := sandboxpolicy.CompileFilteredNetworkRules(rules)
	require.NoError(t, err)
	engine, err := sandboxpolicy.DeployedNetworkEngineForRules(rules)
	require.NoError(t, err)
	require.Equal(t, sandboxpolicy.NetworkEngineProxy, engine,
		"the fixture must be a policy that actually deploys a proxy")
	return sandboxpolicy.MountPlan{
		NetworkPosture:  sandboxpolicy.NetworkFiltered,
		NetworkEngine:   engine,
		FilteredNetwork: &compiled,
		Entries: []sandboxpolicy.MountEntry{
			{Path: proxyFloorAgentdSocket, Mode: sandboxpolicy.MountRO},
		},
	}
}

func renderProxyFloor(
	t *testing.T,
	plan sandboxpolicy.MountPlan,
	endpoint netip.AddrPort,
	socketPaths []string,
) (string, []seatbeltProfileParam) {
	t.Helper()
	profile, params, err := renderSeatbeltProfile(
		nil,
		socketPaths,
		plan,
		endpoint,
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.NoError(t, err)
	return profile, params
}

func proxyFloorEndpoint() netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), proxyFloorProxyPort)
}

func TestRenderSeatbeltProxyFloorGolden(t *testing.T) {
	got, params := renderProxyFloor(
		t,
		proxyFloorPlan(t),
		proxyFloorEndpoint(),
		[]string{proxyFloorAgentdSocket},
	)
	assertSeatbeltAllowDenyOrder(t, got)

	const want = `(version 1)
(allow default)

; Filesystem policy is deny-only. Positive descendants are carved out
; inside each deny predicate so plan precedence does not depend on
; Seatbelt allow/deny rule selection.

; Proxy-floor networking denies public connectivity and listeners.
; Allowlisted connects at the parameterized socket spellings are excepted,
; and so is TCP to the proxy port through Seatbelt's host-wide localhost token.
; A second loopback port, an external address, a UDP
; datagram and every listener stay denied.
(deny network-bind)
(deny network-outbound
  (require-all
    (require-not
      (remote unix-socket
        (literal (param "AGENTD_SOCKET_0"))))
    (require-not (remote tcp "localhost:49871"))
  ))

(deny file-write*
  (require-all
    (require-any (literal (param "WRITE_DENY_0")) (subpath (param "WRITE_DENY_0")))
    (require-not (literal "/dev/null"))
    (require-not (literal "/dev/tty"))
    (require-not (literal "/dev/ptmx"))
    (require-not (literal "/dev/fd"))
    (require-not (subpath "/dev/fd"))
    (require-not (regex #"^/dev/(tty|pty)[A-Za-z0-9]+$"))
    (require-not (literal (param "DARWIN_RUNTIME_TMPDIR")))
    (require-not (subpath (param "DARWIN_RUNTIME_TMPDIR")))
  ))

(deny file-write*
  (require-all
    (require-any (literal (param "WRITE_DENY_1")) (subpath (param "WRITE_DENY_1")))
  ))

(deny file-read*
  (require-all
    (require-any (literal (param "READ_DENY_0")) (subpath (param "READ_DENY_0")))
  ))

(deny network-outbound
  (remote unix-socket
    (require-all
      (require-any (literal (param "READ_DENY_0")) (subpath (param "READ_DENY_0")))
    )))

(deny file-read*
  (require-all
    (require-any (literal (param "READ_DENY_1")) (subpath (param "READ_DENY_1")))
  ))

(deny network-outbound
  (remote unix-socket
    (require-all
      (require-any (literal (param "READ_DENY_1")) (subpath (param "READ_DENY_1")))
    )))
`
	if got != want {
		t.Fatalf("Seatbelt proxy-floor golden mismatch\nparams: %#v\nprofile:\n%s",
			params, got)
	}
	assert.Contains(t, params, seatbeltProfileParam{
		name: "AGENTD_SOCKET_0", path: proxyFloorAgentdSocket,
	}, "the agentd floor must remain a parameterized socket spelling, not inlined text")
}

// The floor emits ONE TCP port exception. Seatbelt's localhost token makes
// that selector host-wide rather than one interface-level destination; the
// Darwin smoke owns that limitation. These portable assertions pin the axes
// the grammar can still express: a second port, UDP, and an allow rule remain
// absent.
func TestSeatbeltProxyFloorOpensExactlyTheProxyPort(t *testing.T) {
	profile, _ := renderProxyFloor(
		t,
		proxyFloorPlan(t),
		proxyFloorEndpoint(),
		[]string{proxyFloorAgentdSocket},
	)

	proxyException := fmt.Sprintf(
		`(require-not (remote tcp "localhost:%d"))`, proxyFloorProxyPort)
	assert.Equal(t, 1, strings.Count(profile, proxyException),
		"the proxy port must be excepted exactly once, inside the outbound deny")

	// §8.2's second host-loopback port. Same interface, different port.
	assert.NotContains(t, profile, fmt.Sprintf("localhost:%d", proxyFloorControlPort),
		"a control server on a second host-loopback port must stay unreachable")
	// A port-less localhost exception, or a remote-ip one, would reach every
	// host-local service rather than only services sharing the proxy's port.
	assert.NotContains(t, profile, `"localhost:*"`)
	assert.NotContains(t, profile, `(remote ip `)
	// UDP has no exception at all: the endpoint's is TCP-only, so a datagram to
	// the proxy port itself is denied along with every other send.
	assert.NotContains(t, profile, `(remote udp `)
	// The floor must stay deny-only. An allow rule would make reachability
	// depend on Seatbelt's rule selection instead of on the deny predicate.
	assert.NotContains(t, profile, "(allow network-outbound")
	assert.Equal(t, 1, strings.Count(profile, "(deny network-bind)"),
		"no listener may be creatable, which is what makes the one listener the host's")
	assert.NotContains(t, profile, "(deny network*)")
	assert.NotContains(t, profile, "system-socket",
		"AF_UNIX socket creation is not connectivity and must stay permitted")

	floorRule := seatbeltRuleContaining(profile, proxyException)
	assert.Contains(t, floorRule, `(literal (param "AGENTD_SOCKET_0"))`,
		"the agentd floor and the proxy endpoint must be exceptions to the SAME deny")
}

// Without the endpoint the same plan renders the bare isolated floor, which has
// no route to the proxy the launch is running. That is the value the golden
// above must differ from, so this pins the difference rather than trusting it.
func TestSeatbeltProxyFloorDiffersFromTheIsolatedFloorItExtends(t *testing.T) {
	proxy, _ := renderProxyFloor(
		t,
		proxyFloorPlan(t),
		proxyFloorEndpoint(),
		[]string{proxyFloorAgentdSocket},
	)
	isolated, _ := renderProxyFloor(
		t,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd,
			Entries: []sandboxpolicy.MountEntry{
				{Path: proxyFloorAgentdSocket, Mode: sandboxpolicy.MountRO},
			},
		},
		netip.AddrPort{},
		[]string{proxyFloorAgentdSocket},
	)

	assert.NotContains(t, isolated, "localhost:",
		"the isolated floor reaches no IP destination at all")
	// Everything the isolated floor denies, the proxy floor still denies: the
	// endpoint is an addition to that floor, not a different one.
	for _, denial := range []string{
		"(deny network-bind)",
		`(literal (param "AGENTD_SOCKET_0"))`,
	} {
		assert.Contains(t, isolated, denial)
		assert.Contains(t, proxy, denial)
	}
	// And it is exactly one addition. Comparing the RULE lines — the comment
	// prose above each floor differs by design — states the relationship as a
	// difference rather than as two assertions that could both hold of a floor
	// that had quietly gained or lost something else.
	assert.Equal(t,
		[]string{fmt.Sprintf(
			`    (require-not (remote tcp "localhost:%d"))`, proxyFloorProxyPort)},
		seatbeltRuleLinesAdded(isolated, proxy),
		"the proxy floor is the isolated floor plus the endpoint exception, nothing more")
	assert.Empty(t, seatbeltRuleLinesAdded(proxy, isolated),
		"the proxy floor must remove nothing the isolated floor denies")
}

func TestSeatbeltProxyFloorCanCarveOneManagedServerBind(t *testing.T) {
	const controlPort = 43210
	profile, _, err := renderSeatbeltProfileWithLoopbackBind(
		nil,
		[]string{proxyFloorAgentdSocket},
		proxyFloorPlan(t),
		proxyFloorEndpoint(),
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
		controlPort,
	)
	require.NoError(t, err)
	assert.Contains(t, profile,
		`(deny network-bind (require-not (local tcp "localhost:43210")))`)
	assert.NotContains(t, profile, "(allow network-bind")
	assert.NotContains(t, profile, `(local tcp "localhost:*")`)
	assert.NotContains(t, profile, fmt.Sprintf("localhost:%d", proxyFloorControlPort),
		"only the runtime-owned control port may be carved out")
	assert.Equal(t, 1, strings.Count(profile, "(deny network-bind"),
		"the carveout must narrow the existing deny, not add a competing rule")
}

func TestSeatbeltLoopbackBindRequiresRestrictedFloor(t *testing.T) {
	_, _, err := renderSeatbeltProfileWithLoopbackBind(
		nil,
		nil,
		sandboxpolicy.MountPlan{NetworkPosture: sandboxpolicy.NetworkHostOpen},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
		43210,
	)
	require.ErrorContains(t, err,
		"seatbelt loopback bind exception requires an isolated or filtered network floor")
}

func TestSeatbeltIsolatedFloorCarvesAppServerBindAndConnect(t *testing.T) {
	const appServerPort = 43210
	profile, _, err := renderSeatbeltProfileWithLoopbackBind(
		nil,
		[]string{proxyFloorAgentdSocket},
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd,
			Entries: []sandboxpolicy.MountEntry{
				{Path: proxyFloorAgentdSocket, Mode: sandboxpolicy.MountRO},
			},
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
		appServerPort,
	)
	require.NoError(t, err)
	assert.Contains(t, profile,
		`(deny network-bind (require-not (local tcp "localhost:43210")))`)
	assert.Contains(t, profile,
		`(require-not (remote tcp "localhost:43210"))`)
}

// seatbeltRuleLinesAdded reports the rule lines present in got and absent from
// base, as an ordered multiset difference. Comment and blank lines are dropped:
// they carry no policy.
func seatbeltRuleLinesAdded(base, got string) []string {
	remaining := map[string]int{}
	for _, line := range strings.Split(base, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed == "" ||
			strings.HasPrefix(trimmed, ";") {
			continue
		}
		remaining[line]++
	}
	added := []string{}
	for _, line := range strings.Split(got, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed == "" ||
			strings.HasPrefix(trimmed, ";") {
			continue
		}
		if remaining[line] > 0 {
			remaining[line]--
			continue
		}
		added = append(added, line)
	}
	return added
}

// A launch with no allowlisted socket at all still has to reach its proxy. The
// isolated floor's shortcut for that case is a bare `(deny network-outbound)`,
// which would deny the endpoint the launch depends on.
func TestSeatbeltProxyFloorWithoutAllowlistedSocketsStillReachesTheProxy(t *testing.T) {
	plan := proxyFloorPlan(t)
	plan.Entries = nil
	profile, params := renderProxyFloor(t, plan, proxyFloorEndpoint(), nil)

	assert.NotContains(t, profile, "(deny network-outbound)\n",
		"the blanket outbound deny would cut the sandbox off from its own proxy")
	assert.Contains(t, profile, fmt.Sprintf(
		`(require-not (remote tcp "localhost:%d"))`, proxyFloorProxyPort))
	for _, param := range params {
		assert.NotContains(t, param.name, "AGENTD_SOCKET_",
			"no socket was allowlisted, so no socket exception may appear")
	}
}

// A real launch allowlists the whole agentd socket floor — canonical and
// compatibility spellings — not the single socket the golden above
// uses. The endpoint exception has to survive alongside all of them, in the
// same deny, or the normal launch shape is the one that loses its route to the
// proxy.
//
// This is asserted as a property over every socket count from zero up, not at
// one fixture size. AgentdSocketFloor() dedups, so it is normally four but three
// whenever a legacy spelling is empty or folds away; a test pinned at three
// discrete sizes leaves the counts between them free to behave differently.
func TestSeatbeltProxyFloorKeepsTheEndpointBesideEverySocketException(t *testing.T) {
	sockets := []string{
		"/Users/dev/.tclaude/api/agentd.sock",
		"/Users/dev/.tclaude/api/agentd-socket/agentd.sock",
		"/Users/dev/.tclaude-agentd.sock",
		"/Users/dev/.tclaude/agentd.sock",
	}
	require.GreaterOrEqual(t, len(sockets), len(sandboxpolicy.AgentdSocketFloor()),
		"the fixture must cover at least as many spellings as the real socket floor")

	for count := 0; count <= len(sockets); count++ {
		t.Run(fmt.Sprintf("%d allowlisted sockets", count), func(t *testing.T) {
			allowlisted := sockets[:count]
			plan := proxyFloorPlan(t)
			plan.Entries = nil
			for _, socket := range allowlisted {
				plan.Entries = append(plan.Entries, sandboxpolicy.MountEntry{
					Path: socket, Mode: sandboxpolicy.MountRO,
				})
			}
			profile, params := renderProxyFloor(
				t, plan, proxyFloorEndpoint(), allowlisted)

			proxyException := fmt.Sprintf(
				`(require-not (remote tcp "localhost:%d"))`, proxyFloorProxyPort)
			assert.Equal(t, 1, strings.Count(profile, proxyException),
				"the endpoint exception must survive a %d-socket floor", count)

			floorRule := seatbeltRuleContaining(profile, proxyException)
			rendered := make([]string, 0, count)
			for index := range allowlisted {
				name := fmt.Sprintf("AGENTD_SOCKET_%d", index)
				require.Contains(t, floorRule,
					fmt.Sprintf(`(literal (param "%s"))`, name),
					"every allowlisted socket must be an exception to the SAME deny as the endpoint")
				for _, param := range params {
					if param.name == name {
						rendered = append(rendered, param.path)
					}
				}
			}
			assert.ElementsMatch(t, allowlisted, rendered,
				"each socket spelling must be parameterized exactly once")
			assert.NotContains(t, floorRule,
				fmt.Sprintf("AGENTD_SOCKET_%d", count),
				"the floor must not invent a socket exception no caller allowlisted")
		})
	}
}

// Loopback identity is decided by the shared predicate, so every spelling of
// the host is one destination rather than several. The rendered exception is
// therefore identical across spellings, and an address outside that space is
// refused rather than opened.
//
// The unspecified addresses the shared predicate also carries are deliberately
// absent here: they name the host as a DESTINATION but are a wildcard bind as a
// LISTEN address, and the refusal test above owns them.
//
// ::ffff:127.0.0.1 is here because Go's net-to-netip bridge produces mapped
// spellings from any 16-byte net.IP, so it is a form a launcher really arrives
// with — not because it discriminates the predicates. It does not: netip's own
// IsLoopback already unmaps 4-in-6.
//
// Worth recording, since it looks like a gap: once the wildcard refusal is in
// place, AddrIsLoopbackIdentity and a plain IsLoopback agree on every address
// that can be a real listen address. The identity predicate is wider only by
// the unspecified addresses (refused above as binds) and the rest of
// 0.0.0.0/8, which names "this network" and is not an address any host binds.
// So the reuse of the shared predicate is correct here BY CONSTRUCTION rather
// than test-enforced, and no fixture below can prove which one is called. It is
// still the right call to make: the identity question has one owner, and a
// local IsLoopback here would be a second definition that happens to coincide
// today.
func TestSeatbeltProxyFloorUsesTheSharedLoopbackIdentity(t *testing.T) {
	canonical, _ := renderProxyFloor(
		t, proxyFloorPlan(t), proxyFloorEndpoint(), []string{proxyFloorAgentdSocket})

	for _, spelling := range []string{
		"127.0.0.1", "127.0.0.5", "::1", "::ffff:127.0.0.1",
	} {
		addr := netip.MustParseAddr(spelling)
		require.True(t, sandboxpolicy.AddrIsLoopbackIdentity(addr),
			"the fixture must name the host by the shared predicate's definition")
		profile, _ := renderProxyFloor(
			t,
			proxyFloorPlan(t),
			netip.AddrPortFrom(addr, proxyFloorProxyPort),
			[]string{proxyFloorAgentdSocket},
		)
		assert.Equal(t, canonical, profile,
			"spelling %s names the same host, so it must render the same profile",
			spelling)
	}
}

func TestRenderSeatbeltProxyFloorRefusesEndpointsItCannotHonor(t *testing.T) {
	plan := proxyFloorPlan(t)
	external := netip.MustParseAddr("93.184.216.34")
	require.False(t, sandboxpolicy.AddrIsLoopbackIdentity(external))

	for name, testCase := range map[string]struct {
		plan     sandboxpolicy.MountPlan
		endpoint netip.AddrPort
		message  string
	}{
		"proxy plan without an endpoint": {
			plan:     plan,
			endpoint: netip.AddrPort{},
			message:  "requires the host-loopback endpoint",
		},
		"proxy plan with an unbound port": {
			plan:     plan,
			endpoint: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 0),
			message:  "requires a bound proxy port",
		},
		"proxy plan pointed off the host": {
			plan:     plan,
			endpoint: netip.AddrPortFrom(external, proxyFloorProxyPort),
			message:  "refuses non-host-loopback proxy endpoint",
		},
		// The unspecified address is a host-loopback DESTINATION — connecting to
		// it lands on loopback, which is why the shared predicate carries it —
		// but as the address the proxy LISTENS on it is a wildcard bind, and the
		// sandbox's only egress would be reachable from the LAN. A launcher that
		// took its endpoint from a `net.Listen(":0")` listener address hits
		// exactly this, so it is refused rather than rendered.
		"proxy plan whose endpoint is a wildcard IPv4 bind": {
			plan:     plan,
			endpoint: netip.AddrPortFrom(netip.MustParseAddr("0.0.0.0"), proxyFloorProxyPort),
			message:  "refuses wildcard proxy endpoint",
		},
		"proxy plan whose endpoint is a wildcard IPv6 bind": {
			plan:     plan,
			endpoint: netip.AddrPortFrom(netip.MustParseAddr("::"), proxyFloorProxyPort),
			message:  "refuses wildcard proxy endpoint",
		},
		// The mapped spelling of the same wildcard. Go's net-to-netip bridge
		// produces it from any 16-byte net.IP, so a launcher reading a bind
		// address from configuration arrives here in this form rather than the
		// unmapped one; a check that did not unmap would accept the exact
		// address it exists to reject.
		"proxy plan whose endpoint is a v4-mapped wildcard bind": {
			plan: plan,
			endpoint: netip.AddrPortFrom(
				netip.MustParseAddr("::ffff:0.0.0.0"), proxyFloorProxyPort),
			message: "refuses wildcard proxy endpoint",
		},
		"filtered proxy plan carrying no compiled policy": {
			plan: sandboxpolicy.MountPlan{
				NetworkPosture: sandboxpolicy.NetworkFiltered,
				NetworkEngine:  sandboxpolicy.NetworkEngineProxy,
			},
			endpoint: proxyFloorEndpoint(),
			message:  "requires a compiled network policy",
		},
		"endpoint supplied for an isolated plan": {
			plan: sandboxpolicy.MountPlan{
				NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd,
			},
			endpoint: proxyFloorEndpoint(),
			message:  "deploys no filtering proxy",
		},
		"endpoint supplied for a host-open plan that named the proxy engine": {
			plan: sandboxpolicy.MountPlan{
				NetworkPosture: sandboxpolicy.NetworkHostOpen,
				NetworkEngine:  sandboxpolicy.NetworkEngineProxy,
			},
			endpoint: proxyFloorEndpoint(),
			message:  "deploys no filtering proxy",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := renderSeatbeltProfile(
				nil,
				nil,
				testCase.plan,
				testCase.endpoint,
				[]string{"/Users/dev/.tclaude/data"},
				"/private/tmp/tmux-501",
				"/private/var/folders/ab/runtime/T",
				nil,
				nil,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.message)
		})
	}
}

// Which floor a filtered plan gets is decided by the deployed engine, not by
// the posture alone. Both halves are required, which is what keeps a policy
// that widened away from filtered from claiming a proxy it does not run.
func TestSeatbeltFilteredFloorFollowsTheDeployedEngine(t *testing.T) {
	loopback, err := sandboxpolicy.CompileFilteredNetworkRules(
		sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{
				{Loopback: true, Ports: []int{11434}},
			},
		})
	require.NoError(t, err)

	// The two filtered renderings cannot both apply: a loopback-only rule set
	// carries no deny rows, so it is not discriminating and resolves to no
	// engine, while any rule set that does resolve to the proxy engine is not
	// loopback-only. This is the property the renderer's branch relies on.
	require.True(t, sandboxpolicy.FilteredNetworkRulesAreLoopbackOnly(&loopback))
	proxyRules := proxyFloorNetworkRules()
	compiledProxy, err := sandboxpolicy.CompileFilteredNetworkRules(proxyRules)
	require.NoError(t, err)
	require.False(t, sandboxpolicy.FilteredNetworkRulesAreLoopbackOnly(&compiledProxy))

	native, _ := renderProxyFloor(
		t,
		sandboxpolicy.MountPlan{
			NetworkPosture:  sandboxpolicy.NetworkFiltered,
			FilteredNetwork: &loopback,
		},
		netip.AddrPort{},
		nil,
	)
	assert.Contains(t, native,
		`(allow network-outbound (remote tcp "localhost:11434"))`,
		"a filtered plan that deploys no proxy keeps the native loopback rules")
	assert.NotContains(t, native, "(deny network-bind)",
		"the native loopback rendering is outbound-only and must not gain the floor's bind deny")
	assert.NotContains(t, native, "Proxy-floor networking")

	require.False(t, TclaudeLayerDeploysProxy(
		sandboxpolicy.NetworkFiltered, sandboxpolicy.NetworkEnginePacket),
		"the packet engine deploys no proxy on either platform")
	_, _, err = renderSeatbeltProfile(
		nil,
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture:  sandboxpolicy.NetworkFiltered,
			NetworkEngine:   sandboxpolicy.NetworkEnginePacket,
			FilteredNetwork: &compiledProxy,
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.Error(t, err,
		"a non-loopback list under the packet engine has no Darwin applier and must be refused")
	assert.Contains(t, err.Error(), "loopback-only list")
}
