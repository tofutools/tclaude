package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFedAuthGateCachesPassAndKeepsRefusal(t *testing.T) {
	calls, reason := 0, ""
	g := newFedAuthGate(func() string { calls++; return reason })
	for range 100 {
		require.Empty(t, g.allowed())
	}
	require.Equal(t, 1, calls, "frames must reuse a fresh passing verdict")

	reason = "revoked"
	require.Empty(t, g.allowed(), "a fresh pass is reused until refreshed")
	require.Equal(t, "revoked", g.refresh())
	reason = ""
	require.Equal(t, "revoked", g.allowed(), "a refusal is sticky")
	require.Equal(t, "revoked", g.refresh())
	require.Equal(t, 2, calls)
}

func TestFedAuthGateRechecksStaleVerdict(t *testing.T) {
	calls := 0
	g := newFedAuthGate(func() string { calls++; return "" })
	require.Empty(t, g.allowed())
	g.verdict.Store(&fedAuthVerdict{at: time.Now().Add(-2 * fedAuthMaxAge)})
	require.Empty(t, g.allowed())
	require.Equal(t, 2, calls, "a stale verdict is re-checked on the data path")
}
