package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardFederationMovesAndTeleport(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	h := agentd.BuildDashboardHandlerForTest()
	move := db.FederationAgentMove{Direction: "out", Peer: fh.peer.id.ID(), ID: "move-one", State: "awaiting_confirmation", SourceAgent: "source", SourceConv: "conv", SHA256: "hash", ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, db.InsertFederationAgentMove(move))
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
		return rec
	}
	var list struct {
		Moves []db.FederationAgentMove `json:"moves"`
	}
	require.NoError(t, json.Unmarshal(call("GET", "/api/federation/moves", nil).Body.Bytes(), &list))
	require.Len(t, list.Moves, 1)
	require.Equal(t, move.ID, list.Moves[0].ID)
	var detail db.FederationAgentMove
	require.NoError(t, json.Unmarshal(call("GET", "/api/federation/moves/"+move.ID, nil).Body.Bytes(), &detail))
	require.Equal(t, "awaiting_confirmation", detail.State)
	require.NoError(t, json.Unmarshal(call("POST", "/api/federation/moves/"+move.ID+"/abandon", nil).Body.Bytes(), &detail))
	require.Equal(t, "abandoned", detail.State)
	stored, err := db.GetFederationAgentMove("out", move.Peer, move.ID)
	require.NoError(t, err)
	require.Equal(t, "abandoned", stored.State)
	call("PUT", "/api/federation/teleport", map[string]bool{"disabled": true})
	// GET and HEAD remain reads even if a client supplies a write-shaped body.
	for _, prefix := range []string{"/api/federation/", "/v1/federation/"} {
		for _, method := range []string{"GET", "HEAD"} {
			req := testharness.JSONRequest(t, method, prefix+"teleport", map[string]bool{"disabled": false})
			var rec *httptest.ResponseRecorder
			if prefix == "/api/federation/" {
				rec = testharness.Serve(h, req)
			} else {
				rec = testharness.Serve(fh.f.Mux, agentd.AsHumanPeer(req))
			}
			require.Equal(t, 200, rec.Code, rec.Body.String())
			require.JSONEq(t, `{"disabled":true}`, rec.Body.String())
		}
	}
	require.JSONEq(t, `{"disabled":false}`, call("PUT", "/api/federation/teleport", map[string]bool{"disabled": false}).Body.String())
	// Even unrestricted remote peers cannot administer the local switch or moves.
	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	peerHandler := agentd.PeerViewHandler(peer.InstanceID)
	for _, route := range []struct{ method, path string }{{"GET", "moves"}, {"GET", "moves/move-one"}, {"POST", "moves/move-one/abandon"}, {"GET", "teleport"}, {"PUT", "teleport"}} {
		rec := testharness.Serve(peerHandler, testharness.JSONRequest(t, route.method, "/api/federation/"+route.path, nil))
		require.Equal(t, 403, rec.Code, rec.Body.String())
	}
	raw := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(raw)
	rec := testharness.Serve(raw, testharness.JSONRequest(t, "GET", "/api/federation/teleport", nil))
	require.Equal(t, 403, rec.Code, rec.Body.String())
}

func TestDashboardFederationMoveAgent(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	aid := fedMoveSource(t, fh)
	h := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/federation/move-agent", map[string]any{"agent": moveSourceConv, "peer": "bob", "group": "receiver"}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
	var v struct {
		Offer struct {
			D struct {
				ID   string          `json:"id"`
				Move json.RawMessage `json:"move"`
			} `json:"offer"`
		} `json:"offer"`
	}
	testharness.DecodeJSON(t, rec, &v)
	require.NotEmpty(t, v.Offer.D.Move, "the dashboard route starts a move, not a plain share")
	m, err := db.GetFederationAgentMove("out", fh.peer.id.ID(), v.Offer.D.ID)
	require.NoError(t, err)
	require.Equal(t, "awaiting_confirmation", m.State)
	require.Equal(t, aid, m.SourceAgent)
	// The source keeps running until the peer confirms its copy.
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())

	// A peer view never reaches move-agent, even for an unrestricted peer.
	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	rec = testharness.Serve(agentd.PeerViewHandler(peer.InstanceID), testharness.JSONRequest(t, "POST", "/api/federation/move-agent", map[string]any{"agent": moveSourceConv, "peer": "bob", "group": "receiver"}))
	require.Equal(t, 403, rec.Code, rec.Body.String())
}
