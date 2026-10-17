package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// A peer's operator changes a visible group's settings through this node's
// own dashboard handlers: groups.settings opens the ordinary settings, every
// other field stays behind its own gate, and unrestricted trust holds it all
// except what no peer is granted.
func TestPeerGroupSettingsThroughDashboardHandlers(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("visible")
	f.HaveGroup("hidden-group")
	dir := testutil.CanonicalTempDir(t)
	grant := func(slug string) {
		t.Helper()
		rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=visible"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	h := agentd.PeerViewHandler(p.id.ID())
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
		return rec.Code, rec.Body.String()
	}
	group := func(name string) *db.AgentGroup {
		t.Helper()
		g, err := db.GetAgentGroupByName(name)
		require.NoError(t, err)
		require.NotNil(t, g)
		return g
	}

	grant(agentd.PermGroupsRosterRead)
	code, body := call("PATCH", "/api/groups/visible", map[string]any{"default_cwd": dir})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsSettings)
	code, body = call("PATCH", "/api/groups/hidden-group", map[string]any{"max_members": 3})
	require.Equal(t, 404, code, body)

	grant(agentd.PermGroupsSettings)
	// Every spawn into the group inherits its directory and profile: they
	// also need the peer's say over a spawn's directory and profile.
	code, body = call("PATCH", "/api/groups/visible", map[string]any{"default_cwd": dir})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsSettingsDefaultDir)
	code, body = call("PATCH", "/api/groups/visible", map[string]any{"default_profile": "anything"})
	require.Equal(t, 403, code, body)
	grant(agentd.PermGroupsMembersSpawnCwd)
	code, body = call("PATCH", "/api/groups/visible", map[string]any{"default_cwd": dir, "max_members": 4, "descr": "from bob"})
	require.Equal(t, 200, code, body)
	g := group("visible")
	require.Equal(t, dir, g.DefaultCwd)
	require.Equal(t, 4, g.MaxMembers)
	require.Equal(t, "from bob", g.Descr)
	// Fields outside groups.settings are refused before any part is applied.
	for _, field := range []map[string]any{
		{"environment": []any{}, "descr": "env"},
		{"permissions": []any{}},
		{"default_spawn_group": true},
	} {
		code, body = call("PATCH", "/api/groups/visible", field)
		require.Equal(t, 403, code, body)
	}
	require.Equal(t, "from bob", group("visible").Descr)
	code, body = call("POST", "/api/groups/visible/attachment", map[string]any{"url": "https://example.com/board"})
	require.Equal(t, 200, code, body)
	require.Equal(t, "https://example.com/board", group("visible").AttachmentURL)
	code, body = call("GET", "/api/groups/visible/attachment", nil)
	require.Equal(t, 200, code, body)
	// A group's sandbox profile also needs the spawn sandbox grant.
	code, body = call("PUT", "/api/groups/visible/sandbox-profile", map[string]any{"name": "no-such-profile"})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermSandboxProfilesManage)

	// The snapshot names the grants behind each group's controls.
	snap := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/snapshot", nil))
	require.Equal(t, 200, snap.Code, snap.Body.String())
	var out struct {
		PeerView struct {
			Groups map[string][]string `json:"groups"`
		} `json:"peer_view"`
	}
	require.NoError(t, json.Unmarshal(snap.Body.Bytes(), &out))
	require.Equal(t, map[string][]string{"visible": {agentd.PermGroupsSettings, agentd.PermGroupsMembersSpawnCwd}}, out.PeerView.Groups)

	// Grants follow the group, not its name.
	code, body = call("POST", "/api/groups/visible/rename", map[string]any{"new_name": "renamed"})
	require.Equal(t, 200, code, body)
	code, body = call("PATCH", "/api/groups/renamed", map[string]any{"max_members": 5})
	require.Equal(t, 200, code, body)
	require.Equal(t, 5, group("renamed").MaxMembers)

	// Unrestricted trust holds every peer grant, environment and sandbox
	// included; member permissions are never a peer's to set.
	peer, err := db.GetFederationPeer(p.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	code, body = call("PATCH", "/api/groups/hidden-group", map[string]any{"environment": []any{}, "max_members": 2})
	require.Equal(t, 200, code, body)
	require.Equal(t, 2, group("hidden-group").MaxMembers)
	code, body = call("PUT", "/api/groups/hidden-group/sandbox-profile", map[string]any{"name": "no-such-profile"})
	require.Equal(t, 404, code, body, "past the gate: the profile itself is missing")
	code, body = call("PATCH", "/api/groups/hidden-group", map[string]any{"permissions": []any{}})
	require.Equal(t, 403, code, body)
}
