package db

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFederationTeleportDormantQuotaConcurrentAndRelease(t *testing.T) {
	setupTestDB(t)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l := FederationTeleportLease{Direction: "out", Peer: "peer", Offer: fmt.Sprint(i), SourceAgent: fmt.Sprint(i), Epoch: 1, State: "reserved"}
			if ReserveFederationTeleportLease(l, 3) == nil {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	require.Equal(t, int32(3), accepted.Load())
	rows, err := ListFederationTeleportLeases()
	require.NoError(t, err)
	require.Len(t, rows, 3)
	l := rows[0]
	l.State = "released"
	won, err := TransitionFederationTeleportLease(l, "")
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, ReserveFederationTeleportLease(FederationTeleportLease{Direction: "out", Peer: "peer", Offer: "new", SourceAgent: l.SourceAgent, Epoch: 1, State: "reserved"}, 3))
	require.Error(t, ReserveFederationTeleportLease(FederationTeleportLease{Direction: "out", Peer: "peer", Offer: "duplicate", SourceAgent: l.SourceAgent, Epoch: 1, State: "reserved"}, 100))
}
func TestFederationTeleportLeaseEpochCAS(t *testing.T) {
	setupTestDB(t)
	l := FederationTeleportLease{Direction: "out", Peer: "peer", Offer: "offer", SourceAgent: "source", Epoch: 1, State: "paused"}
	require.NoError(t, ReserveFederationTeleportLease(l, 1))
	stale := l
	l.State = "recovering"
	l.Epoch++
	won, err := TransitionFederationTeleportLease(l, "")
	require.NoError(t, err)
	require.True(t, won)
	stale.State = "paused"
	stale.Sequence++
	won, err = TransitionFederationTeleportLease(stale, "")
	require.NoError(t, err)
	require.False(t, won)
	got, err := GetFederationTeleportLease("out", "peer", "offer")
	require.NoError(t, err)
	require.Equal(t, int64(2), got.Epoch)
	require.Equal(t, "recovering", got.State)
}
