package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardFederationModelsSharedHandlers(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	fedModelPolicy(t, fh, "https://provider-secret.invalid")
	h := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, body any, status int) *httptest.ResponseRecorder {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, "/api/federation/models/"+tail, body))
		require.Equal(t, status, rec.Code, rec.Body.String())
		require.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
		return rec
	}
	rec := call("GET", "control", nil, 200)
	require.NotContains(t, rec.Body.String(), "provider-secret")
	require.NotContains(t, rec.Body.String(), "credential")
	require.Contains(t, rec.Body.String(), "test-model")
	lease := db.ModelProxyLease{ID: proto.NewEnvelopeID(), Peer: fh.peer.id.ID(), Request: proto.NewEnvelopeID(), Proxy: "model", IdleSeconds: 3600}
	require.NoError(t, db.IssueModelProxyLease(lease))
	var leases []db.ModelProxyLease
	require.NoError(t, json.Unmarshal(call("GET", "leases", nil, 200).Body.Bytes(), &leases))
	require.Len(t, leases, 1)
	require.Equal(t, lease.ID, leases[0].ID)
	// HEAD is routed by Go's GET registration; write-shaped bodies must not mutate.
	for _, prefix := range []string{"/api/federation/models/", "/v1/models/"} {
		for _, item := range []struct {
			tail string
			body any
		}{{"control", map[string]any{"disabled": true}}, {"leases", map[string]string{"id": lease.ID}}} {
			req := testharness.JSONRequest(t, "HEAD", prefix+item.tail, item.body)
			var got *httptest.ResponseRecorder
			if prefix == "/api/federation/models/" {
				got = testharness.Serve(h, req)
			} else {
				got = testharness.Serve(fh.f.Mux, agentd.AsHumanPeer(req))
			}
			require.Equal(t, 200, got.Code, got.Body.String())
		}
	}
	stored, err := db.GetModelProxyLease(lease.ID)
	require.NoError(t, err)
	require.False(t, stored.Revoked)
	require.Contains(t, call("GET", "control", nil, 200).Body.String(), `"disabled":false`)
	call("POST", "control", map[string]any{"name": "model", "peer": "bob", "disabled": true}, 200)
	stored, err = db.GetModelProxyLease(lease.ID)
	require.NoError(t, err)
	require.True(t, stored.Revoked)
	call("POST", "control", map[string]any{"name": "model", "peer": "bob", "disabled": false}, 200)
	require.JSONEq(t, `{"revoked":true}`, call("POST", "leases", map[string]string{"id": lease.ID}, 200).Body.String())
	call("GET", "usage?day=2026-10-10", nil, 200)
	call("GET", "usage?day=bad", nil, 400)
	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	for _, route := range []struct{ method, tail string }{{"GET", "control"}, {"POST", "control"}, {"GET", "leases"}, {"POST", "leases"}, {"GET", "usage"}} {
		rec = testharness.Serve(agentd.PeerViewHandler(peer.InstanceID), testharness.JSONRequest(t, route.method, "/api/federation/models/"+route.tail, nil))
		require.Equal(t, 403, rec.Code, rec.Body.String())
	}
	raw := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(raw)
	rec = testharness.Serve(raw, testharness.JSONRequest(t, "GET", "/api/federation/models/control", nil))
	require.Equal(t, 403, rec.Code, rec.Body.String())
}
