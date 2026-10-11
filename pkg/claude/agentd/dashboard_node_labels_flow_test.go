package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardFederationNodeLabelsAndHubConfig(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	h := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, body any, status int) string {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, "/api/federation/"+tail, body))
		require.Equal(t, status, rec.Code, rec.Body.String())
		return rec.Body.String()
	}
	call("POST", "node-labels", map[string]any{"add": []string{"gpu", "test-rig", "gpu"}}, 200)
	require.JSONEq(t, `{"labels":["gpu","test-rig"]}`, call("GET", "node-labels", nil, 200))
	call("POST", "node-labels", map[string]any{"add": []string{"bad space"}}, 400)
	for _, prefix := range []string{"/api/federation/", "/v1/federation/"} {
		req := testharness.JSONRequest(t, "HEAD", prefix+"node-labels", map[string]any{"remove": []string{"gpu"}})
		handler := h
		if prefix == "/v1/federation/" {
			handler = fh.f.Mux
			req = agentd.AsHumanPeer(req)
		}
		rec := testharness.Serve(handler, req)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.JSONEq(t, `{"labels":["gpu","test-rig"]}`, rec.Body.String())
	}
	call("POST", "node-labels", map[string]any{"remove": []string{"gpu"}}, 200)
	require.JSONEq(t, `{"labels":["test-rig"]}`, call("GET", "node-labels", nil, 200))
	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	for _, method := range []string{"GET", "POST"} {
		rec := testharness.Serve(agentd.PeerViewHandler(peer.InstanceID), testharness.JSONRequest(t, method, "/api/federation/node-labels", nil))
		require.Equal(t, 403, rec.Code, rec.Body.String())
	}
	raw := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(raw)
	rec := testharness.Serve(raw, testharness.JSONRequest(t, "GET", "/api/federation/node-labels", nil))
	require.Equal(t, 403, rec.Code, rec.Body.String())
	// Existing hub-config wrapper supports setup fields, not only enabled toggles.
	call("POST", "config", map[string]any{"enabled": false, "hub_url": "wss://hub.example.test/relay", "name": "new-node", "invite": "invite-value", "hub_ca_file": "/operator/chosen/ca.pem"}, 200)
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, "wss://hub.example.test/relay", cfg.Federation.HubURL)
	require.Equal(t, "new-node", cfg.Federation.Name)
	require.Equal(t, "invite-value", cfg.Federation.Invite)
	require.Equal(t, "/operator/chosen/ca.pem", cfg.Federation.HubCAFile)
	require.Equal(t, []string{"test-rig"}, cfg.Federation.NodeLabels)
	call("POST", "config", map[string]string{"hub_url": "not-a-url"}, 400)
}
