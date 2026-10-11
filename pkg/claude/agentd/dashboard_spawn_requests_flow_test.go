package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardFederationSpawnRequestsAndOutbox(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	f.HaveGroup("team")
	require.Equal(t, 200, fedGrantCaps(t, f, "POST", "/v1/federation/grants", map[string]any{"group": "team", "peer": "bob", "caps": []string{"mail"}}).Code)
	h := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, body any, status int) *httptest.ResponseRecorder {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, "/api/federation/"+tail, body))
		require.Equal(t, status, rec.Code, rec.Body.String())
		require.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
		return rec
	}
	for _, name := range []string{"approve-me", "deny-me", "abandon-me"} {
		env := p.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: name, Brief: "review the change"})
		p.send(env)
		require.Equal(t, proto.AckAccepted, fedAckFor(t, p, env.ID).Status)
	}
	var rows []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Brief  string `json:"brief"`
	}
	require.NoError(t, json.Unmarshal(call("GET", "spawn-requests", nil, 200).Body.Bytes(), &rows))
	require.Len(t, rows, 3)
	ids := map[string]int64{}
	for _, row := range rows {
		ids[row.Name] = row.ID
		require.Equal(t, "pending", row.Status)
		require.Equal(t, "review the change", row.Brief)
	}
	approvePath := fmt.Sprintf("spawn-requests/%d/approve", ids["approve-me"])
	rec := call("POST", approvePath, map[string]any{"name": "approved-worker"}, 200)
	var approved struct {
		Conv string `json:"conv_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &approved))
	f.AssertGroupMember("team", approved.Conv, "approved-worker", 5*time.Second)
	call("POST", fmt.Sprintf("spawn-requests/%d/deny", ids["deny-me"]), map[string]string{"reason": "not today"}, 200)
	denied, err := db.GetFederationSpawnRequest(ids["deny-me"])
	require.NoError(t, err)
	require.Equal(t, db.FedSpawnDenied, denied.Status)
	abandonPath := fmt.Sprintf("spawn-requests/%d/abandon", ids["abandon-me"])
	won, err := db.BeginFederationSpawnRequest(ids["abandon-me"], db.NewAgentID(), false)
	require.NoError(t, err)
	require.True(t, won)
	call("POST", abandonPath, map[string]bool{}, 400)
	call("POST", abandonPath, map[string]bool{"acknowledge_late_worker": true}, 200)
	pending, err := db.GetFederationSpawnRequest(ids["abandon-me"])
	require.NoError(t, err)
	require.Equal(t, db.FedSpawnPending, pending.Status)
	// Outgoing requests use the same catalog admission and durable outbox as CLI.
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{Name: "builders", Caps: []string{proto.CapMail, proto.CapSpawn}}}}))
	fedEventually(t, "remote catalog", func() bool { raw, _, _ := db.GetFederationCatalog(p.id.ID()); return raw != "" })
	rec = call("POST", "spawn-requests", map[string]any{"peer": "bob", "group": "builders", "brief": "build the thing"}, 200)
	var sent struct {
		ID string `json:"envelope_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sent))
	require.NotEmpty(t, sent.ID)
	var outbox []struct {
		ID   string `json:"envelope_id"`
		From string `json:"from"`
	}
	require.NoError(t, json.Unmarshal(call("GET", "outbox?limit=100", nil, 200).Body.Bytes(), &outbox))
	found := false
	for _, row := range outbox {
		if row.ID == sent.ID {
			found = true
			require.Equal(t, "human operator", row.From)
		}
	}
	require.True(t, found)
	peer, err := db.GetFederationPeer(p.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	for _, route := range []struct{ method, tail string }{{"GET", "spawn-requests"}, {"POST", "spawn-requests"}, {"POST", approvePath}, {"POST", fmt.Sprintf("spawn-requests/%d/deny", ids["abandon-me"])}, {"POST", abandonPath}, {"GET", "outbox"}} {
		rec = testharness.Serve(agentd.PeerViewHandler(peer.InstanceID), testharness.JSONRequest(t, route.method, "/api/federation/"+route.tail, nil))
		require.Equal(t, 403, rec.Code, rec.Body.String())
	}
	raw := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(raw)
	rec = testharness.Serve(raw, testharness.JSONRequest(t, "GET", "/api/federation/outbox", nil))
	require.Equal(t, 403, rec.Code, rec.Body.String())
}
