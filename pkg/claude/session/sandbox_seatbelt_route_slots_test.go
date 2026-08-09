package session

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestRenderSeatbeltIsolatedRouteSlotsAreExactBindAndOutboundExceptions(t *testing.T) {
	profile, _, err := renderSeatbeltProfileWithLoopbackBindAndRouteSlots(
		nil,
		nil,
		sandboxpolicy.MountPlan{NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
		0,
		[]int{41101, 41102},
	)
	require.NoError(t, err)
	assert := func(needle string) {
		t.Helper()
		if !strings.Contains(profile, needle) {
			t.Fatalf("profile does not contain %q:\n%s", needle, profile)
		}
	}
	assert(`(require-not (local tcp "localhost:41101"))`)
	assert(`(require-not (local tcp "localhost:41102"))`)
	assert(`(require-not (remote tcp "localhost:41101"))`)
	assert(`(require-not (remote tcp "localhost:41102"))`)
	if strings.Contains(profile, `localhost:41103`) {
		t.Fatalf("unreserved neighboring port appeared in route profile:\n%s", profile)
	}
}

func TestRenderSeatbeltInternalBindsDoNotConsumeDarwinRoutePool(t *testing.T) {
	routeSlots := make([]int, DarwinRouteSlotCountMax)
	for i := range routeSlots {
		routeSlots[i] = 42000 + i
	}
	profile, _, err := renderSeatbeltProfileWithLoopbackBindsAndRouteSlots(
		nil, nil,
		sandboxpolicy.MountPlan{NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd},
		netip.AddrPort{}, []string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501", "/private/var/folders/ab/runtime/T",
		nil, nil, []int{43001, 43002}, routeSlots,
	)
	require.NoError(t, err, "two system listeners must not overflow the valid 16-slot agent route pool")
	for _, port := range []string{"42000", "42015", "43001", "43002"} {
		require.Contains(t, profile, `localhost:`+port)
	}
}

func TestRenderSeatbeltRouteSlotsRejectHostOpenFloor(t *testing.T) {
	_, _, err := renderSeatbeltProfileWithLoopbackBindAndRouteSlots(
		nil, nil,
		sandboxpolicy.MountPlan{NetworkPosture: sandboxpolicy.NetworkHostOpen},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501", "/private/var/folders/ab/runtime/T",
		nil, nil, 0, []int{41101},
	)
	require.Error(t, err)
}

func TestRenderSeatbeltNativeFilteredRouteSlotsAddOnlyExactOutboundAndBindRows(t *testing.T) {
	rules, err := sandboxpolicy.CompileFilteredNetworkRules(sandboxpolicy.NetworkRules{
		Mode:  sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{Loopback: true, Ports: []int{41201}}},
	})
	require.NoError(t, err)
	profile, _, err := renderSeatbeltProfileWithLoopbackBindAndRouteSlots(
		nil, nil,
		sandboxpolicy.MountPlan{NetworkPosture: sandboxpolicy.NetworkFiltered, FilteredNetwork: &rules},
		netip.AddrPort{}, []string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501", "/private/var/folders/ab/runtime/T", nil, nil, 41203,
		[]int{41202},
	)
	require.NoError(t, err)
	if strings.Count(profile, `(allow network-outbound (remote tcp "localhost:41201"))`) != 1 {
		t.Fatalf("authored native loopback slot missing:\n%s", profile)
	}
	if strings.Count(profile, `(allow network-outbound (remote tcp "localhost:41202"))`) != 1 {
		t.Fatalf("route slot outbound exception missing:\n%s", profile)
	}
	if strings.Count(profile, `(allow network-outbound (remote tcp "localhost:41203"))`) != 1 {
		t.Fatalf("managed app-server outbound exception missing:\n%s", profile)
	}
	if strings.Count(profile, `(allow network-outbound (remote udp "localhost:41203"))`) != 0 {
		t.Fatalf("managed TCP app-server port widened to UDP:\n%s", profile)
	}
	if strings.Count(profile, `(require-not (local tcp "localhost:41202"))`) != 1 {
		t.Fatalf("route slot bind exception missing:\n%s", profile)
	}
	if strings.Count(profile, `(require-not (local tcp "localhost:41203"))`) != 1 {
		t.Fatalf("managed app-server bind exception missing:\n%s", profile)
	}
	if strings.Contains(profile, `localhost:41204`) {
		t.Fatalf("unreserved neighboring slot appeared:\n%s", profile)
	}
}

func TestRenderSeatbeltNativeFilteredAppServerPreservesUnrestrictedBind(t *testing.T) {
	rules, err := sandboxpolicy.CompileFilteredNetworkRules(sandboxpolicy.NetworkRules{
		Mode:  sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{Loopback: true, Ports: []int{41201}}},
	})
	require.NoError(t, err)
	profile, _, err := renderSeatbeltProfileWithLoopbackBindAndRouteSlots(
		nil, nil,
		sandboxpolicy.MountPlan{NetworkPosture: sandboxpolicy.NetworkFiltered, FilteredNetwork: &rules},
		netip.AddrPort{}, []string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501", "/private/var/folders/ab/runtime/T", nil, nil, 41203, nil,
	)
	require.NoError(t, err)
	if strings.Count(profile, `(allow network-outbound (remote tcp "localhost:41203"))`) != 1 {
		t.Fatalf("managed app-server outbound exception missing:\n%s", profile)
	}
	if strings.Contains(profile, `(deny network-bind`) {
		t.Fatalf("outbound-only native filtering must preserve unrestricted local binds:\n%s", profile)
	}
}
