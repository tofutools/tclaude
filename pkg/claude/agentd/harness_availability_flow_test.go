package agentd_test

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"net/http"
	"testing"
)

func TestHarnessAvailabilityAuthorityAndPools(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	request := func() int {
		rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/harnesses/availability", nil))
		return rec.Code
	}
	require.Equal(t, 403, request())
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermNodeRead}))
	require.Equal(t, 403, request(), "node.read does not imply detailed availability")
	for _, scope := range []string{"group=anything"} {
		rec := fedHuman(t, fh.f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermNodeHarnessesRead, "scope": scope})
		require.Equal(t, 400, rec.Code, rec.Body.String())
	}
	pool := fedPool(t, fh, "availability", false)
	rec := fedHuman(t, fh.f, "POST", "/v1/federation/nodes/groups/availability/members", map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, fh.f, "POST", "/v1/federation/grants", map[string]any{"peer": "group:availability", "slug": agentd.PermNodeHarnessesRead})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/harnesses/availability", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"harnesses"`)
	require.Contains(t, rec.Body.String(), `node.harnesses`)
	require.NotEmpty(t, pool.ID)
	rec = fedHuman(t, fh.f, "DELETE", "/v1/federation/nodes/groups/availability/members", map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, 403, request())
	rec = fedHuman(t, fh.f, "POST", "/v1/federation/peers/trust", map[string]any{"instance": "bob", "level": "unrestricted", "confirm_fingerprint": proto.Fingerprint(fh.peer.id.Pub)})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, 200, request(), "unrestricted trust implies detailed availability")
	fedNodeProfile(t, fh, "harness-profile", db.FederationNodeProfileSpec{PeerGrants: []db.FederationPeerGrant{{Slug: agentd.PermNodeHarnessesRead}}})
	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/harnesses/availability", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/harnesses/availability", nil), "reader"))
	require.Equal(t, 403, rec.Code)
}
