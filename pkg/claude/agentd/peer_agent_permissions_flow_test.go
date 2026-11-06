package agentd_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type peerPermsSnapshot struct {
	Permissions struct {
		Overrides       map[string]map[string]string `json:"overrides"`
		ScopeDimOptions map[string]struct {
			Values []string `json:"values"`
		} `json:"scope_dim_options"`
	} `json:"permissions"`
	Slugs []struct{ Slug string } `json:"slugs"`
}

func peerPermissionsView(t *testing.T, h *peerProcessHarness) peerPermsSnapshot {
	t.Helper()
	code, body := h.call("GET", "/api/snapshot", nil)
	require.Equal(t, 200, code, body)
	var snap peerPermsSnapshot
	require.NoError(t, json.Unmarshal([]byte(body), &snap))
	return snap
}

// A peer edits an agent's own overrides under permissions.grant and
// permissions.revoke held in every group of the agent: what it grants is held
// to the peer ceiling, what it denies or drops needs only the revoke grant,
// and lifting a deny restores what the agent inherits, so it is a grant.
func TestPeerAgentPermissionsHoldTheCeiling(t *testing.T) {
	h := newPeerProcessHarness(t)
	const conv = "proc-crew-member"
	h.f.HaveGroup("secret")
	require.NoError(t, db.SetAgentPermissionOverrideWithScope(conv, agentd.PermSelfRename, db.PermEffectDeny, "", "human"))
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	post := func(overrides map[string]string, scopes map[string]any) (int, string) {
		body := map[string]any{"conv": h.member, "overrides": overrides}
		if scopes != nil {
			body["scopes"] = scopes
		}
		return h.call("POST", "/api/permissions", body)
	}
	crew := map[string]any{agentd.PermGroupsRosterRead: map[string]any{"group": []string{"crew"}}}

	require.Empty(t, peerPermissionsView(t, h).Permissions.Overrides, "withheld without the grant")
	code, body := post(map[string]string{agentd.PermGroupsRosterRead: "grant"}, crew)
	require.Equal(t, 403, code, body)

	h.grant(t, agentd.PermPermissionsGrant, "group=crew")
	snap := peerPermissionsView(t, h)
	require.Equal(t, map[string]string{agentd.PermSelfRename: "deny"}, snap.Permissions.Overrides[conv], "shown under the grant")
	require.NotEmpty(t, snap.Slugs, "the editor's rows")
	require.Equal(t, []string{"crew"}, snap.Permissions.ScopeDimOptions["group"].Values, "only the groups the peer sees")

	code, body = post(map[string]string{agentd.PermGroupsRosterRead: "grant", agentd.PermSelfRename: "deny"}, crew)
	require.Equal(t, 200, code, "within the ceiling, the stored deny as shown: %s", body)
	rows, err := db.ListAgentPermissionOverrideRowsForConv(conv)
	require.NoError(t, err)
	for _, row := range rows {
		if row.Slug == agentd.PermGroupsRosterRead {
			require.Contains(t, row.GrantedBy, "peer:", "recorded as the peer's")
		}
	}
	for _, refused := range []struct {
		overrides map[string]string
		scopes    map[string]any
	}{
		{map[string]string{agentd.PermGroupsRosterRead: "grant"}, map[string]any{agentd.PermGroupsRosterRead: map[string]any{}}},
		{map[string]string{agentd.PermGroupsRosterRead: "grant"}, map[string]any{agentd.PermGroupsRosterRead: map[string]any{"group": []string{"secret"}}}},
		{map[string]string{agentd.PermGroupsMembersStop: "grant"}, nil},
		{map[string]string{agentd.PermSelfRename: "default"}, nil},
		{map[string]string{agentd.PermGroupsPresenceRead: "deny"}, nil},
	} {
		code, body = post(refused.overrides, refused.scopes)
		require.Equal(t, 403, code, "%v %v: %s", refused.overrides, refused.scopes, body)
	}
	code, body = h.call("POST", "/api/permissions", map[string]any{"conv": h.member, "overrides": map[string]string{agentd.PermGroupsRosterRead: "grant"}, "carry": map[string]bool{agentd.PermGroupsRosterRead: true}})
	require.Equal(t, 403, code, body)

	// Taking authority away needs the revoke grant.
	h.grant(t, agentd.PermPermissionsRevoke, "group=crew")
	code, body = post(map[string]string{agentd.PermGroupsPresenceRead: "deny", agentd.PermGroupsRosterRead: "deny"}, nil)
	require.Equal(t, 200, code, body)
	overrides, err := agentPermissionOverridesForTest(conv)
	require.NoError(t, err)
	require.Equal(t, map[string]string{agentd.PermSelfRename: "deny", agentd.PermGroupsPresenceRead: "deny", agentd.PermGroupsRosterRead: "deny"}, overrides)

	// The agent joins a group the peer holds nothing in: every group counts.
	h.f.HaveMemberWithRole("secret", conv, "dev")
	code, body = post(map[string]string{agentd.PermGroupsPresenceRead: "default"}, nil)
	require.Equal(t, 404, code, "an agent in a group the peer cannot see: %s", body)
	require.Empty(t, peerPermissionsView(t, h).Permissions.Overrides[conv], "no longer shown")
}

// A peer-written override names a group by name, so it pins the name as a
// peer-written group row does.
func TestPeerAgentPermissionsPinTheirGroupNames(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	h.grant(t, agentd.PermGroupsSettings, "group=crew")
	h.grant(t, agentd.PermPermissionsGrant, "group=crew")
	code, body := h.call("POST", "/api/permissions", map[string]any{"conv": h.member,
		"overrides": map[string]string{agentd.PermGroupsRosterRead: "grant"},
		"scopes":    map[string]any{agentd.PermGroupsRosterRead: map[string]any{"group": []string{"crew"}}}})
	require.Equal(t, 200, code, body)
	code, body = h.call("POST", "/api/groups/crew/rename", map[string]any{"new_name": "crew-old"})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, "a peer conferred")

	h.trustUnrestricted(t)
	code, body = h.call("POST", "/api/permissions", map[string]any{"conv": h.member,
		"overrides": map[string]string{agentd.PermGroupsMembersStop: "grant"}})
	require.Equal(t, 200, code, "unrestricted trust edits as this node's operator: %s", body)
}

func agentPermissionOverridesForTest(conv string) (map[string]string, error) {
	rows, err := db.ListAgentPermissionOverrideRowsForConv(conv)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, row := range rows {
		out[row.Slug] = row.Effect
	}
	return out, nil
}

// A batch that changes nothing still takes a permissions grant: the route
// answers with what it stored. A peer's answer never carries the overrides,
// and the projection shows a scope naming what the peer cannot see as
// unreadable, which the peer may not change.
func TestPeerAgentPermissionsShowOnlyWhatThePeerSees(t *testing.T) {
	h := newPeerProcessHarness(t)
	const conv = "proc-crew-member"
	h.f.HaveGroup("secret")
	require.NoError(t, db.SetAgentPermissionOverrideWithScope(conv, agentd.PermGroupsRosterRead, db.PermEffectGrant, `{"group":["secret"]}`, "human"))
	h.grant(t, agentd.PermGroupsPresenceRead, "group=crew")
	noop := map[string]any{"conv": h.member, "overrides": map[string]string{agentd.PermGroupsMembersStop: "default"}}
	code, body := h.call("POST", "/api/permissions", noop)
	require.Equal(t, 403, code, "a no-op batch without either grant: %s", body)
	require.NotContains(t, body, "secret")

	h.grant(t, agentd.PermPermissionsGrant, "group=crew")
	code, body = h.call("POST", "/api/permissions", noop)
	require.Equal(t, 200, code, body)
	require.NotContains(t, body, "secret", "the answer carries no overrides")

	code, body = h.call("GET", "/api/snapshot", nil)
	require.Equal(t, 200, code, body)
	require.NotContains(t, body, `"secret"`, "a scope naming a hidden group is shown as unreadable")
	var snap struct {
		Permissions struct {
			UnreadableScopes map[string][]string `json:"unreadable_scopes"`
		} `json:"permissions"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &snap))
	require.Equal(t, []string{agentd.PermGroupsRosterRead}, snap.Permissions.UnreadableScopes[conv])
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	code, body = h.call("POST", "/api/permissions", map[string]any{"conv": h.member,
		"overrides": map[string]string{agentd.PermGroupsRosterRead: "grant"},
		"scopes":    map[string]any{agentd.PermGroupsRosterRead: map[string]any{"group": []string{"crew"}}}})
	require.Equal(t, 403, code, "replacing a scope the peer was not shown: %s", body)
}

// An override shadows what the agent inherits, so dropping a scoped grant
// may widen it: that takes permissions.grant and the ceiling, as lifting a
// deny does. Dropping an unscoped grant only takes authority away.
func TestPeerAgentPermissionsDroppingANarrowingIsAGrant(t *testing.T) {
	h := newPeerProcessHarness(t)
	const conv = "proc-crew-member"
	require.NoError(t, db.SetAgentPermissionOverrideWithScope(conv, agentd.PermGroupsMembersRename, db.PermEffectGrant, `{"group":["crew"]}`, "human"))
	require.NoError(t, db.SetAgentPermissionOverrideWithScope(conv, agentd.PermGroupsMembersTask, db.PermEffectGrant, "", "human"))
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	h.grant(t, agentd.PermPermissionsRevoke, "group=crew")
	drop := func(slug string) (int, string) {
		return h.call("POST", "/api/permissions", map[string]any{"conv": h.member, "overrides": map[string]string{slug: "default"}})
	}
	code, body := drop(agentd.PermGroupsMembersRename)
	require.Equal(t, 403, code, "restores what the agent inherits: %s", body)
	code, body = drop(agentd.PermGroupsMembersTask)
	require.Equal(t, 200, code, body)
	overrides, err := agentPermissionOverridesForTest(conv)
	require.NoError(t, err)
	require.Equal(t, map[string]string{agentd.PermGroupsMembersRename: "grant"}, overrides)
}
