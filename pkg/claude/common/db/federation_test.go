package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpdateFederationOutboxPreservesSettledOutcome(t *testing.T) {
	for _, pending := range []string{FedOutboxQueued, FedOutboxSent} {
		for _, settled := range []string{FedOutboxAccepted, FedOutboxRefused, FedOutboxExpired} {
			t.Run(pending+"/"+settled, func(t *testing.T) {
				setupTestDB(t)
				const id = "group-mail-envelope"
				require.NoError(t, InsertFederationOutbox(FederationOutboxRow{
					EnvelopeID: id, Kind: "group_mail", ToInstance: "peer", Sealed: []byte("sealed"), ExpiresAt: time.Now().Add(time.Hour),
				}))
				require.NoError(t, UpdateFederationOutbox(id, pending, time.Now().Add(time.Minute), "", 1))
				row, err := GetFederationOutbox(id)
				require.NoError(t, err)
				require.Equal(t, pending, row.State)
				require.Equal(t, 1, row.Attempts)

				changed, err := SettleFederationOutbox(id, settled, "delivered to 3 members")
				require.NoError(t, err)
				require.True(t, changed)
				before, err := GetFederationOutbox(id)
				require.NoError(t, err)

				// The peer's ack can settle the row before the sender records
				// the hub's delivery result. Neither that late result nor a
				// subsequent send error/retryable ack may reopen the row.
				for _, late := range []struct {
					state string
					note  string
					inc   int
				}{
					{FedOutboxSent, "", 1},
					{FedOutboxQueued, "send failed", 1},
					{FedOutboxQueued, "queue_full: retry later", 0},
				} {
					require.NoError(t, UpdateFederationOutbox(id, late.state, time.Now().Add(2*time.Minute), late.note, late.inc))
					after, err := GetFederationOutbox(id)
					require.NoError(t, err)
					require.Equal(t, before, after)
				}
				changed, err = SettleFederationOutbox(id, FedOutboxExpired, "late expiry")
				require.NoError(t, err)
				require.False(t, changed)
				after, err := GetFederationOutbox(id)
				require.NoError(t, err)
				require.Equal(t, before, after)
				due, err := DueFederationOutbox(time.Now().Add(3*time.Minute), 50)
				require.NoError(t, err)
				require.Empty(t, due)
			})
		}
	}
}
