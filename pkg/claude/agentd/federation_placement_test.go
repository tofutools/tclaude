package agentd

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPlacementOrderUsesStableInstanceTies(t *testing.T) {
	a, b := float64(1), float64(1)
	ramA, ramB := uint64(100), uint64(200)
	rows := []fedPlacementCandidate{{Instance: "inst-b", Eligible: true, LoadPerCore: &b, RAMAvailable: &ramB}, {Instance: "inst-a", Eligible: true, LoadPerCore: &a, RAMAvailable: &ramA}, {Instance: "inst-c", Eligible: false}}
	require.Equal(t, []int{1, 0}, placementOrder(rows, "least-loaded"))
	require.Equal(t, []int{0, 1}, placementOrder(rows, "most-free-ram"))
}
