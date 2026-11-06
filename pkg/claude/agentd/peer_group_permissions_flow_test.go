package agentd_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type peerGroupPermsRow struct {
	Name        string                         `json:"name"`
	Permissions []string                       `json:"permissions"`
	OwnerScopes map[string]map[string][]string `json:"owner_scopes"`
	Members     []struct {
		ConvID string `json:"conv_id"`
		Owner  bool   `json:"owner"`
	} `json:"members"`
}

func peerSnapshotGroup(t *testing.T, h *peerProcessHarness, name string) peerGroupPermsRow {
	t.Helper()
	code, body := h.call("GET", "/api/snapshot", nil)
	require.Equal(t, 200, code, body)
	var snap struct {
		Groups []peerGroupPermsRow `json:"groups"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &snap))
	for _, g := range snap.Groups {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("group %s not in the peer snapshot: %s", name, body)
	return peerGroupPermsRow{}
}

func groupPermissionSlugs(t *testing.T, name string) []string {
	t.Helper()
	g, err := db.GetAgentGroupByName(name)
	require.NoError(t, err)
	slugs, err := db.ListAgentGroupPermissions(g.ID)
	require.NoError(t, err)
	return slugs
}

// A peer replaces a group's member permissions only when it was shown them
// (the grant that edits them shows them), and adds or widens only rows within
// its own grants; a row it leaves as shown, or drops, needs nothing more.
func TestPeerGroupMemberPermissionsHoldTheCeiling(t *testing.T) {
	h := newPeerProcessHarness(t)
	require.NoError(t, db.ReplaceAgentGroupPermissionGrants(h.group.ID, []db.PermissionGrant{{Slug: agentd.PermSelfRename}}, "human"))
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	patch := func(rows ...any) (int, string) {
		return h.call("PATCH", "/api/groups/crew", map[string]any{"permissions": append([]any{}, rows...)})
	}

	require.Empty(t, peerSnapshotGroup(t, h, "crew").Permissions, "withheld without the grant")
	// Pinned: without the grant that shows the rows, no replacement of them,
	// even with the owner-scope grant there, which opens the same PATCH.
	h.grant(t, agentd.PermGroupsSettingsOwnerScopes, "group=crew")
	code, body := patch()
	require.Equal(t, 403, code, "an empty list would wipe rows the peer never saw: %s", body)
	require.Equal(t, []string{agentd.PermSelfRename}, groupPermissionSlugs(t, "crew"))

	// The grant alone opens the PATCH: no groups.settings needed.
	h.grant(t, agentd.PermGroupsSettingsMemberPermissions, "group=crew")
	require.Equal(t, []string{agentd.PermSelfRename}, peerSnapshotGroup(t, h, "crew").Permissions, "shown under the grant")
	// The operator's row, as shown, plus one the peer holds.
	code, body = patch(agentd.PermSelfRename, map[string]any{"slug": agentd.PermGroupsRosterRead, "scope": map[string]any{"group": []string{"crew"}}})
	require.Equal(t, 200, code, body)
	require.ElementsMatch(t, []string{agentd.PermSelfRename, agentd.PermGroupsRosterRead}, groupPermissionSlugs(t, "crew"))
	for _, row := range []any{
		map[string]any{"slug": agentd.PermGroupsRosterRead, "scope": map[string]any{}},
		map[string]any{"slug": agentd.PermGroupsMembersStop, "scope": map[string]any{"group": []string{"crew"}}},
		agentd.PermSelfCompact,
	} {
		code, body = patch(agentd.PermSelfRename, row)
		require.Equal(t, 403, code, "%v: %s", row, body)
	}
	require.ElementsMatch(t, []string{agentd.PermSelfRename, agentd.PermGroupsRosterRead}, groupPermissionSlugs(t, "crew"), "a refused save writes nothing")
	code, body = patch()
	require.Equal(t, 200, code, "dropping rows only takes authority away: %s", body)
	require.Empty(t, groupPermissionSlugs(t, "crew"))
}

// Narrowing owner scopes needs only the grant; loosening one widens every
// owner's grant, which the peer's ceiling must cover.
func TestPeerGroupOwnerScopesNarrowFreely(t *testing.T) {
	h := newPeerProcessHarness(t)
	_, err := db.SetAgentGroupOwnerScopes("crew", `{"groups.members.spawn":{"spawn_profile":["a","b"]}}`)
	require.NoError(t, err)
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	set := func(scopes map[string]any) (int, string) {
		return h.call("PATCH", "/api/groups/crew", map[string]any{"owner_scopes": scopes})
	}
	code, body := set(map[string]any{"groups.members.spawn": map[string]any{"spawn_profile": []string{"a"}}})
	require.Equal(t, 403, code, "without the grant: %s", body)
	// The grant alone opens the PATCH, for that field only.
	h.grant(t, agentd.PermGroupsSettingsOwnerScopes, "group=crew")
	code, body = h.call("PATCH", "/api/groups/crew", map[string]any{"descr": "x"})
	require.Equal(t, 403, code, "a field of groups.settings: %s", body)
	require.Contains(t, peerSnapshotGroup(t, h, "crew").OwnerScopes, "groups.members.spawn", "shown under the grant")

	code, body = set(map[string]any{"groups.members.spawn": map[string]any{"spawn_profile": []string{"a"}}, "groups.members.stop": map[string]any{"group": []string{"crew"}}})
	require.Equal(t, 200, code, "a narrowing: %s", body)
	for _, loosen := range []map[string]any{
		{"groups.members.spawn": map[string]any{"spawn_profile": []string{"a", "c"}}},
		{},
	} {
		code, body = set(loosen)
		require.Equal(t, 403, code, "%v: %s", loosen, body)
	}
	g, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	require.Contains(t, g.OwnerScopesJSON, `"a"`)
	require.NotContains(t, g.OwnerScopesJSON, `"b"`)
}

// A peer removes owners under groups.owners.manage; making an owner confers
// the whole owner tier, spawning included, which no restricted peer may
// confer. Unrestricted trust adds owners as this node's operator does.
func TestPeerGroupOwnersHoldTheOwnerTier(t *testing.T) {
	h := newPeerProcessHarness(t)
	require.NoError(t, db.AddAgentGroupOwner(h.group.ID, "proc-crew-member", "human"))
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	require.False(t, peerSnapshotGroup(t, h, "crew").Members[0].Owner, "withheld without the grant")

	code, body := h.call("DELETE", "/api/groups/crew/owners/proc-crew-member", nil)
	require.Equal(t, 403, code, body)
	h.grant(t, agentd.PermGroupsOwnersManage, "group=crew")
	require.True(t, peerSnapshotGroup(t, h, "crew").Members[0].Owner, "shown under the grant")
	code, body = h.call("DELETE", "/api/groups/crew/owners/proc-crew-member", nil)
	require.Less(t, code, 300, body)
	owners, err := db.ListAgentGroupOwners(h.group.ID)
	require.NoError(t, err)
	require.Empty(t, owners)

	code, body = h.call("POST", "/api/groups/crew/owners", map[string]any{"conv": "proc-crew-member"})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawn, "names what an owner holds beyond the peer")
	h.f.HaveGroup("other")
	havePerformerAgent(t, h.f, "other", "outsider-conv", "outsider", "dev", "offline")
	h.grant(t, agentd.PermGroupsRosterRead, "group=other")
	code, body = h.call("POST", "/api/groups/crew/owners", map[string]any{"conv": "outsider-conv"})
	require.Equal(t, 403, code, "only a member: %s", body)

	h.trustUnrestricted(t)
	code, body = h.call("POST", "/api/groups/crew/owners", map[string]any{"conv": "proc-crew-member"})
	require.Equal(t, 200, code, body)
	owners, err = db.ListAgentGroupOwners(h.group.ID)
	require.NoError(t, err)
	require.Len(t, owners, 1)
}
