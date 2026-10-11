package hub_test

import (
	"encoding/json"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"testing"
	"time"
)

func adminSocket(t *testing.T, url string, id *proto.Identity) (*websocket.Conn, proto.Frame, proto.Frame) {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial(url+proto.WSPath, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.Close() })
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	var challenge, welcome proto.Frame
	require.NoError(t, ws.ReadJSON(&challenge))
	require.Equal(t, proto.FrameChallenge, challenge.Type)
	require.NoError(t, ws.WriteJSON(&proto.Frame{Type: proto.FrameHello, Proto: proto.ProtocolVersion, InstanceID: id.ID(), PubKey: id.Pub, Sig: proto.SignHello(id, challenge.HubID, challenge.Nonce), Name: "operator"}))
	require.NoError(t, ws.ReadJSON(&welcome))
	require.Equal(t, proto.FrameWelcome, welcome.Type)
	return ws, challenge, welcome
}
func adminRequest(id *proto.Identity, ch, welcome proto.Frame, method string, payload any) *proto.HubAdminRequest {
	raw, _ := json.Marshal(payload)
	now := time.Now().UTC()
	r := &proto.HubAdminRequest{ID: proto.NewEnvelopeID(), HubID: ch.HubID, Nonce: ch.Nonce, Generation: welcome.AdminGeneration, Method: method, Payload: raw, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	r.Sign(id)
	return r
}
func adminWireCall(t *testing.T, ws *websocket.Conn, r *proto.HubAdminRequest) *proto.HubAdminResult {
	t.Helper()
	require.NoError(t, ws.WriteJSON(&proto.Frame{Type: proto.FrameAdminRequest, AdminRequest: r}))
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	for {
		var f proto.Frame
		require.NoError(t, ws.ReadJSON(&f))
		if f.Type == proto.FrameDirectory {
			continue
		}
		require.Equal(t, proto.FrameAdminResult, f.Type)
		require.Equal(t, r.ID, f.AdminResult.ID)
		return f.AdminResult
	}
}
func TestHubAdminWireSignatureReplayAndReset(t *testing.T) {
	_, st, url := newHub(t, hub.Config{})
	id, _ := proto.NewIdentity()
	require.NoError(t, st.Admit(id.ID()))
	token, err := st.PrepareAdminClaim(false, time.Now())
	require.NoError(t, err)
	ws, ch, welcome := adminSocket(t, url, id)
	r := adminRequest(id, ch, welcome, "claim", map[string]any{"token": token})
	require.Equal(t, 200, adminWireCall(t, ws, r).Status)
	r = adminRequest(id, ch, welcome, "invites.create", map[string]any{"space": "team", "ttl_seconds": 120})
	require.Equal(t, 200, adminWireCall(t, ws, r).Status)
	require.Equal(t, "replay", adminWireCall(t, ws, r).Code)
	invites, err := st.ListInvites()
	require.NoError(t, err)
	require.Len(t, invites, 1, "replay must not create a second invite")
	tampered := *adminRequest(id, ch, welcome, "health", map[string]any{})
	tampered.Method = "admins.remove"
	require.Equal(t, "bad_auth", adminWireCall(t, ws, &tampered).Code)
	unknown := adminRequest(id, ch, welcome, "exec", map[string]any{})
	require.Equal(t, "operation", adminWireCall(t, ws, unknown).Code)
	require.NoError(t, ws.Close())
	ws2, ch2, welcome2 := adminSocket(t, url, id)
	require.Equal(t, "bad_auth", adminWireCall(t, ws2, r).Code, "old connection nonce cannot be reused")
	_, err = st.PrepareAdminClaim(true, time.Now())
	require.NoError(t, err)
	stale := adminRequest(id, ch2, welcome2, "health", map[string]any{})
	require.Equal(t, "admin_generation", adminWireCall(t, ws2, stale).Code)
}
