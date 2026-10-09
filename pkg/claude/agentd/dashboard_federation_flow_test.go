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

func TestDashboardFederationAdministrationSharedHandlers(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	f.HaveGroup("team")
	h := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, body any, status int) map[string]any {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, "/api/federation/"+tail, body))
		require.Equal(t, status, rec.Code, rec.Body.String())
		require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}
	// Trust keeps the CLI's fingerprint confirmation gate, even for a local UI.
	trust := map[string]any{"instance": p.id.ID(), "level": "unrestricted"}
	require.Equal(t, "confirmation_required", call("POST", "peers/trust", trust, 400)["code"])
	trust["confirm_fingerprint"] = proto.Fingerprint(p.id.Pub)
	call("POST", "peers/trust", trust, 200)
	peer, err := db.GetFederationPeer(p.id.ID())
	require.NoError(t, err)
	require.Equal(t, db.FederationTrustUnrestricted, peer.TrustLevel)
	// Administration stays local-only for unrestricted peers too.
	peerHandler := agentd.PeerViewHandler(p.id.ID())
	for _, req := range []*http.Request{
		testharness.JSONRequest(t, "GET", "/api/federation/audit", nil),
		testharness.JSONRequest(t, "POST", "/api/federation/peers/untrust", map[string]any{"instance": p.id.ID()}),
		testharness.JSONRequest(t, "PUT", "/api/federation/profiles/demo", map[string]any{}),
		testharness.JSONRequest(t, "POST", "/api/federation/enroll-tokens", map[string]any{}),
	} {
		rec := testharness.Serve(peerHandler, req)
		require.Equal(t, 403, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "local")
	}
	grant := map[string]any{"peer": "bob", "slug": agentd.PermGroupsRosterRead, "scope": "group=team"}
	call("POST", "grants", grant, 200)
	require.Len(t, call("GET", "grants?peer=bob", nil, 200)["grants"], 1)
	call("DELETE", "grants", grant, 200)
	// Pool member routes retain their Go mux path variables.
	call("POST", "nodes/groups", map[string]any{"name": "fleet"}, 200)
	call("POST", "nodes/groups/fleet/members", map[string]any{"peer": "bob"}, 200)
	pool, err := db.GetFederationNodeGroup("fleet")
	require.NoError(t, err)
	members, err := db.ListFederationNodeGroupMembers(pool.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, p.id.ID(), members[0].InstanceID)
	call("DELETE", "nodes/groups/fleet/members", map[string]any{"peer": "bob"}, 200)
	call("DELETE", "nodes/groups/fleet", nil, 200)
	// Profile edits preserve revision checks; applying preserves preview tokens.
	profile := call("POST", "profiles", map[string]any{"name": "demo", "definition": map[string]any{"trust_level": "restricted", "labels": []string{}}}, 200)
	require.Equal(t, float64(1), profile["revision"])
	require.Contains(t, call("GET", "profiles/demo", nil, 200), "profile")
	stale := map[string]any{"name": "demo", "revision": 0, "definition": map[string]any{"trust_level": "restricted", "labels": []string{}}}
	require.Equal(t, "stale_profile", call("PUT", "profiles/demo", stale, 409)["code"])
	updated := call("PUT", "profiles/demo", profile, 200)
	require.Equal(t, float64(2), updated["revision"])
	plan := call("POST", "profiles/demo/apply", map[string]any{"peer": "bob"}, 200)
	require.NotEmpty(t, plan["preview_token"])
	require.Equal(t, false, plan["applied"])
	require.Equal(t, "preview_required", call("POST", "profiles/demo/apply", map[string]any{"peer": "bob", "apply": true}, 400)["code"])
	applied := call("POST", "profiles/demo/apply", map[string]any{"peer": "bob", "apply": true, "preview_token": plan["preview_token"]}, 200)
	require.Equal(t, true, applied["applied"])
	// Invite mint/list/revoke uses the same profile and exact token ID as CLI.
	token := call("POST", "enroll-tokens", map[string]any{"profile": "demo", "uses": 1, "ttl_seconds": 600}, 200)
	require.NotEmpty(t, token["token"])
	claims := token["claims"].(map[string]any)
	require.Len(t, call("GET", "enroll-tokens", nil, 200)["tokens"], 1)
	call("POST", "enroll-tokens/"+claims["token_id"].(string)+"/revoke", nil, 200)
	call("GET", "enrollments", nil, 200)
	call("PUT", "default-peer-profile", map[string]any{"profile": "demo"}, 200)
	require.Equal(t, profile["id"], call("GET", "profiles", nil, 200)["default"].(map[string]any)["id"])
	call("PUT", "default-peer-profile", map[string]any{"profile": ""}, 200)
	// Joining a node is still a previewed CLI operation, with identical validation.
	require.Equal(t, "enrollment", call("POST", "enroll/preview", map[string]any{"token": "bad"}, 409)["code"])
	call("POST", "peers/untrust", map[string]any{"instance": p.id.ID()}, 200)
	peer, err = db.GetFederationPeer(p.id.ID())
	require.NoError(t, err)
	require.Nil(t, peer)
	// Audit is an array surface, rather than an altered dashboard schema.
	rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/federation/audit?limit=5", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var rows []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.NotEmpty(t, rows)
	call("POST", "config", map[string]any{"enabled": false}, 200)
	require.Equal(t, false, call("GET", "status", nil, 200)["enabled"])
	// A raw browser mux does not silently acquire human authority.
	raw := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(raw)
	rec = httptest.NewRecorder()
	raw.ServeHTTP(rec, testharness.JSONRequest(t, "POST", "/api/federation/config", map[string]any{"enabled": false}))
	require.Equal(t, 403, rec.Code, rec.Body.String())
}
