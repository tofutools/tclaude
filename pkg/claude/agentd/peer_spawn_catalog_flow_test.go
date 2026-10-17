package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
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
	catalog := []string{"/api/spawn-profiles", "/api/spawn-profile-default", "/api/spawn-launch-defaults", "/api/saved-prompts", "/api/roles",
		"/api/sandbox-profiles", "/api/sandbox-profile-default", "/api/groups/one/sandbox-profile", "/api/spawn-harness-policy",
		"/api/groups/one/spawn-harness-policy", "/api/spawn-launch-catalog"}
	for _, path := range catalog {
		require.Equal(t, 403, get(path), "%s needs a grant", path)
	}
	rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsRosterRead, "scope": "group=one"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	code, opts := peerSpawnOptions(t, fh.peer.id.ID(), "one")
	require.Equal(t, 200, code)
	require.False(t, opts.Catalog)
	rec = fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermNodeSpawnRead, "scope": "group=one"})
	require.Equal(t, 400, rec.Code, "node.spawn.read is node-wide")
	rec = fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermNodeSpawnRead})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	for _, path := range catalog {
		require.Equal(t, 200, get(path), path)
	}
	require.Contains(t, peerViewIncludes(t, h, "/api/snapshot"), "spawn.catalog")
	code, opts = peerSpawnOptions(t, fh.peer.id.ID(), "one")
	require.Equal(t, 200, code)
	require.True(t, opts.Catalog, "spawn-options tells the dialog it may pre-fill from the catalogs")
	require.Contains(t, opts.Defaults, "cwd")
	// One group's settings only for a group the peer can see.
	f.HaveGroup("hidden")
	require.Equal(t, 404, get("/api/groups/hidden/sandbox-profile"))
	require.Equal(t, 404, get("/api/groups/hidden/spawn-harness-policy"))
	require.Equal(t, 404, get("/api/groups/missing/sandbox-profile"))
	// Read-only: the catalogs' writes stay local-only.
	for _, path := range []string{"/api/spawn-profiles", "/api/roles", "/api/saved-prompts", "/api/sandbox-profile-default"} {
		rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", path, map[string]any{"name": "x"}))
		require.Equal(t, 403, rec.Code, path)
	}
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/browse-directories", map[string]any{}))
	require.Equal(t, 403, rec.Code, "it lends no cwd picker")

	// A group named in a query or body is as gated as a path's.
	require.Equal(t, 200, get("/api/spawn-launch-defaults?group=one"))
	require.Equal(t, 404, get("/api/spawn-launch-defaults?group=hidden"))
	preview := func(group string) int {
		return testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/sandbox-profile-enforcement", map[string]any{
			"draft": map[string]any{"id": 1, "name": "probe", "filesystem": []any{}}, "context": map[string]any{"group": group},
		})).Code
	}
	require.NotEqual(t, 404, preview("one"))
	require.Equal(t, 404, preview("hidden"), "the preview names a group's policy only for a visible group")
	// The effective-sandbox read walks project settings up from dir: a
	// restricted peer names only a directory it could launch in anyway.
	_, err := db.SetAgentGroupDefaultCwd("one", "/srv/one")
	require.NoError(t, err)
	require.Equal(t, 404, get("/api/spawn/effective-sandbox?harness=claude&dir=/etc"), "no filesystem probe")
	require.NotEqual(t, 404, get("/api/spawn/effective-sandbox?harness=claude&dir=/srv/one"))
	require.NotEqual(t, 404, get("/api/spawn/effective-sandbox?harness=claude"))

	// spawn-options names the group's own defaults to a catalog holder, which
	// the peer-view snapshot may withhold.
	require.NoError(t, db.SaveSavedPrompt("", db.SavedPrompt{Name: "house-rules", Body: "Follow the house rules."}))
	_, err = db.SetAgentGroupDefaultPrompts("one", []string{"house-rules"})
	require.NoError(t, err)
	code, opts = peerSpawnOptions(t, fh.peer.id.ID(), "one")
	require.Equal(t, 200, code)
	require.Equal(t, []string{"house-rules"}, opts.DefaultPrompts)
	require.Contains(t, opts.Defaults, "group_profile")
}

func TestUnrestrictedPeerReadsTheCatalogsAndPicksAnyProfile(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("one")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "cheap", Harness: "claude", Model: "haiku", Effort: "low"})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "off", Harness: "claude", Disabled: true})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "vip", Harness: "claude", OperatorOnly: true})
	require.NoError(t, err)
	peer := fh.peer.id.ID()
	h := agentd.PeerViewHandler(peer)
	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	for _, path := range []string{"/api/spawn-profiles", "/api/roles", "/api/saved-prompts", "/api/sandbox-profiles"} {
		rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", path, nil))
		require.Equal(t, 200, rec.Code, path)
	}
	require.Contains(t, peerViewIncludes(t, h, "/api/snapshot"), "spawn.catalog")

	code, opts := peerSpawnOptions(t, peer, "one")
	require.Equal(t, 200, code)
	require.True(t, opts.Catalog)
	require.Equal(t, "automatic", opts.Approval)
	var names []string
	for _, p := range opts.Profiles {
		names = append(names, p.Name)
	}
	require.Contains(t, names, "cheap", "unrestricted trust selects any enabled profile without an allowlist")
	require.NotContains(t, names, "off")
	require.NotContains(t, names, "vip", "an operator-only profile still needs an explicit allowlist")

	code, id, errCode := peerSpawnPost(t, peer, "one", map[string]any{"brief": "task", "profile": "cheap"})
	require.Equal(t, 202, code, errCode)
	waitSpawnSettled(t, id)
	code, _, _ = peerSpawnPost(t, peer, "one", map[string]any{"brief": "task", "profile": "off"})
	require.Equal(t, 403, code, "a disabled profile stays unavailable")
	code, _, _ = peerSpawnPost(t, peer, "one", map[string]any{"brief": "task", "profile": "vip"})
	require.Equal(t, 403, code, "an operator-only profile is not selectable by trust alone")
	_, _, missing := peerSpawnPost(t, peer, "one", map[string]any{"brief": "task", "profile": "nope"})
	_, _, unlisted := peerSpawnPost(t, peer, "one", map[string]any{"brief": "task", "profile": "vip"})
	require.Equal(t, unlisted, missing, "a missing profile answers like an unlisted one: no name probe")

	setFedTrustLevel(t, fh, db.FederationTrustRestricted)
	rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/roles", nil))
	require.Equal(t, 403, rec.Code, "lowering trust takes the catalogs back")
}
