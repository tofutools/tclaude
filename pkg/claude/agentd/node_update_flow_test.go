package agentd_test

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"testing"
)

func TestNodeUpdateAuthorityAndValidation(t *testing.T) {
	fh := newFedHarness(t)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	request := func() int {
		return testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/node/update", map[string]any{"action": "invalid"})).Code
	}
	require.Equal(t, 403, request())
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermNodeRead}))
	require.Equal(t, 403, request(), "node.read cannot update")
	rec := fedHuman(t, fh.f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermNodeUpdate, "scope": "group=anything"})
	require.Equal(t, 400, rec.Code, rec.Body.String())
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermNodeUpdate}))
	require.Equal(t, 400, request(), "authorized invalid input fails before updater initialization")
	rec = fedHuman(t, fh.f, "POST", "/v1/node/update", map[string]any{"action": "apply", "version": "latest"})
	require.Equal(t, 400, rec.Code, rec.Body.String())
	rec = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/node/update", map[string]any{"action": "check"}), "reader"))
	require.Equal(t, 403, rec.Code)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/federation/status", nil))
	require.Equal(t, 403, rec.Code, "grant never exposes local human wrappers")
}
