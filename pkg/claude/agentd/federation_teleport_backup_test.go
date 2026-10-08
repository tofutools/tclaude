package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
