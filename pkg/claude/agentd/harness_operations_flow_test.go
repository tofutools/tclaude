package agentd_test

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/harnesscredentials"
	"github.com/tofutools/tclaude/pkg/testharness"
	"testing"
)

func TestHarnessOperationsIndependentAuthorityAndValidation(t *testing.T) {
	fh := newFedHarness(t)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	invalid := func() int {
		return testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/harnesses/operations", map[string]any{"action": "invalid"})).Code
	}
	require.Equal(t, 403, invalid())
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermNodeHarnessesRead}))
	require.Equal(t, 403, invalid())
	for _, slug := range []string{agentd.PermNodeHarnessesInstall, agentd.PermNodeCredentialsReceive} {
		rec := fedHuman(t, fh.f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=anything"})
		require.Equal(t, 400, rec.Code, rec.Body.String())
	}
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermNodeHarnessesInstall}))
	require.Equal(t, 400, invalid())
	b := harnesscredentials.Bundle{Harness: "codex", Files: []harnesscredentials.File{{Name: "auth.json", Data: []byte(`{"token":"test-only"}`)}}}
	body := map[string]any{"action": "install", "harness": "codex", "copy_credentials": true, "credentials": b}
	rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/harnesses/operations", body))
	require.Equal(t, 403, rec.Code, "install grant cannot drop credentials")
	require.NotContains(t, rec.Body.String(), "test-only")
	rec = fedHuman(t, fh.f, "POST", "/v1/harnesses/operations", body)
	require.Equal(t, 400, rec.Code, "local callers cannot submit credential contents")
	rec = fedHuman(t, fh.f, "POST", "/v1/harnesses/operations", map[string]any{"action": "install", "harness": "codex", "copy_credentials": true})
	require.Equal(t, 400, rec.Code, "copy is a remote option")
	rec = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/harnesses/operations", map[string]any{"action": "update", "all": true}), "reader"))
	require.Equal(t, 403, rec.Code)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/node-summary", nil))
	require.Equal(t, 200, rec.Code)
	var snapshot map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snapshot))
	require.Contains(t, string(snapshot["peer_view"]), "node.credentials.receive")
}
