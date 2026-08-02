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
