package agentd_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The Groups toolbar on a peer page: the global spawn and sandbox defaults
// and "+ new group" reach this node under their peer grants, and unrestricted
// trust holds them all.
func TestPeerViewManagesNodeDefaultsAndCreatesGroups(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("one")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "cheap", Harness: "claude", Model: "haiku"})
	require.NoError(t, err)
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{Name: "locked"})
	require.NoError(t, err)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	do := func(method, path string, body any) int {
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
		return rec.Code
	}
	snapshot := func() map[string]any {
		rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/snapshot", nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}
	grant := func(slug string) {
		rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}

	require.Equal(t, 403, do("PUT", "/api/spawn-profile-default", map[string]any{"name": "cheap"}))
	require.Equal(t, 403, do("PUT", "/api/sandbox-profile-default", map[string]any{"name": "locked"}))
	require.Equal(t, 403, do("POST", "/api/groups", map[string]any{"name": "fresh"}))
	rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermNodeGroupsCreate, "scope": "group=one"})
	require.Equal(t, 400, rec.Code, "node.groups.create is node-wide")

	rec = fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsRosterRead, "scope": "group=one"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Empty(t, snapshot()["spawn_profile_default"], "the defaults travel only with a spawn grant")
	grant(agentd.PermNodeSpawnDefaultsManage)
	require.Equal(t, 200, do("PUT", "/api/spawn-profile-default", map[string]any{"name": "cheap"}))
	require.Equal(t, 200, do("PUT", "/api/sandbox-profile-default", map[string]any{"name": "locked"}))
	snap := snapshot()
	require.Equal(t, "cheap", snap["spawn_profile_default"])
	require.Equal(t, "locked", snap["sandbox_profile_default"])
	require.Contains(t, snap["peer_view"].(map[string]any)["included"], "spawn.defaults")
	require.NotEqual(t, 200, do("PUT", "/api/spawn-profile-default", map[string]any{"name": "missing"}), "only an existing profile")
	require.Equal(t, 200, do("DELETE", "/api/sandbox-profile-default", nil))
	require.Empty(t, snapshot()["sandbox_profile_default"])
	require.Equal(t, 403, do("POST", "/api/spawn-profiles", map[string]any{"name": "x"}), "profiles are still created on their own node")

	grant(agentd.PermNodeGroupsCreate)
	f.HaveGroup("hidden")
	require.Equal(t, 403, do("POST", "/api/groups", map[string]any{"name": "fresh", "default_cwd": "/etc"}), "a restricted peer sets no launch settings")
	require.Equal(t, 403, do("POST", "/api/groups", map[string]any{"name": "fresh", "environment": []any{map[string]any{"name": "A", "value": "b"}}}))
	require.Equal(t, 404, do("POST", "/api/groups", map[string]any{"name": "fresh", "parent": "hidden"}), "nests only under a visible group")
	require.Equal(t, 201, do("POST", "/api/groups", map[string]any{"name": "fresh", "parent": "one", "descr": "made remotely", "default_cwd": "", "environment": []any{}, "max_members": 0}))
	g, err := db.GetAgentGroupByName("fresh")
	require.NoError(t, err)
	require.NotNil(t, g)
	require.Equal(t, "made remotely", g.Descr)
	require.Equal(t, 403, do("POST", "/api/groups/one/clone", map[string]any{"new_name": "copy"}), "clones take unrestricted trust")

	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	require.Equal(t, 201, do("POST", "/api/groups", map[string]any{"name": "rooted", "default_cwd": t.TempDir()}))
	require.Equal(t, 200, do("POST", "/api/groups/one/clone", map[string]any{"new_name": "copy", "no_clone_members": true}))
	require.Contains(t, snapshot()["peer_view"].(map[string]any)["included"], "groups.create.full")
}
