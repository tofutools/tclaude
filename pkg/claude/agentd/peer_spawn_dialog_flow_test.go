package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// peerViewIncludes reads a peer-view response's included features.
func peerViewIncludes(t *testing.T, h http.Handler, path string) []any {
	t.Helper()
	rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", path, nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var out struct {
		PeerView struct {
			Included []any `json:"included"`
		} `json:"peer_view"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.PeerView.Included
}

func TestPeerSpawnCatalogIsAGrantableReadOnlyPeerView(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("one")
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	get := func(path string) int {
		return testharness.Serve(h, testharness.JSONRequest(t, "GET", path, nil)).Code
	}
	catalog := []string{"/api/spawn-profiles", "/api/spawn-profile-default", "/api/saved-prompts", "/api/roles",
		"/api/sandbox-profiles", "/api/sandbox-profile-default", "/api/groups/one/sandbox-profile", "/api/spawn-harness-policy"}
	for _, path := range catalog {
		require.Equal(t, 403, get(path), "%s needs a grant", path)
	}
	rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermNodeSpawnRead, "scope": "group=one"})
	require.Equal(t, 400, rec.Code, "node.spawn.read is node-wide")
	rec = fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermNodeSpawnRead})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	for _, path := range catalog {
		require.Equal(t, 200, get(path), path)
	}
	require.Contains(t, peerViewIncludes(t, h, "/api/snapshot"), "spawn.catalog")
	// Read-only: the catalogs' writes stay local-only.
	for _, path := range []string{"/api/spawn-profiles", "/api/roles", "/api/saved-prompts", "/api/sandbox-profile-default"} {
		rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", path, map[string]any{"name": "x"}))
		require.Equal(t, 403, rec.Code, path)
	}
	// It lends no spawn, and no cwd picker.
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/browse-directories", map[string]any{}))
	require.Equal(t, 403, rec.Code)
}

func TestPeerFullSpawnRunsTheDialogsSpawnUnderItsGrant(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	f.HaveGroup("one")
	f.HaveGroup("two")
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	grant := func(slug, group string) {
		t.Helper()
		rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=" + group})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	cwd := testutil.CanonicalTempDir(t)
	full := map[string]any{"name": "peer-full", "initial_message": "hello", "cwd": cwd, "is_owner": false, "permission_overrides": map[string]any{}}
	spawn := func(group string, body map[string]any) *httptest.ResponseRecorder {
		return testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/groups/"+group+"/spawn", body))
	}
	grant(agentd.PermGroupsRosterRead, "one")
	grant(agentd.PermGroupsMembersSpawn, "one")
	rec := spawn("one", full)
	require.Equal(t, 403, rec.Code, "groups.members.spawn alone keeps the brief-only request: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), agentd.PermGroupsMembersSpawnFull)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/browse-directories", map[string]any{"path": cwd}))
	require.Equal(t, 403, rec.Code, "no cwd picker without a full spawn grant")

	grant(agentd.PermGroupsMembersSpawnFull, "one")
	require.Contains(t, peerViewIncludes(t, h, "/api/snapshot"), "spawn.full")
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/browse-directories", map[string]any{"path": cwd}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = spawn("one", full)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var launched struct {
		ConvID string `json:"conv_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &launched))
	require.NotEmpty(t, launched.ConvID)
	one, err := db.GetAgentGroupByName("one")
	require.NoError(t, err)
	members, err := db.ListAgentGroupMembers(one.ID)
	require.NoError(t, err)
	var joined bool
	for _, m := range members {
		joined = joined || m.ConvID == launched.ConvID
	}
	require.True(t, joined, "the spawn joins the group")

	grant(agentd.PermGroupsRosterRead, "two")
	require.Equal(t, 403, spawn("two", full).Code, "the grant is group-scoped")
}

func TestUnrestrictedPeerHoldsTheFullSpawnDialog(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	f.HaveGroup("one")
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	for _, path := range []string{"/api/spawn-profiles", "/api/roles", "/api/saved-prompts", "/api/sandbox-profiles"} {
		rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", path, nil))
		require.Equal(t, 200, rec.Code, path)
	}
	included := peerViewIncludes(t, h, "/api/snapshot")
	require.Contains(t, included, "spawn.catalog")
	require.Contains(t, included, "spawn.full")
	cwd := testutil.CanonicalTempDir(t)
	rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/groups/one/spawn",
		map[string]any{"name": "peer-full", "cwd": cwd, "is_owner": false, "permission_overrides": map[string]any{}}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	setFedTrustLevel(t, fh, db.FederationTrustRestricted)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/roles", nil))
	require.Equal(t, 403, rec.Code, "lowering trust takes the catalogs back")
}
