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
	// ...and carries the values those grants let it change, so its editors
	// start from them; the environment stays its own grant's.
	var rows struct {
		Groups []map[string]any `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(snap.Body.Bytes(), &rows))
	require.Len(t, rows.Groups, 1)
	require.Equal(t, dir, rows.Groups[0]["default_cwd"])
	require.Equal(t, float64(4), rows.Groups[0]["max_members"])
	require.Equal(t, "https://example.com/board", rows.Groups[0]["attachment_url"])
	require.Equal(t, true, rows.Groups[0]["notify_enabled"])
	require.Nil(t, rows.Groups[0]["environment"])

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

// The ⚙ menu's settings run on the same grant: a peer nests a group only
// under one this node shows it, and the default saved prompts, which open
// every spawn's brief, also need the grant to choose a spawn's prompts.
func TestPeerGroupNestingAndDefaultPrompts(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("child")
	f.HaveGroup("shown")
	f.HaveGroup("hidden-group")
	grant := func(slug, group string) {
		t.Helper()
		rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=" + group})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	h := agentd.PeerViewHandler(p.id.ID())
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
		return rec.Code, rec.Body.String()
	}
	parentOf := func(name string) string {
		t.Helper()
		g, err := db.GetAgentGroupByName(name)
		require.NoError(t, err)
		if g.ParentGroupID == nil {
			return ""
		}
		parent, err := db.GetAgentGroupByID(*g.ParentGroupID)
		require.NoError(t, err)
		return parent.Name
	}

	grant(agentd.PermGroupsSettings, "child")
	grant(agentd.PermGroupsRosterRead, "shown")
	code, body := call("PUT", "/api/groups/child/parent", map[string]any{"parent": "hidden-group"})
	require.Equal(t, 404, code, body)
	require.Empty(t, parentOf("child"))
	code, body = call("PUT", "/api/groups/child/parent", map[string]any{"parent": "shown"})
	require.Equal(t, 200, code, body)
	require.Equal(t, "shown", parentOf("child"))
	code, body = call("PUT", "/api/groups/child/parent", map[string]any{"parent": ""})
	require.Equal(t, 200, code, body)
	require.Empty(t, parentOf("child"))
	// Nesting the shown group itself needs a settings grant there.
	code, body = call("PUT", "/api/groups/shown/parent", map[string]any{"parent": "child"})
	require.Equal(t, 403, code, body)

	code, body = call("PATCH", "/api/groups/child", map[string]any{"default_prompts": []string{}})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawnPrompts)
	grant(agentd.PermGroupsMembersSpawnPrompts, "child")
	code, body = call("PATCH", "/api/groups/child", map[string]any{"default_prompts": []string{}})
	require.Equal(t, 200, code, body)

	snap := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/snapshot", nil))
	require.Equal(t, 200, snap.Code, snap.Body.String())
	var out struct {
		PeerView struct {
			Groups map[string][]string `json:"groups"`
		} `json:"peer_view"`
	}
	require.NoError(t, json.Unmarshal(snap.Body.Bytes(), &out))
	require.Equal(t, map[string][]string{"child": {agentd.PermGroupsSettings, agentd.PermGroupsMembersSpawnPrompts}}, out.PeerView.Groups)
}
