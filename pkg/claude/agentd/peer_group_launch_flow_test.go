package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A restricted peer deploys a team from this node's template only when it
// could spawn every agent of it itself: the whole call is refused otherwise,
// naming the first grant it lacks, whoever wrote the template.
func TestPeerTemplateDeployStaysWithinItsSpawnGrants(t *testing.T) {
	h := newPeerProcessHarness(t)
	for _, tmpl := range []map[string]any{
		{"name": "squad", "agents": []any{map[string]any{"name": "dev"}}},
		{"name": "loaded", "agents": []any{map[string]any{"name": "lead", "model": "opus"}}},
		{"name": "crowned", "agents": []any{map[string]any{"name": "boss", "is_owner": true}}},
	} {
		rec := fedHuman(t, h.f, http.MethodPost, "/v1/templates", tmpl)
		require.Less(t, rec.Code, 300, rec.Body.String())
	}
	deploy := func(name string, body map[string]any) (int, string) {
		return h.call("POST", "/api/templates/"+name+"/instantiate", body)
	}

	code, body := deploy("squad", map[string]any{"group_name": "team"})
	require.Equal(t, 403, code, "no grant: %s", body)
	h.grant(t, agentd.PermTemplatesUse, "")
	code, body = deploy("squad", map[string]any{"group_name": "team"})
	require.Equal(t, 403, code, "a new group takes node.groups.create: %s", body)
	h.grant(t, agentd.PermNodeGroupsCreate, "")
	code, body = deploy("squad", map[string]any{"group_name": "team"})
	require.Equal(t, 403, code, "its agents spawn: %s", body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawn)
	h.grant(t, agentd.PermGroupsMembersSpawn, "")

	code, body = deploy("loaded", map[string]any{"group_name": "team"})
	require.Equal(t, 403, code, "an operator-written agent it could not spawn: %s", body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawnModel)
	code, body = deploy("crowned", map[string]any{"group_name": "team"})
	require.Equal(t, 403, code, "owner seats: %s", body)
	require.Contains(t, body, agentd.PermGroupsOwnersManage)
	code, body = deploy("squad", map[string]any{"group_name": "team", "cwd": t.TempDir()})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawnCwd)
	code, body = deploy("squad", map[string]any{"group_name": "team", "parent": "hidden-from-peer"})
	require.Equal(t, 404, code, body)
	code, body = deploy("squad", map[string]any{"group_name": "team", "spawn_everything": true})
	require.Equal(t, 403, code, "an unknown key: %s", body)

	code, body = deploy("squad", map[string]any{"group_name": "team", "task": "build the thing"})
	require.Less(t, code, 300, body)
	g, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	require.NotNil(t, g, "the team's group exists")
}

// A clone takes groups.clone in the source group and node.groups.create;
// its members take groups.members.clone, and its owners stay this node's.
func TestPeerGroupClone(t *testing.T) {
	h := newPeerProcessHarness(t)
	clone := func(body map[string]any) (int, string) { return h.call("POST", "/api/groups/crew/clone", body) }

	code, body := clone(map[string]any{"new_name": "crew2", "no_clone_members": true})
	require.Equal(t, 404, code, "crew is not visible: %s", body)
	h.grant(t, agentd.PermGroupsClone, "group=crew")
	code, body = clone(map[string]any{"new_name": "crew2", "no_clone_members": true})
	require.Equal(t, 403, code, "a new group takes node.groups.create: %s", body)
	h.grant(t, agentd.PermNodeGroupsCreate, "")
	code, body = clone(map[string]any{"new_name": "crew2"})
	require.Equal(t, 403, code, "its members: %s", body)
	require.Contains(t, body, agentd.PermGroupsMembersClone)
	code, body = clone(map[string]any{"new_name": "crew2", "no_clone_members": true, "default_cwd": t.TempDir()})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawnCwd)

	code, body = clone(map[string]any{"new_name": "crew2", "no_clone_members": true})
	require.Equal(t, 200, code, body)
	g, err := db.GetAgentGroupByName("crew2")
	require.NoError(t, err)
	require.NotNil(t, g)
}

// A group's launch settings at creation take the peer's spawn grants for
// every group.
func TestPeerGroupCreateWithLaunchSettings(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.grant(t, agentd.PermNodeGroupsCreate, "")
	dir := t.TempDir()
	code, body := h.call("POST", "/api/groups", map[string]any{"name": "rooted", "default_cwd": dir})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawnCwd)
	h.grant(t, agentd.PermGroupsMembersSpawn, "")
	h.grant(t, agentd.PermGroupsMembersSpawnCwd, "")
	code, body = h.call("POST", "/api/groups", map[string]any{"name": "rooted", "default_cwd": dir})
	require.Equal(t, 201, code, body)
	code, body = h.call("POST", "/api/groups", map[string]any{"name": "lit", "environment": []any{map[string]any{"name": "A", "value": "b"}}})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawnEnvironment)
}
