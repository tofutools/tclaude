package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A fused view's Plugins section reads and manages a peer's plugins through
// its peer view, which serves them only under unrestricted trust.
func TestPeerViewPluginsNeedUnrestrictedTrust(t *testing.T) {
	fh := newFedHarness(t)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/plugins", nil))
	require.Equal(t, 403, rec.Code, "a restricted peer reads no plugins: %s", rec.Body.String())
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/plugins", map[string]any{"name": "probe", "steps": []any{map[string]any{"name": "s", "check": "true"}}}))
	require.Equal(t, 403, rec.Code, "a restricted peer installs no plugin: %s", rec.Body.String())

	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/plugins", map[string]any{"name": "probe", "steps": []any{map[string]any{"name": "s", "check": "true"}}}))
	require.Less(t, rec.Code, 300, rec.Body.String())
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/plugins", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var body struct {
		Plugins []struct {
			Name string `json:"name"`
		} `json:"plugins"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Plugins, 1)
	require.Equal(t, "probe", body.Plugins[0].Name)
}

// A fused view's Processes section reaches a peer's process API through
// /api/peer/{node}/v1/process/..., which the proxy forwards to /v1/process/...
// for the peer's own view rules to decide.
func TestFederationPeerProxyReachesTheProcessAPI(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	p := fh.peer
	h := agentd.BuildDashboardHandlerForTest()
	ch := make(chan *httptest.ResponseRecorder, 1)
	req := testharness.JSONRequest(t, "GET", "/api/peer/"+p.id.ID()+"/v1/process/templates", nil)
	go func() { ch <- testharness.Serve(h, req) }()

	conn := acceptOutboundPeerView(t, p, 0)
	wire := readPeerViewWire(t, conn)
	require.Equal(t, "GET", wire.Method)
	require.Equal(t, "/v1/process/templates", wire.URI)
	writePeerViewWire(t, conn, peerViewWire{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"templates":[]}`)})
	_ = conn.Close()
	select {
	case rec := <-ch:
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.JSONEq(t, `{"templates":[]}`, rec.Body.String())
	case <-time.After(10 * time.Second):
		t.Fatal("proxy request did not finish")
	}

	// The peer's side: its template library only under unrestricted trust.
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":true}}`), 0o644))
	view := agentd.PeerViewHandler(fh.peer.id.ID())
	rec := testharness.Serve(view, testharness.JSONRequest(t, "GET", "/v1/process/templates", nil))
	require.Equal(t, 403, rec.Code, rec.Body.String())
	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	rec = testharness.Serve(view, testharness.JSONRequest(t, "GET", "/v1/process/templates", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
}
