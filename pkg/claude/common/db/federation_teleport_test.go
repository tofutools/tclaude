package db

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestFederationTeleportConcurrentAttemptsRespectOriginQuota(t *testing.T) {
	setupTestDB(t)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := FederationTeleport{Direction: "in", Peer: "peer", Offer: proto.NewEnvelopeID(), State: "pending", Intent: bundletransfer.TeleportIntent{Chain: "chain", OriginInstance: "origin", OriginAgent: "agent"}}
			if RecordFederationTeleport(r, config.TeleportLimits{Hour: 3, Day: 10}) == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(3), accepted.Load())
	rows, err := ListFederationTeleports()
	require.NoError(t, err)
	require.Len(t, rows, 3)
	// A new immediate peer does not reset the stable origin's limit.
	require.ErrorIs(t, RecordFederationTeleport(FederationTeleport{Direction: "in", Peer: "other", Offer: "other", State: "pending", Intent: rows[0].Intent}, config.TeleportLimits{Hour: 3, Day: 10}), ErrTeleportLimit)
}
func TestFederationTeleportReservationCASPreservesTarget(t *testing.T) {
	setupTestDB(t)
	row := FederationTeleport{Direction: "in", Peer: "peer", Offer: "offer", State: "auto_pending", Intent: bundletransfer.TeleportIntent{OriginInstance: "origin", OriginAgent: "agent"}}
	require.NoError(t, RecordFederationTeleport(row, config.TeleportLimits{}))
	stale := row
	row.State, row.TargetAgent = "admitting", "landed-agent"
	won, err := TransitionFederationTeleport(row, "auto_pending")
	require.NoError(t, err)
	require.True(t, won)
	stale.State, stale.TargetAgent = "admitting", "duplicate-agent"
	won, err = TransitionFederationTeleport(stale, "auto_pending")
	require.NoError(t, err)
	require.False(t, won)
	got, err := FederationTeleportForAgent("landed-agent")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "admitting", got.State)
}
