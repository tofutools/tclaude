package agentd_test

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func peerGroupHasMember(t *testing.T, group, conv string) bool {
	t.Helper()
	g, err := db.GetAgentGroupByName(group)
	require.NoError(t, err)
	require.NotNil(t, g)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	for _, m := range members {
		if m.ConvID == conv {
			return true
		}
	}
	return false
}

// Adding an agent takes groups.members.add in the group and in every group
// the agent is already in: joining puts it under another group's settings.
func TestPeerMemberAddHoldsTheAgentsGroups(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.f.HaveGroup("squad")
	havePerformerAgent(t, h.f, "squad", "sq-conv", "sq", "dev", "offline")
	add := func(body map[string]any) (int, string) { return h.call("POST", "/api/groups/crew/members", body) }

	code, body := add(map[string]any{"conv": "sq-conv"})
	require.Equal(t, 404, code, "crew is not visible: %s", body)
	h.grant(t, agentd.PermGroupsMembersAdd, "group=crew")
	code, body = add(map[string]any{"conv": "sq-conv"})
	require.Equal(t, 404, code, "the agent is not visible: %s", body)
	h.grant(t, agentd.PermGroupsRosterRead, "group=squad")
	code, body = add(map[string]any{"conv": "sq-conv"})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, "group:squad")
	code, body = add(map[string]any{"conv": "sq-conv", "rolE": "boss"})
	require.Equal(t, 403, code, "a look-alike key: %s", body)
	h.grant(t, agentd.PermGroupsMembersAdd, "group=squad")
	code, body = add(map[string]any{"conv": "sq-conv", "role": "dev"})
	require.Less(t, code, 300, body)
	require.True(t, peerGroupHasMember(t, "crew", "sq-conv"))
}

// Leaving a group may hand the agent another group's environment or sandbox,
// which takes that group's spawn grant; a move takes the target's.
func TestPeerMemberRemoveAndMoveHoldTheLaunchTheyHandOver(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.f.HaveGroup("squad")
	havePerformerAgent(t, h.f, "crew", "dual-conv", "dual", "dev", "offline")
	h.f.HaveMemberWithRole("squad", "dual-conv", "dev")
	_, err := db.SetAgentGroupEnvironment("squad", []sandboxpolicy.EnvironmentEntry{{Name: "A", Value: "b"}})
	require.NoError(t, err)
	remove := func(conv string) (int, string) { return h.call("DELETE", "/api/groups/crew/members/"+conv, nil) }

	h.grant(t, agentd.PermGroupsRosterRead, "group=squad")
	code, body := remove("dual-conv")
	require.Equal(t, 404, code, "crew is not visible: %s", body)
	h.grant(t, agentd.PermGroupsMembersRemove, "group=crew")
	code, body = remove("dual-conv")
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawnEnvironment)
	require.Contains(t, body, "group:squad")
	h.grant(t, agentd.PermGroupsMembersSpawn, "group=squad")
	h.grant(t, agentd.PermGroupsMembersSpawnEnvironment, "group=squad")
	code, body = remove("dual-conv")
	require.Less(t, code, 300, body)
	require.False(t, peerGroupHasMember(t, "crew", "dual-conv"))

	move := func(body map[string]any) (int, string) {
		return h.call("POST", "/api/groups/crew/members/proc-crew-member/move", body)
	}
	code, body = move(map[string]any{"to": "squad"})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsMembersAdd)
	h.grant(t, agentd.PermGroupsMembersAdd, "group=squad")
	code, body = move(map[string]any{"to": "squad", "keep_permissions": true})
	require.Equal(t, 403, code, "kept permissions are the operator's: %s", body)
	code, body = move(map[string]any{"to": "squad"})
	require.Equal(t, 200, code, body)
	require.True(t, peerGroupHasMember(t, "squad", "proc-crew-member"))
	require.False(t, peerGroupHasMember(t, "crew", "proc-crew-member"))
}

// A link takes groups.link.add in both its groups; removing one takes
// groups.link.remove in its source.
func TestPeerGroupLinksHoldBothEnds(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.f.HaveGroup("squad")
	h.grant(t, agentd.PermGroupsLinkAdd, "group=crew")
	h.grant(t, agentd.PermGroupsRosterRead, "group=squad")
	code, body := h.call("POST", "/api/groups/crew/links", map[string]any{"to": "squad"})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, "group:squad")
	code, body = h.call("POST", "/api/groups/crew/links", map[string]any{"to": "squad", "to_peer": "other"})
	require.Equal(t, 403, code, "a link to another node: %s", body)
	h.grant(t, agentd.PermGroupsLinkAdd, "group=squad")
	code, body = h.call("POST", "/api/groups/crew/links", map[string]any{"to": "squad"})
	require.Equal(t, 200, code, body)
	var link struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &link))
	require.NotZero(t, link.ID, body)
	id := strconv.FormatInt(link.ID, 10)

	code, body = h.call("PATCH", "/api/groups/squad/links/"+id, map[string]any{"mode": "cc"})
	require.NotEqual(t, 403, code, "a mode change under both ends' grants: %s", body)
	h.revoke(t, agentd.PermGroupsLinkAdd, "group=squad")
	code, body = h.call("PATCH", "/api/groups/crew/links/"+id, map[string]any{"mode": "cc"})
	require.Equal(t, 403, code, "a mode change takes both ends: %s", body)
	require.Contains(t, body, "group:squad")

	code, body = h.call("DELETE", "/api/groups/squad/links/"+id, nil)
	require.Equal(t, 403, code, "removal takes the grant in the source: %s", body)
	require.Contains(t, body, "group:crew")
	h.grant(t, agentd.PermGroupsLinkRemove, "group=crew")
	code, body = h.call("DELETE", "/api/groups/squad/links/"+id, nil)
	require.Less(t, code, 300, body)
	code, body = h.call("DELETE", "/api/groups/crew/links/xlink_abc", nil)
	require.Equal(t, 403, code, body)
}

// Archive, unarchive and delete take their grant in the group.
func TestPeerGroupArchiveAndDelete(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.f.HaveGroup("temp")
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	code, body := h.call("POST", "/api/groups/crew/archive", nil)
	require.Equal(t, 403, code, body)
	h.grant(t, agentd.PermGroupsArchive, "group=crew")
	code, body = h.call("POST", "/api/groups/crew/archive", nil)
	require.Equal(t, 200, code, body)
	g, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	require.True(t, g.IsArchived())
	code, body = h.call("POST", "/api/groups/crew/unarchive", nil)
	require.Equal(t, 200, code, body)
	g, err = db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	require.False(t, g.IsArchived())

	h.grant(t, agentd.PermGroupsRosterRead, "group=temp")
	code, body = h.call("DELETE", "/api/groups/temp", nil)
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsDelete)
	h.grant(t, agentd.PermGroupsDelete, "group=temp")
	code, body = h.call("DELETE", "/api/groups/temp", nil)
	require.Less(t, code, 300, body)
	g, err = db.GetAgentGroupByName("temp")
	require.NoError(t, err)
	require.Nil(t, g)
}

// The settings an agent may take from its other groups count its archived
// memberships too, and a delete counts agents that moved away keeping a
// setting on the group: the rebind reaches both.
func TestPeerLeaveChecksEveryGroupTheRebindReaches(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.f.HaveGroup("attic")
	havePerformerAgent(t, h.f, "crew", "arch-conv", "arch", "dev", "offline")
	h.f.HaveMemberWithRole("attic", "arch-conv", "dev")
	_, err := db.SetAgentGroupEnvironment("attic", []sandboxpolicy.EnvironmentEntry{{Name: "A", Value: "b"}})
	require.NoError(t, err)
	require.NoError(t, db.ArchiveAgentGroup("attic"))
	h.grant(t, agentd.PermGroupsMembersRemove, "group=crew")
	h.grant(t, agentd.PermGroupsMembersSpawn, "")
	code, body := h.call("DELETE", "/api/groups/crew/members/arch-conv", nil)
	require.Equal(t, 403, code, "an archived group may become its source: %s", body)
	require.Contains(t, body, "a group you cannot see")
	require.NotContains(t, body, "attic")

	temp := h.f.HaveGroup("temp")
	squad := h.f.HaveGroup("squad")
	_, err = db.SetAgentGroupEnvironment("squad", []sandboxpolicy.EnvironmentEntry{{Name: "A", Value: "b"}})
	require.NoError(t, err)
	agentID := havePerformerAgent(t, h.f, "squad", "pinned-conv", "pinned", "dev", "offline")
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &sandboxpolicy.Snapshot{Version: sandboxpolicy.SnapshotVersion, ResolutionGroupID: temp.ID}))
	_ = squad
	h.grant(t, agentd.PermGroupsDelete, "group=temp")
	code, body = h.call("DELETE", "/api/groups/temp", nil)
	require.Equal(t, 403, code, "an agent keeping a setting on temp takes squad's next: %s", body)
	require.Contains(t, body, "group:squad", "squad is visible under the unscoped spawn grant")
	h.grant(t, agentd.PermGroupsMembersSpawnEnvironment, "group=squad")
	code, body = h.call("DELETE", "/api/groups/temp", nil)
	require.Less(t, code, 300, body)
}
