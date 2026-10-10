package agentd_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardHubAdminClaimAndManagement(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	handler := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return testharness.Serve(handler, testharness.JSONRequest(t, method, "/api/federation/hub/"+tail, body))
	}
	must := func(method, tail string, body any) map[string]any {
		t.Helper()
		rec := call(method, tail, body)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var result map[string]any
		testharness.DecodeJSON(t, rec, &result)
		return result
	}
	// Non-admin sees bootstrap/status only. Ordinary node trust is irrelevant.
	before := must("GET", "status", nil)
	require.Equal(t, false, before["admin"])
	require.Equal(t, fh.url, before["hub_url"])
	denied := call("GET", "admins", nil)
	require.Equal(t, 403, denied.Code, denied.Body.String())
	token, err := fh.store.PrepareAdminClaim(false, time.Now())
	require.NoError(t, err)
	// A newly generated token changes the generation: learn it from status first.
	conflict := call("GET", "status", nil)
	require.Equal(t, 409, conflict.Code, conflict.Body.String())
	must("GET", "status", nil)
	bad := call("POST", "claim", map[string]any{"token": "tchac_" + string(make([]byte, 64))})
	require.Equal(t, 403, bad.Code)
	must("POST", "claim", map[string]any{"token": token})
	reused := call("POST", "claim", map[string]any{"token": token})
	require.Equal(t, 409, reused.Code, reused.Body.String())
	status := must("GET", "status", nil)
	require.Equal(t, true, status["admin"])
	local := status["instance"].(string)
	require.Equal(t, "private, no-store", call("GET", "status", nil).Header().Get("Cache-Control"))
	// List/add/remove, including a live remote admin and last-manager guard.
	require.Len(t, must("GET", "admins", nil)["admins"], 1)
	last := call("DELETE", "admins/"+local, nil)
	require.Equal(t, 409, last.Code, last.Body.String())
	must("POST", "admins", map[string]any{"instance": fh.peer.id.ID(), "capabilities": []string{"hub.health.read"}})
	peerResult, err := fh.peer.cl.AdminCall(context.Background(), "health", nil)
	require.NoError(t, err)
	if peerResult.Code == "admin_generation" {
		peerResult, err = fh.peer.cl.AdminCall(context.Background(), "health", nil)
		require.NoError(t, err)
	}
	require.Equal(t, 200, peerResult.Status)
	peerResult, err = fh.peer.cl.AdminCall(context.Background(), "settings.patch", map[string]any{"overrides": map[string]int{"max_connections": 1}})
	require.NoError(t, err)
	require.Equal(t, 403, peerResult.Status)
	must("DELETE", "admins/"+fh.peer.id.ID(), nil)
	peerResult, err = fh.peer.cl.AdminCall(context.Background(), "health", nil)
	require.NoError(t, err)
	require.Equal(t, 403, peerResult.Status)
	// Admission and spaces use immutable IDs, with bounded list envelopes.
	third, _ := proto.NewIdentity()
	must("POST", "admissions", map[string]any{"instance": third.ID(), "spaces": []string{"team"}})
	list := must("GET", "admissions?max_entries=1", nil)
	require.Len(t, list["admissions"], 1)
	require.NotEmpty(t, list["next_cursor"])
	must("PUT", "spaces", map[string]any{"instance": third.ID(), "spaces": []string{"other"}})
	require.NotEmpty(t, must("GET", "spaces", nil)["spaces"])
	must("DELETE", "admissions/"+third.ID(), nil)
	// Invite bearers appear only at creation; list/revoke addresses their hash.
	invite := must("POST", "invites", map[string]any{"space": "team", "ttl_seconds": 120})
	require.NotEmpty(t, invite["token"])
	rec := call("GET", "invites", nil)
	require.Equal(t, 200, rec.Code)
	require.NotContains(t, rec.Body.String(), invite["token"])
	must("DELETE", "invites/"+invite["token_hash"].(string), nil)
	// Persisted settings override boot settings and null restores boot values.
	settings := must("GET", "settings", nil)["settings"].(map[string]any)
	boot := settings["max_connections"].(map[string]any)["boot"]
	updated := must("PATCH", "settings", map[string]any{"overrides": map[string]any{"max_connections": 8}})["settings"].(map[string]any)
	require.Equal(t, float64(8), updated["max_connections"].(map[string]any)["effective"])
	restored := must("PATCH", "settings", map[string]any{"overrides": map[string]any{"max_connections": nil}})["settings"].(map[string]any)
	require.Equal(t, boot, restored["max_connections"].(map[string]any)["effective"])
	require.Equal(t, 400, call("PATCH", "settings", map[string]any{"overrides": map[string]any{"accept_remote_scripts": 1}}).Code)
	// Key-loss recovery is preview/apply with exact replacement confirmation.
	replacement, _ := proto.NewIdentity()
	must("POST", "admissions", map[string]any{"instance": replacement.ID(), "spaces": []string{"replacement"}})
	preview := must("POST", "identity/recover", map[string]any{"old": third.ID(), "new": replacement.ID()})
	require.Equal(t, false, preview["applied"])
	require.Equal(t, third.ID(), preview["old"].(map[string]any)["instance"])
	require.Equal(t, 400, call("POST", "identity/recover", map[string]any{"old": third.ID(), "new": replacement.ID(), "apply": true, "fingerprint": "wrong"}).Code)
	// Recover a live admission; revoked predecessor is refused by the store.
	must("POST", "admissions", map[string]any{"instance": third.ID(), "spaces": []string{"team"}})
	applied := must("POST", "identity/recover", map[string]any{"old": third.ID(), "new": replacement.ID(), "apply": true, "fingerprint": proto.InstanceFingerprint(replacement.ID())})
	require.Equal(t, true, applied["applied"])
	must("POST", "identity/revoke-old", map[string]any{"instance": replacement.ID()})
	require.Equal(t, 400, call("POST", "identity/revoke-old", map[string]any{"instance": replacement.ID(), "apply": true}).Code)
	must("POST", "identity/revoke-old", map[string]any{"instance": replacement.ID(), "apply": true, "fingerprint": proto.InstanceFingerprint(replacement.ID())})
	require.NotNil(t, must("GET", "health", nil)["load"])
	logs := must("GET", "logs?max_entries=10", nil)
	require.NotEmpty(t, logs["entries"])
	raw, _ := json.Marshal(logs)
	require.NotContains(t, string(raw), token)
	require.NotContains(t, string(raw), invite["token"])
	// /v1 CLI surface calls the same signed handler, with normal human authority.
	rec = fedHuman(t, fh.f, "GET", "/v1/federation/hub/status", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
}
func TestDashboardHubAdminRoutesNeverReachPeersOrAgents(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	handler := agentd.BuildDashboardHandlerForTest()
	routes := []struct{ method, tail string }{
		{"GET", "status"}, {"POST", "claim"}, {"GET", "admins"}, {"POST", "admins"}, {"DELETE", "admins/" + fh.peer.id.ID()},
		{"GET", "admissions"}, {"POST", "admissions"}, {"DELETE", "admissions/" + fh.peer.id.ID()},
		{"GET", "invites"}, {"POST", "invites"}, {"DELETE", "invites/hash"}, {"GET", "spaces"}, {"PUT", "spaces"},
		{"GET", "settings"}, {"PATCH", "settings"}, {"POST", "identity/recover"}, {"POST", "identity/revoke-old"}, {"GET", "health"}, {"GET", "logs"},
	}
	for _, route := range routes {
		path := "/api/federation/hub/" + route.tail
		rec := testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()), testharness.JSONRequest(t, route.method, path, map[string]any{}))
		require.Equal(t, 403, rec.Code, path)
		rec = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, route.method, "/v1/federation/hub/"+route.tail, map[string]any{}), "hub-agent"))
		require.Equal(t, 403, rec.Code, path)
	}
	// Real authenticated browser denied when an origin attempts a mutation.
	req := testharness.JSONRequest(t, "POST", "/api/federation/hub/claim", map[string]any{"token": "secret"})
	req.Header.Set("Origin", "https://evil.invalid")
	rec := testharness.Serve(handler, req)
	require.Equal(t, 403, rec.Code)
}
