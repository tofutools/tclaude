package agentd_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// A peer deletes an agent under groups.members.delete in every group the
// agent is in or owns. Deleting its worktree with it destroys uncommitted
// work on this machine, past undoing: that takes unrestricted trust, even for
// a peer holding the delete grant everywhere.
func TestPeerAgentDeleteHoldsItsGroupsAndNeverTheWorktree(t *testing.T) {
	h := newPeerProcessHarness(t)
	other := h.f.HaveGroup("other")
	agentID := havePerformerAgent(t, h.f, "crew", "del-conv", "del", "dev", "offline")
	require.NoError(t, db.AddAgentGroupOwner(other.ID, "del-conv", "human"))
	del := func(query string) (int, string) { return h.call("DELETE", "/api/agents/"+agentID+query, nil) }

	code, body := del("")
	require.Equal(t, 404, code, "crew is not visible: %s", body)
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	code, body = del("")
	require.Equal(t, 403, code, body)
	h.grant(t, agentd.PermGroupsMembersDelete, "group=crew")
	code, body = del("")
	require.Equal(t, 403, code, "a delete purges its seat in the group it owns: %s", body)
	h.grant(t, agentd.PermGroupsMembersDelete, "group=other")
	// An archived group's membership goes with the delete too, and the
	// group may come back: the grant must name it.
	// (A grant names a group while it is active, and outlives its archiving.)
	h.f.HaveGroup("attic")
	h.f.HaveMemberWithRole("attic", "del-conv", "dev")
	require.NoError(t, db.ArchiveAgentGroup("attic"))
	code, body = del("")
	require.Equal(t, 403, code, "an archived group of the agent: %s", body)
	require.NoError(t, db.UnarchiveAgentGroup("attic"))
	h.grant(t, agentd.PermGroupsMembersDelete, "group=attic")
	require.NoError(t, db.ArchiveAgentGroup("attic"))

	for _, query := range []string{"?delete_worktree=1", "?delete_worktree=true&expected_worktree=/w", "?expected_worktree=/w"} {
		code, body = del(query)
		require.Equal(t, 403, code, "%s: %s", query, body)
		require.Contains(t, body, "unrestricted trust")
	}
	code, body = del("?force=1")
	require.Equal(t, 400, code, body)
	a, err := db.GetAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, a, "nothing was deleted yet")

	code, body = del("")
	require.Less(t, code, 300, body)
	a, err = db.GetAgent(agentID)
	require.NoError(t, err)
	require.Nil(t, a)

	// The delete dialog's worktree probe stays unrestricted trust's: the
	// dialog then offers no worktree to delete.
	code, body = h.call("GET", "/api/agents/"+h.member+"/worktree", nil)
	require.Equal(t, 403, code, body)
}

// A restricted peer's debug export carries no environment value, wherever
// the export records one; unrestricted trust gets the export as it is.
func TestPeerAgentDebugExportRedactsEnvironmentValues(t *testing.T) {
	h := newPeerProcessHarness(t)
	agentID := havePerformerAgent(t, h.f, "crew", "dbg-conv", "dbg", "dev", "offline")
	require.NoError(t, db.SetAgentInitialSpawnConfig(agentID, `{"profile":"","environment":[{"name":"SPAWN_TOKEN","value":"s3cret-spawn"}]}`))
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &sandboxpolicy.Snapshot{
		Version:                    sandboxpolicy.SnapshotVersion,
		LaunchEnvironment:          []sandboxpolicy.EnvironmentEntry{{Name: "GROUP_VAR", Value: "s3cret-group"}},
		LaunchEnvironmentOverrides: []sandboxpolicy.EnvironmentEntry{{Name: "OVERRIDE_VAR", Value: "s3cret-override"}},
	}))
	export := func() (int, string) { return h.call("GET", "/api/agents/"+agentID+"/debug-export", nil) }

	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	code, body := export()
	require.Equal(t, 403, code, body)
	h.grant(t, agentd.PermGroupsMembersDebugExport, "group=crew")
	code, body = export()
	require.Equal(t, 200, code, body)
	require.NotContains(t, body, "s3cret")
	for _, name := range []string{"SPAWN_TOKEN", "GROUP_VAR", "OVERRIDE_VAR"} {
		require.Contains(t, body, name, "the names stay")
	}
	var doc struct {
		Redactions []string `json:"redactions"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &doc))
	require.Contains(t, doc.Redactions, "configurations.requested.parameters.environment")
	require.Contains(t, doc.Redactions, "configurations.resolved.effective_sandbox.launch_environment")

	h.trustUnrestricted(t)
	code, body = export()
	require.Equal(t, 200, code, body)
	require.Contains(t, body, "s3cret-group")
}

// Remote access is reached under groups.members.remote-control in every
// group of the agent (the gate; a status read stands in for a switch).
func TestPeerAgentRemoteControlHoldsItsGroups(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.f.HaveGroup("other")
	h.f.HaveMemberWithRole("other", "proc-crew-member", "dev")
	rc := func() (int, string) {
		return h.call("POST", "/api/agents/"+h.member+"/remote-control", map[string]any{"intent": "status"})
	}
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	h.grant(t, agentd.PermGroupsMembersRemoteControl, "group=crew")
	code, body := rc()
	require.Equal(t, 403, code, "the agent is in other too: %s", body)
	h.grant(t, agentd.PermGroupsMembersRemoteControl, "group=other")
	code, body = rc()
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"action":"status"`)
}
