package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Without a tclaude-layer launch sandbox to read, the posture comes straight
// from the requested network access, no engine is deployed, and the root
// follows the posture: only an agentd-reachable or filtered network needs the
// constructed root.
func TestResolveTclaudeLayerNetworkWithoutALaunchSandbox(t *testing.T) {
	claude := harness.MustGet(harness.DefaultName)
	cases := []struct {
		access      sandboxpolicy.NetworkAccess
		outerLayer  bool
		wantPosture sandboxpolicy.NetworkPosture
		wantRoot    sandboxpolicy.RootPosture
	}{
		{sandboxpolicy.NetworkAccessInherit, false, sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited},
		{sandboxpolicy.NetworkAccessInternet, true, sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited},
		{sandboxpolicy.NetworkAccessNone, false, sandboxpolicy.NetworkIsolatedWithAgentd, sandboxpolicy.RootConstructed},
		{sandboxpolicy.NetworkAccessNone, true, sandboxpolicy.NetworkIsolatedWithAgentd, sandboxpolicy.RootConstructed},
	}
	for _, tc := range cases {
		got, err := resolveTclaudeLayerNetwork(claude, sandboxpolicy.ImplementationTclaudeLayer, tc.outerLayer, tc.access, nil)
		require.NoError(t, err, "access %q", tc.access)
		assert.Equal(t, tclaudeLayerNetwork{
			posture: tc.wantPosture,
			engine:  sandboxpolicy.NetworkEngineUnset,
			root:    tc.wantRoot,
		}, got, "access %q outer=%v", tc.access, tc.outerLayer)
	}

	_, err := resolveTclaudeLayerNetwork(claude, sandboxpolicy.ImplementationTclaudeLayer, false, "carrier-pigeon", nil)
	require.ErrorContains(t, err, "network_access")
}
