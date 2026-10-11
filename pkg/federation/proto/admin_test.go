package proto

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestHubAdminSignatureBindsRequestAndConnection(t *testing.T) {
	id, err := NewIdentity()
	require.NoError(t, err)
	now := time.Now()
	r := HubAdminRequest{ID: NewEnvelopeID(), HubID: "hub-a", Nonce: "connection-a", Generation: "generation-a", Method: "claim", Payload: json.RawMessage(`{"token":"secret"}`), IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	r.Sign(id)
	require.NoError(t, r.Verify(id.Pub, "hub-a", "connection-a", now))
	require.Error(t, r.Verify(id.Pub, "hub-b", "connection-a", now))
	require.Error(t, r.Verify(id.Pub, "hub-a", "connection-b", now))
	require.Error(t, r.Verify(id.Pub, "hub-a", "connection-a", now.Add(time.Minute)))
	for _, mutate := range []func(*HubAdminRequest){func(p *HubAdminRequest) { p.Generation = "reset" }, func(p *HubAdminRequest) { p.Method = "admins.add" }, func(p *HubAdminRequest) { p.ID = NewEnvelopeID() }, func(p *HubAdminRequest) { p.Payload = json.RawMessage(`{"token":"other"}`) }, func(p *HubAdminRequest) { p.ExpiresAt = now.Add(2 * time.Minute) }} {
		altered := r
		mutate(&altered)
		require.Error(t, altered.Verify(id.Pub, "hub-a", "connection-a", now))
	}
}
