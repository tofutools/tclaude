package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFederationHomeLocationLiveEpochAndMonotonicHops(t *testing.T) {
	setupTestDB(t)
	id, _, e := EnsureAgentForConv("source", "test")
	require.NoError(t, e)
	identity := FederationIdentity{Agent: id, Home: "home", Hops: 1, Mail: true, Proofs: map[string]string{"home": "first-proof"}}
	require.NoError(t, DepartFederationIdentity("source", "home", "b", "first-offer", identity))
	require.NoError(t, UpdateFederationAgentLocation(id, "home", "b", "first-proof", "arrival-b", 1))
	require.NoError(t, UpdateFederationAgentLocation(id, "home", "c", "first-proof", "arrival-c", 2))
	dest, e := FederationMailDestination(id, "home")
	require.NoError(t, e)
	require.Equal(t, "c", dest)
	p, e := ProjectedAgentFederationPresence(id)
	require.NoError(t, e)
	require.Equal(t, "c", p.CurrentInstance)
	require.Equal(t, 2, p.HopCount)
	pinned, e := GetAgentFederationPresence(id)
	require.NoError(t, e)
	require.Equal(t, "b", pinned.CurrentPeer)
	require.Error(t, UpdateFederationAgentLocation(id, "home", "b", "first-proof", "arrival-b", 1))
	require.Error(t, UpdateFederationAgentLocation(id, "home", "fork", "first-proof", "fork", 2))
	identity.Hops = 3
	_, e = ReserveFederationIdentity(identity, "home", "c", "return")
	require.NoError(t, e)
	_, _, e = EnsureAgentForConvWithID("returned", id, "return")
	require.NoError(t, e)
	require.Error(t, UpdateFederationAgentLocation(id, "home", "c", "first-proof", "late", 2))
	identity.Proofs["home"] = "new-proof"
	identity.Hops = 4
	require.NoError(t, DepartFederationIdentity("returned", "home", "d", "next", identity))
	dest, e = FederationMailDestination(id, "home")
	require.NoError(t, e)
	require.Equal(t, "d", dest)
	require.Error(t, UpdateFederationAgentLocation(id, "home", "c", "first-proof", "late", 5))
	require.NoError(t, UpdateFederationAgentLocation(id, "home", "d", "new-proof", "arrival-d", 4))
	_, e = RetireAgentAuthorizationByConv("returned", "human", "done")
	require.NoError(t, e)
	require.Error(t, UpdateFederationAgentLocation(id, "home", "d", "new-proof", "late", 5))
}
func TestFederationHomeMailFenceAndPortableDeliveryLedger(t *testing.T) {
	setupTestDB(t)
	id, _, e := EnsureAgentForConv("source", "test")
	require.NoError(t, e)
	expiry := time.Now().Add(time.Hour)
	require.NoError(t, FenceFederationMail(id, "fence", expiry))
	message := &AgentMessage{ToAgent: id, ToConv: "source", Body: "portable mail"}
	inbound := FederationInbound{FromInstance: "sender", EnvelopeID: "mail-1"}
	_, e = InsertFederationInboundMessage(message, inbound, expiry, 100, nil)
	require.ErrorIs(t, e, ErrFederationMailMoving)
	rows, e := ListFederationMailDeliveries(id)
	require.NoError(t, e)
	require.Empty(t, rows)
	require.NoError(t, ReleaseFederationMailFence(id, "fence"))
	_, e = InsertFederationInboundMessage(message, inbound, expiry, 100, nil)
	require.NoError(t, e)
	rows, e = ListFederationMailDeliveries(id)
	require.NoError(t, e)
	require.Len(t, rows, 1)
	other, _, e := EnsureAgentForConv("visit", "test")
	require.NoError(t, e)
	require.NoError(t, ImportFederationMailDeliveries(other, rows))
	_, e = InsertFederationInboundMessage(&AgentMessage{ToAgent: other, ToConv: "visit", Body: "duplicate"}, inbound, expiry, 100, nil)
	require.ErrorIs(t, e, ErrFederationDuplicate)
	delivered, e := FederationMailDelivered(other, "sender", "mail-1")
	require.NoError(t, e)
	require.True(t, delivered)
}
func TestFederationHomeCustodyQuotaAndDuplicateSettlement(t *testing.T) {
	setupTestDB(t)
	expiry := time.Now().Add(time.Hour)
	m := FederationMailCustody{SenderInstance: "sender", EnvelopeID: "mail", AgentID: "agent", IngressInstance: "sender", IngressEnvelope: "mail", Payload: `{}`, ExpiresAt: expiry}
	fresh, e := QueueFederationMailCustody(m, 1)
	require.NoError(t, e)
	require.True(t, fresh)
	fresh, e = QueueFederationMailCustody(m, 1)
	require.NoError(t, e)
	require.False(t, fresh)
	extra := m
	extra.EnvelopeID = "extra"
	_, e = QueueFederationMailCustody(extra, 1)
	require.Error(t, e)
	rows, e := DueFederationMailCustody(time.Now(), 50)
	require.NoError(t, e)
	require.Len(t, rows, 1)
	rows[0].State = "accepted"
	require.NoError(t, UpdateFederationMailCustody(rows[0]))
	fresh, e = QueueFederationMailCustody(m, 1)
	require.NoError(t, e)
	require.False(t, fresh)
	rows, e = DueFederationMailCustody(time.Now(), 50)
	require.NoError(t, e)
	require.Empty(t, rows)
}
