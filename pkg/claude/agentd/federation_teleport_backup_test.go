package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestTeleportBackupObservationRestartsAfterOriginOutage(t *testing.T) {
	now := time.Now()
	var o teleportLeaseObservations
	require.True(t, o.observe(now, true))
	o.Rows["offer"] = teleportLeaseObservation{LastLive: now.Add(-6 * time.Minute)}
	require.True(t, o.observe(now.Add(time.Second), true))
	require.Len(t, o.Rows, 1)
	require.False(t, o.observe(now.Add(2*time.Second), false))
	require.Nil(t, o.Rows)
	require.True(t, o.observe(now.Add(time.Hour), true))
	require.Empty(t, o.Rows)
	o.Rows["offer"] = teleportLeaseObservation{LastLive: now}
	require.True(t, o.observe(now.Add(2*time.Hour), true))
	require.Empty(t, o.Rows, "suspend gap is not online lease time")
	o.Rows["offer"] = teleportLeaseObservation{LastLive: now}
	require.True(t, o.observe(now, true))
	require.Empty(t, o.Rows, "wall clock rollback fails closed")
}

func TestTeleportBackupObservationSeparatesPeersAndDirections(t *testing.T) {
	now := time.Now()
	rt := fedRuntime{teleportLeases: teleportLeaseObservations{Rows: map[string]teleportLeaseObservation{}}}
	first := db.FederationTeleportLease{Direction: "out", Peer: "one", Offer: "same"}
	rt.teleportLeases.Rows[teleportObservationKey(first)] = teleportLeaseObservation{LastLive: now.Add(-time.Hour)}
	for _, other := range []db.FederationTeleportLease{{Direction: "out", Peer: "two", Offer: "same"}, {Direction: "in", Peer: "one", Offer: "same"}} {
		require.Equal(t, now, rt.teleportObservation(other, now).LastLive, "one peer or direction must not inherit another lease's clock")
	}
}
