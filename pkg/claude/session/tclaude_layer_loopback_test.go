package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// This is the spike answer for TCL-1054, pinned as a test so it cannot drift
// silently: whether a tclaude-layer launch shares host loopback is decided by
// the NETWORK POSTURE axis, not by the sandbox implementation. Copilot being
// admitted in exactly one implementation topology says nothing about it.
//
// If any of these expectations ever flips, a port allocated outside the wrap
// stops being the port the pane binds, and the API drive silently stops
// working — so the table is the contract, not a description.
func TestTclaudeLayerSharesHostLoopback(t *testing.T) {
	tests := []struct {
		name       string
		impl       sandboxpolicy.Implementation
		network    *sandboxpolicy.NetworkRules
		wantShares bool
		wantPuture string
	}{
		{
			// No profile at all: the walking-skeleton posture, and the default
			// every Copilot agent launches under today.
			name:       "tclaude-layer with no authored network keeps the host namespace",
			impl:       sandboxpolicy.ImplementationTclaudeLayer,
			network:    nil,
			wantShares: true,
			wantPuture: "host-open",
		},
		{
			name:       "an explicitly open network keeps the host namespace",
			impl:       sandboxpolicy.ImplementationTclaudeLayer,
			network:    &sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen},
			wantShares: true,
			wantPuture: "host-open",
		},
		{
			// --unshare-net: the pane gets its own loopback, and a port agentd
			// allocated on the host's is unreachable from inside and outside
			// alike. This is the arm that makes the whole approach impossible.
			name:       "a closed network takes the pane out of host loopback",
			impl:       sandboxpolicy.ImplementationTclaudeLayer,
			network:    &sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
			wantShares: false,
			wantPuture: "isolated-with-agentd",
		},
		{
			// Also --unshare-net, plus a filtering engine. Nothing forwards a
			// port inward on Linux; the loopback-bind exception in the renderer
			// is Darwin-only.
			name: "an allow-list network takes the pane out of host loopback",
			impl: sandboxpolicy.ImplementationTclaudeLayer,
			network: &sandboxpolicy.NetworkRules{
				Mode:  sandboxpolicy.AccessModeList,
				Allow: []sandboxpolicy.NetworkAllowEntry{{Host: "example.com"}},
			},
			wantShares: false,
			wantPuture: "filtered",
		},
		{
			// An open mode with denials is filtered too — the posture is not
			// readable from the mode word alone, which is exactly why this asks
			// the same helpers the launch renderer asks.
			name: "open-with-denials is filtered, not host-open",
			impl: sandboxpolicy.ImplementationTclaudeLayer,
			network: &sandboxpolicy.NetworkRules{
				Mode: sandboxpolicy.AccessModeOpen,
				Deny: []sandboxpolicy.NetworkAllowEntry{{CIDR: "192.0.2.0/24"}},
			},
			wantShares: false,
			wantPuture: "filtered",
		},
		{
			// tclaude builds no namespace here, so there is only one loopback
			// however the profile is authored. Stated rather than assumed.
			name:       "no outer layer shares loopback whatever the profile says",
			impl:       sandboxpolicy.ImplementationOff,
			network:    &sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
			wantShares: true,
			wantPuture: "host-open",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			effective := sandboxpolicy.EffectiveProfile{Network: tc.network}
			shares, posture, err := TclaudeLayerSharesHostLoopback(tc.impl, effective)
			require.NoError(t, err)
			assert.Equal(t, tc.wantShares, shares)
			assert.Equal(t, tc.wantPuture, posture,
				"the reported posture must be the floor the decision was made on, "+
					"so a refusal names the namespace the launch would have built")
		})
	}
}

// An unreadable profile must not resolve to "shares loopback". Failing open
// here would admit a launch into a namespace agentd cannot reach and defer the
// failure to a timeout somewhere else.
func TestTclaudeLayerSharesHostLoopback_InvalidProfileFailsClosed(t *testing.T) {
	effective := sandboxpolicy.EffectiveProfile{
		Network: &sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessMode("nonsense")},
	}
	shares, _, err := TclaudeLayerSharesHostLoopback(
		sandboxpolicy.ImplementationTclaudeLayer, effective)
	require.Error(t, err)
	assert.False(t, shares, "an unreadable profile must not be treated as host-open")
}
