package db

import (
	"errors"
	"fmt"
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

func TestLocalHomeMailAtomicCustodyAndReturnReleasesOriginal(t *testing.T) {
	setupTestDB(t)
	id, _, err := EnsureAgentForConv("worker", "test")
	require.NoError(t, err)
	identity := FederationIdentity{Agent: id, Home: "home", Hops: 1, Mail: true, Proofs: map[string]string{"home": "proof"}}
	require.NoError(t, DepartFederationIdentity("worker", "home", "host", "departure", identity))
	makeCustody := func(mid int64) (FederationMailCustody, error) {
		return FederationMailCustody{SenderInstance: "home", EnvelopeID: fmt.Sprintf("local:%d", mid), AgentID: id, IngressInstance: "home", IngressEnvelope: fmt.Sprintf("local:%d", mid), Payload: fmt.Sprintf(`{"local_message":%d}`, mid), ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	mid, _, err := InsertLocalHomeMail(&AgentMessage{ToConv: "worker", Body: "original"}, nil, 10, makeCustody)
	require.NoError(t, err)
	_, claimed, err := ClaimAgentMessageNudge(mid, time.Now())
	require.NoError(t, err)
	require.False(t, claimed)
	_, _, err = InsertLocalHomeMail(&AgentMessage{ToConv: "worker", Body: "rollback"}, nil, 10, func(int64) (FederationMailCustody, error) {
		return FederationMailCustody{}, errors.New("custody failed")
	})
	require.Error(t, err)
	identity.Hops++
	_, err = ReserveFederationIdentity(identity, "home", "host", "return")
	require.NoError(t, err)
	_, _, err = EnsureAgentForConvWithID("returned", id, "return")
	require.NoError(t, err)
	// A reincarnation/return cannot dispatch the pending original before custody settles.
	_, claimed, err = ClaimAgentMessageNudge(mid, time.Now())
	require.NoError(t, err)
	require.False(t, claimed)
	custody, err := LocalHomeMailCustody(mid)
	require.NoError(t, err)
	require.NotNil(t, custody)
	custody.State = "accepted"
	custody.Destination = "home"
	require.NoError(t, UpdateFederationMailCustody(*custody))
	require.NoError(t, SetLocalHomeMailOutcome(mid, true, true))
	_, claimed, err = ClaimAgentMessageNudge(mid, time.Now())
	require.NoError(t, err)
	require.True(t, claimed)
}

func TestLocalHomeCronMailCoalescesAtomically(t *testing.T) {
	setupTestDB(t)
	id, _, err := EnsureAgentForConv("worker", "test")
	require.NoError(t, err)
	build := func(mid int64) (FederationMailCustody, error) {
		return FederationMailCustody{SenderInstance: "home", EnvelopeID: fmt.Sprintf("local:%d", mid), AgentID: id, IngressInstance: "home", IngressEnvelope: fmt.Sprintf("local:%d", mid), Payload: fmt.Sprintf(`{"local_message":%d}`, mid), ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	old, _, err := InsertLocalHomeMail(&AgentMessage{ToConv: "worker", Body: "old tick"}, nil, 0, build, 11)
	require.NoError(t, err)
	latest, _, err := InsertLocalHomeMail(&AgentMessage{ToConv: "worker", Body: "latest tick"}, nil, 0, build, 11)
	require.NoError(t, err)
	oldRow, err := GetAgentMessage(old)
	require.NoError(t, err)
	require.Nil(t, oldRow)
	job, err := AgentMessageCronJobID(latest)
	require.NoError(t, err)
	require.EqualValues(t, 11, job)
	require.NoError(t, err)
	custody, err := LocalHomeMailCustody(latest)
	require.NoError(t, err)
	require.NotNil(t, custody)
}
