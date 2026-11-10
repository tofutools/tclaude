package agentd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The moves a restricted peer may make only add confinement.
func TestPeerSandboxImplMoves(t *testing.T) {
	for _, move := range []struct {
		from, to string
		ok       bool
	}{
		{"off", "off", true},
		{"off", "resource-only", true},
		{"off", "harness-builtin", true},
		{"off", "tclaude-layer", true},
		{"resource-only", "harness-builtin", true},
		{"resource-only", "tclaude-layer", true},
		{"", "harness-builtin", true},
		{"resource-only", "off", false},
		{"harness-builtin", "off", false},
		{"harness-builtin", "resource-only", false},
		{"harness-builtin", "tclaude-layer", false},
		{"tclaude-layer", "harness-builtin", false},
		{"tclaude-layer", "off", false},
		{"bogus", "harness-builtin", false},
	} {
		got := peerSandboxImplMoveFailure(move.from, move.to)
		require.Equal(t, move.ok, got == "", "%s -> %s: %q", move.from, move.to, got)
	}
}
