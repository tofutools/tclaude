package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestPeerViewScopedReadsWritesAndRevocation(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	f.HaveGroup("visible")
	f.HaveGroup("hidden-group")
	f.HaveConvWithTitle("visible-conv", "visible-agent")
	f.HaveMember("visible", "visible-conv")
	f.HaveMember("hidden-group", "visible-conv")
	f.HaveConvWithTitle("hidden-conv", "hidden-agent")
	f.HaveMember("hidden-group", "hidden-conv")
	aid, err := db.AgentIDForConv("visible-conv")
	require.NoError(t, err)
	hiddenID, err := db.AgentIDForConv("hidden-conv")
	require.NoError(t, err)
	g, err := db.GetAgentGroupByName("visible")
	require.NoError(t, err)
	grant := func(slug string) {
		t.Helper()
		rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=visible"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	h := agentd.PeerViewHandler(p.id.ID())
	read := func(path string) map[string]any {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", path, nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Contains(t, out, "peer_view")
		return out
	}
	empty := read("/api/snapshot")
	require.Empty(t, empty["groups"])
	grant(agentd.PermGroupsRosterRead)
	snap := read("/api/snapshot")
	raw, _ := json.Marshal(snap)
	require.Contains(t, string(raw), "visible-agent")
	require.NotContains(t, string(raw), "hidden-group")
	require.NotContains(t, string(raw), hiddenID)
	require.NotContains(t, string(raw), "conv_id")
	require.NotContains(t, string(raw), "effective")
	require.NotContains(t, string(raw), `"state"`)
	read("/api/groups/visible")
	read("/api/agents/" + aid)
	for _, path := range []string{"/api/groups/hidden-group", "/api/agents/" + hiddenID, "/api/groups/no-such-group"} {
		rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", path, nil))
		require.Equal(t, 404, rec.Code, rec.Body.String())
		require.NotContains(t, rec.Body.String(), "hidden")
	}
	body := map[string]any{"to": aid, "subject": "hello", "body": "from remote operator"}
	rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/operator-message", body))
	require.Equal(t, 403, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), agentd.PermMessageDirect)
	require.Contains(t, rec.Body.String(), "group:visible")
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/operator-message", map[string]any{"to": hiddenID, "body": "hidden"}))
	require.Equal(t, 404, rec.Code, rec.Body.String())
	grant(agentd.PermMessageDirect)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/operator-message", body))
	require.Equal(t, 202, rec.Code, rec.Body.String())
	messages, err := db.ListAgentMessagesForConv("visible-conv", 100)
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	require.False(t, db.IsOperatorAgentMessage(messages[0].ID))
	inbox := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "GET", fmt.Sprintf("/v1/messages/%d?keep-unread=1", messages[0].ID), nil), "visible-conv"))
	require.Equal(t, 200, inbox.Code, inbox.Body.String())
	var mail map[string]any
	testharness.DecodeJSON(t, inbox, &mail)
	require.Equal(t, "operator@bob (remote)", mail["from_title"])
	require.Equal(t, true, mail["remote"])
	require.Contains(t, messages[0].Body, "bob")
	audit, err := db.ListFederationActivity(p.id.ID(), time.Time{}, 100)
	require.NoError(t, err)
	require.NotEmpty(t, audit)
	found := false
	for _, row := range audit {
		if row.Kind == "federation.peer_view" && row.Status == 202 {
			require.Equal(t, "remote:operator@"+p.id.ID(), row.Actor)
			require.Equal(t, aid, row.Target)
			found = true
		}
	}
	require.True(t, found)
	_, err = db.DeleteFederationPeerGrant(p.id.ID(), agentd.PermMessageDirect, db.FederationGroupScope(g.ID))
	require.NoError(t, err)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/operator-message", body))
	require.Equal(t, 403, rec.Code)
	require.Empty(t, read("/api/costs")["rows"])
	require.Empty(t, read("/api/audit")["rows"])
	for _, path := range []string{"/api/config", "/api/unmapped", "/v1/groups", "/api/spawn"} {
		rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", path, map[string]any{}))
		require.Equal(t, 403, rec.Code, rec.Body.String())
	}
	local := testharness.Serve(agentd.BuildDashboardHandlerForTest(), testharness.JSONRequest(t, "GET", "/api/snapshot", nil))
	require.Equal(t, 200, local.Code, local.Body.String())
	require.NotContains(t, local.Body.String(), `"peer_view"`)
	require.Contains(t, local.Body.String(), "hidden-group")
}

func TestPeerViewInstanceGrantsAndUnrestricted(t *testing.T) {
	fh := newFedHarness(t)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	for _, slug := range []string{agentd.PermCostsRead, agentd.PermFederationAuditRead} {
		rec := fedHuman(t, fh.f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=anything"})
		require.Equal(t, 400, rec.Code, rec.Body.String())
		rec = fedHuman(t, fh.f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/api/costs", "/api/audit"} {
		rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", path, nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var obj map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &obj))
		meta := obj["peer_view"].(map[string]any)
		require.Contains(t, meta["included"], map[string]string{"/api/costs": "costs", "/api/audit": "audit"}[path])
	}
	fh.f.HaveGroup("private-group")
	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/snapshot", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "private-group")
	require.Contains(t, rec.Body.String(), `"peer_view"`)
	for _, path := range []string{"/api/config", "/api/new-endpoint"} {
		rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", path, nil))
		require.Equal(t, 403, rec.Code)
	}
	setFedTrustLevel(t, fh, db.FederationTrustRestricted)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/snapshot", nil))
	require.Equal(t, 200, rec.Code)
	require.NotContains(t, rec.Body.String(), "private-group")
	_, err := db.UntrustFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	req := testharness.JSONRequest(t, "GET", "/api/snapshot", nil)
	req.Header.Set("X-Tclaude-Peer", "bob")
	rec = testharness.Serve(h, req)
	require.Equal(t, 403, rec.Code)
}
