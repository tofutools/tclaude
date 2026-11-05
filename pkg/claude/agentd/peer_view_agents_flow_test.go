package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// peerViewAudit returns the receiver's peer view audit rows.
func peerViewAudit(t *testing.T) []db.AuditLogEntry {
	t.Helper()
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "federation.peer_view"})
	require.NoError(t, err)
	return rows
}

// Pins: a peer view opened by the remote operator (no agent principal)
// keeps its behaviour. A restricted peer acts under its grants and is
// audited as operator@<inst>; an unrestricted peer runs the as-local paths.
func TestPeerViewOperatorPrincipalPins(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("crew")
	f.HaveAliveSession(moveSourceConv, "pin-source", "pin-pane", testutil.CanonicalTempDir(t))
	f.HaveMember("crew", moveSourceConv)
	aid, err := db.AgentIDForConv(moveSourceConv)
	require.NoError(t, err)
	inst := fh.peer.id.ID()
	h := agentd.PeerViewHandler(inst)
	serve := func(method, path string, body any) int {
		return testharness.Serve(h, testharness.JSONRequest(t, method, path, body)).Code
	}
	for _, slug := range []string{agentd.PermGroupsRosterRead, agentd.PermGroupsMembersStop} {
		rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=crew"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	require.Equal(t, 200, serve("POST", "/api/agents/"+aid+"/stop", nil))
	rows := peerViewAudit(t)
	require.NotEmpty(t, rows)
	require.Equal(t, "remote:operator@"+inst, rows[0].ActorLabel)
	require.Contains(t, rows[0].Detail, "POST /api/agents/{id}/stop")
	require.Equal(t, 403, serve("GET", "/api/resilience/policies", nil), "a restricted peer never reaches the local dashboard")
	require.Equal(t, 403, serve("POST", "/api/agents/"+aid+"/tags", map[string]any{"tags": []string{"remote"}}), "no manage grant")

	peer, err := db.GetFederationPeer(inst)
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	require.Equal(t, 200, serve("GET", "/api/resilience/policies", nil), "an unrestricted operator runs the local dashboard route")
	require.Equal(t, http.StatusOK, serve("POST", "/api/agents/"+aid+"/tags", map[string]any{"tags": []string{"remote"}}), "an unrestricted operator needs no grant")
}
