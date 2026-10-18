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
	// included, and sets what no grant covers (member permissions) as this
	// node's operator does.
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
	require.Equal(t, 200, code, body)
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

// A group's ⚙ shut down and power on run under the member lifecycle grants
// for that group, through the group's own routes, and only on members whose
// every group grants them (as a per-agent stop or wake).
func TestPeerGroupPowerUnderLifecycleGrants(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("crew")
	f.HaveGroup("secret")
	f.HaveConvWithTitle("pwr-solo", "solo")
	f.HaveConvWithTitle("pwr-shared", "shared")
	f.HaveAliveSession("pwr-solo", "spwn-pwr-solo", "tmux-pwr-solo", f.World.HomeDir)
	f.HaveAliveSession("pwr-shared", "spwn-pwr-shared", "tmux-pwr-shared", f.World.HomeDir)
	f.HaveMember("crew", "pwr-solo")
	f.HaveMember("crew", "pwr-shared")
	f.HaveMember("secret", "pwr-shared")
	grant := func(slug, group string) {
		t.Helper()
		rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=" + group})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	h := agentd.PeerViewHandler(p.id.ID())
	call := func(path string) (int, string) {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", path, map[string]any{"grace_ms": 60}))
		return rec.Code, rec.Body.String()
	}

	grant(agentd.PermGroupsRosterRead, "crew")
	code, body := call("/api/groups/crew/shutdown")
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsMembersStop)
	grant(agentd.PermGroupsMembersStop, "crew")
	// A member also in secret, where the peer holds nothing, is not crew's
	// to stop: nothing is.
	code, body = call("/api/groups/crew/shutdown")
	require.Equal(t, 403, code, body)
	require.Contains(t, body, "every affected group")
	require.True(t, f.World.Tmux.IsAlive("tmux-pwr-solo"))
	require.True(t, f.World.Tmux.IsAlive("tmux-pwr-shared"))
	grant(agentd.PermGroupsMembersStop, "secret")
	code, body = call("/api/groups/crew/shutdown")
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"group":"crew"`)
	require.False(t, f.World.Tmux.IsAlive("tmux-pwr-solo"))
	require.False(t, f.World.Tmux.IsAlive("tmux-pwr-shared"))

	code, body = call("/api/groups/crew/power-on")
	require.Equal(t, 403, code, body)
	grant(agentd.PermGroupsMembersResume, "crew")
	code, body = call("/api/groups/crew/power-on")
	require.Equal(t, 403, code, body, "the shared member also needs secret's resume grant")
	grant(agentd.PermGroupsMembersResume, "secret")
	code, body = call("/api/groups/crew/power-on")
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"resumed":2`)

	snap := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/snapshot", nil))
	var out struct {
		PeerView struct {
			Groups map[string][]string `json:"groups"`
		} `json:"peer_view"`
	}
	require.NoError(t, json.Unmarshal(snap.Body.Bytes(), &out))
	require.Equal(t, []string{agentd.PermGroupsMembersStop, agentd.PermGroupsMembersResume}, out.PeerView.Groups["crew"])
}

// A group's ⚙ cleanup, its delete and the delete's retiring of members first
// remove what no peer grant hands out: a restricted peer is refused whatever
// it holds in the group, and unrestricted trust runs them on this node.
func TestPeerGroupDeleteAndCleanupUnderUnrestrictedTrust(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("crew")
	f.HaveGroup("doomed")
	f.HaveConvWithTitle("cln-off", "offline")
	f.HaveConvWithTitle("cln-doomed", "doomed member")
	f.HaveMember("crew", "cln-off")
	f.HaveMember("doomed", "cln-doomed")
	for _, group := range []string{"crew", "doomed"} {
		for _, slug := range []string{agentd.PermGroupsRosterRead, agentd.PermGroupsSettings, agentd.PermGroupsMembersRetire} {
			rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=" + group})
			require.Equal(t, 200, rec.Code, rec.Body.String())
		}
	}
	h := agentd.PeerViewHandler(p.id.ID())
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
		return rec.Code, rec.Body.String()
	}
	cleanup := map[string]any{"group": "crew", "members": []string{"cln-off"}}
	retire := map[string]any{"convs": []string{"cln-doomed"}, "shutdown": true, "delete_worktree": false}

	for _, req := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/cleanup/group", cleanup},
		{"POST", "/api/groups/doomed/retire", retire},
		{"DELETE", "/api/groups/doomed", nil},
	} {
		code, body := call(req.method, req.path, req.body)
		require.Equal(t, 403, code, "%s %s: %s", req.method, req.path, body)
	}
	members := func(group string) int {
		t.Helper()
		g, err := db.GetAgentGroupByName(group)
		require.NoError(t, err)
		require.NotNil(t, g)
		rows, err := db.ListAgentGroupMembers(g.ID)
		require.NoError(t, err)
		return len(rows)
	}
	require.Equal(t, 1, members("crew"))
	require.Equal(t, 1, members("doomed"))

	peer, err := db.GetFederationPeer(p.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))

	code, body := call("POST", "/api/cleanup/group", cleanup)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"removed":1`)
	require.Equal(t, 0, members("crew"))
	code, body = call("POST", "/api/groups/doomed/retire", retire)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"action":"retired"`)
	code, body = call("DELETE", "/api/groups/doomed", nil)
	require.Equal(t, 204, code, body)
	g, err := db.GetAgentGroupByName("doomed")
	require.NoError(t, err)
	require.Nil(t, g)
}

// A peer's operator changes an agent's ordinary settings through this node's
// own dashboard handlers under groups.members.manage: its own settings need the
// grant in every group it is in, its role in a group that group's grant, and
// the peer's metadata names the agents it may change.
func TestPeerMemberSettingsUnderManageGrant(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("crew")
	f.HaveGroup("secret")
	f.HaveConvWithTitle("mbr-solo", "solo")
	f.HaveConvWithTitle("mbr-shared", "shared")
	f.HaveAliveSession("mbr-solo", "spwn-mbr-solo", "tmux-mbr-solo", f.World.HomeDir)
	f.HaveMember("crew", "mbr-solo")
	f.HaveMember("crew", "mbr-shared")
	f.HaveMember("secret", "mbr-shared")
	solo, err := db.AgentIDForConv("mbr-solo")
	require.NoError(t, err)
	shared, err := db.AgentIDForConv("mbr-shared")
	require.NoError(t, err)
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
	tags := func(agent string) (int, string) {
		return call("POST", "/api/agents/"+agent+"/tags", map[string]any{"tags": []string{"remote"}})
	}

	grant(agentd.PermGroupsRosterRead, "crew")
	code, body := tags(solo)
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsMembersManage)
	grant(agentd.PermGroupsMembersManage, "crew")

	code, body = tags(solo)
	require.Equal(t, 200, code, body)
	got, err := db.ListAgentTags(solo)
	require.NoError(t, err)
	require.Equal(t, []string{"remote"}, got)
	code, body = call("POST", "/api/agents/"+solo+"/notify", map[string]any{"mode": "off"})
	require.Equal(t, 200, code, body)
	code, body = call("POST", "/api/agents/"+solo+"/task", map[string]any{"url": "https://example.com/issue/1"})
	require.Equal(t, 200, code, body)
	code, body = call("POST", "/api/agents/"+solo+"/rename", map[string]any{"title": "renamed"})
	require.NotEqual(t, 403, code, body)
	require.NotEqual(t, 404, code, body)

	// The shared agent is also in secret, where the peer holds nothing: its
	// own settings are not crew's to change, but its role in crew is.
	for _, verb := range []string{"tags", "notify", "task", "rename"} {
		code, body = call("POST", "/api/agents/"+shared+"/"+verb, map[string]any{})
		require.Equal(t, 403, code, "%s: %s", verb, body)
	}
	code, body = call("PATCH", "/api/groups/crew/members/"+shared, map[string]any{"role": "reviewer"})
	require.Equal(t, 200, code, body)
	code, body = call("PATCH", "/api/groups/secret/members/"+shared, map[string]any{"role": "reviewer"})
	require.Equal(t, 404, code, "secret is not visible: %s", body)

	code, body = call("PATCH", "/api/groups/crew/members/"+shared, map[string]any{"descr": "reviews"})
	require.Equal(t, 200, code, body)
	require.NoError(t, db.ReplaceAgentTags(shared, []string{"private"}))

	snap := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/snapshot", nil))
	var out struct {
		Groups []struct {
			Name    string `json:"name"`
			Members []struct {
				AgentID string   `json:"agent_id"`
				Descr   string   `json:"descr"`
				Tags    []string `json:"tags"`
				Notify  string   `json:"notify"`
			} `json:"members"`
		} `json:"groups"`
		PeerView struct {
			Groups map[string][]string `json:"groups"`
			Agents map[string][]string `json:"agents"`
		} `json:"peer_view"`
	}
	require.NoError(t, json.Unmarshal(snap.Body.Bytes(), &out))
	require.Equal(t, map[string][]string{solo: {agentd.PermGroupsMembersManage}}, out.PeerView.Agents)
	require.Contains(t, out.PeerView.Groups["crew"], agentd.PermGroupsMembersManage)
	// The editors start from what they change: the description where the
	// group's grant reaches, the tags and notification override where the
	// agent's does; another agent's tags stay withheld.
	require.Len(t, out.Groups, 1)
	byAgent := map[string]int{}
	for i, m := range out.Groups[0].Members {
		byAgent[m.AgentID] = i
	}
	soloRow, sharedRow := out.Groups[0].Members[byAgent[solo]], out.Groups[0].Members[byAgent[shared]]
	require.Equal(t, []string{"remote"}, soloRow.Tags)
	require.Equal(t, "off", soloRow.Notify)
	require.Equal(t, "reviews", sharedRow.Descr)
	require.Empty(t, sharedRow.Tags)
}

// An agent's removal from a group, its permanent delete (and the delete
// dialog's worktree read), its remote access, fast mode, sandbox
// implementation, debug export and reincarnation reach what no peer grant hands out: a restricted peer is refused whatever
// it holds, and unrestricted trust runs them on this node.
func TestPeerAgentManagementUnderUnrestrictedTrust(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("crew")
	f.HaveConvWithTitle("full-keep", "keep")
	f.HaveConvWithTitle("full-gone", "gone")
	f.HaveMember("crew", "full-keep")
	f.HaveMember("crew", "full-gone")
	keep, err := db.AgentIDForConv("full-keep")
	require.NoError(t, err)
	gone, err := db.AgentIDForConv("full-gone")
	require.NoError(t, err)
	for _, slug := range []string{agentd.PermGroupsRosterRead, agentd.PermGroupsMembersManage, agentd.PermGroupsMembersRetire} {
		rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=crew"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	h := agentd.PeerViewHandler(p.id.ID())
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
		return rec.Code, rec.Body.String()
	}
	for _, req := range []struct{ method, path string }{
		{"DELETE", "/api/groups/crew/members/" + keep},
		{"DELETE", "/api/agents/" + gone},
		{"GET", "/api/agents/" + gone + "/worktree"},
		{"POST", "/api/agents/" + keep + "/remote-control"},
		{"POST", "/api/agents/" + keep + "/fast-mode/enable"},
		{"POST", "/api/agents/" + keep + "/reincarnate"},
		{"GET", "/api/agents/" + keep + "/sandbox-impl"},
		{"POST", "/api/agents/" + keep + "/sandbox-impl"},
		{"GET", "/api/agents/" + keep + "/debug-export"},
	} {
		code, body := call(req.method, req.path, nil)
		require.Equal(t, 403, code, "%s %s: %s", req.method, req.path, body)
	}
	crew, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	members := func() int {
		t.Helper()
		rows, err := db.ListAgentGroupMembers(crew.ID)
		require.NoError(t, err)
		return len(rows)
	}
	require.Equal(t, 2, members())

	peer, err := db.GetFederationPeer(p.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))

	code, body := call("GET", "/api/agents/"+keep+"/debug-export", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, "full-keep")
	// The fixture records no launch profile: this node's own handler answers.
	code, body = call("GET", "/api/agents/"+keep+"/sandbox-impl", nil)
	require.Equal(t, 409, code, body)
	require.Contains(t, body, "relaunch_profile")
	code, body = call("GET", "/api/agents/"+gone+"/worktree", nil)
	require.Equal(t, 200, code, body)
	// An escaped slash names no agent: it never reaches another verb.
	code, body = call("DELETE", "/api/agents/"+gone+"%2Fexports", nil)
	require.Equal(t, 404, code, body)
	code, body = call("DELETE", "/api/agents/"+gone, nil)
	require.Less(t, code, 300, body)
	require.Equal(t, 1, members())
	code, body = call("DELETE", "/api/groups/crew/members/"+keep, nil)
	require.Less(t, code, 300, body)
	require.Equal(t, 0, members())
}
