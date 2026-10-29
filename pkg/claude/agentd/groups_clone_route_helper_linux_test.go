//go:build linux

package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestGroupsClone_RouteHelperContractCoversNoCopyAndCopy(t *testing.T) {
	t.Run("no-copy", func(t *testing.T) {
		f := newFlow(t)
		const source = "route-no-copy-source-111111111111"
		makeRouteEligibleCloneSource(t, f, source, "route-no-copy", "spwn-route-no-copy")
		response := groupCloneRequest(t, f, "route-no-copy", nil)
		require.Len(t, response.Members, 1)
		require.Empty(t, response.Members[0].Error)
		clonedGroup, err := db.GetAgentGroupByName(response.Group)
		require.NoError(t, err)
		require.NotNil(t, clonedGroup)
		helper, ok := f.World.SpawnRouteHelper(response.Members[0].NewConv)
		require.True(t, ok)
		assert.Equal(t, []int64{clonedGroup.ID}, helper.GroupIDs,
			"the helper must receive the cloned destination group, not the source group")
		assert.Equal(t, response.Members[0].NewConv, helper.ConvID)
		assert.NotEmpty(t, helper.AgentID)
		assert.NotEmpty(t, helper.LaunchGeneration)
		assert.NotEmpty(t, helper.Credential)
	})

	t.Run("copy", func(t *testing.T) {
		f := newFlow(t)
		const source = "e1e1e1e1-aaaa-bbbb-cccc-111111111111"
		makeRouteEligibleCloneSource(t, f, source, "route-copy", "spwn-route-copy")
		r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
			"/v1/groups/route-copy/clone", map[string]any{}))
		rec := testharness.Serve(f.Mux, r)
		require.Equal(t, http.StatusOK, rec.Code, "group copy clone: %s", rec.Body.String())
		var response cloneGroupResp
		testharness.DecodeJSON(t, rec, &response)
		require.Len(t, response.Members, 1)
		require.Empty(t, response.Members[0].Error)
		clonedGroup, err := db.GetAgentGroupByName(response.Group)
		require.NoError(t, err)
		require.NotNil(t, clonedGroup)
		helper, ok := f.World.SpawnRouteHelper(response.Members[0].NewConv)
		require.True(t, ok)
		assert.Equal(t, []int64{clonedGroup.ID}, helper.GroupIDs)
		assert.Equal(t, response.Members[0].NewConv, helper.ConvID)
		assert.NotEmpty(t, helper.Credential)
	})
}

func TestHostOnlyCloneInstallsPublisherDiscovery(t *testing.T) {
	for _, groupClone := range []bool{false, true} {
		t.Run(map[bool]string{false: "agent", true: "group"}[groupClone], func(t *testing.T) {
			f := newFlow(t)
			const source = "host-only-clone-source"
			group := makeRouteEligibleCloneSource(t, f, source, "host-only-clone", "spwn-host-only-clone")
			require.NoError(t, db.ReplaceAgentGroupPermissions(group.ID, nil, "test"))
			require.NoError(t, db.GrantAgentPermission(source, agentd.PermRoutesPublishHost, "test"))
			var newConv string
			if groupClone {
				result := groupCloneRequest(t, f, group.Name, nil)
				require.Len(t, result.Members, 1)
				require.Empty(t, result.Members[0].Error)
				newConv = result.Members[0].NewConv
			} else {
				newConv = f.AsHuman().CloneFresh(source).NewConv
			}
			helper, ok := f.World.SpawnRouteHelper(newConv)
			require.True(t, ok, "host-only clone requires a publisher helper")
			require.Equal(t, []int64{0}, helper.GroupIDs)
			require.NotEmpty(t, helper.Credential)
		})
	}
}

func TestHostOnlySpawnDoesNotEnableGroupConsumption(t *testing.T) {
	f := newFlow(t)
	group := f.HaveGroup("host-only-spawn")
	require.NoError(t, db.ReplaceAgentGroupPermissions(group.ID, []string{agentd.PermRoutesPublishHost}, "test"))
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "host-only-private", NetworkAccess: sandboxpolicy.NetworkAccessNone})
	require.NoError(t, err)
	spawn := f.AsHuman().SpawnWith(group.Name, map[string]any{
		"name": "host-only-worker", "harness": "claude", "sandbox_implementation": "tclaude-layer", "sandbox_profile": "host-only-private",
	})
	require.Equal(t, http.StatusOK, spawn.Code, spawn.Raw)

	helper, ok := f.World.SpawnRouteHelper(spawn.ConvID)
	require.True(t, ok, "host-only spawn should install its publisher helper")
	require.Equal(t, []int64{0}, helper.GroupIDs, "plain group membership must not install group consumer authority")
}

// Codex's conversation is created by the harness after the helper launch args
// have been frozen. Both route surfaces must work on that first launch.
func TestCodexSpawnLateRouteHelper(t *testing.T) {
	for _, mode := range []string{"inline", "pending", "sweeper"} {
		t.Run(mode, func(t *testing.T) { testCodexSpawnLateRouteHelper(t, mode != "inline", mode == "sweeper") })
	}
}

func testCodexSpawnLateRouteHelper(t *testing.T, pending, sweep bool) {
	f := newFlow(t)
	var delayed *delayedCodexSpawner
	if pending {
		t.Cleanup(agentd.SetCodexAsyncSpawnResponseGraceForTest(20 * time.Millisecond))
		if sweep {
			t.Cleanup(agentd.SetAsyncSpawnInlineGraceForTest(20 * time.Millisecond))
		} else {
			t.Cleanup(agentd.SetAsyncSpawnInlineGraceForTest(30 * time.Second))
		}
		delayed = &delayedCodexSpawner{t: t, w: f.World, inner: f.World.DefaultMocks(t).Spawner, release: make(chan struct{}), conv: map[string]string{}}
		prev := agentd.Spawn
		agentd.Spawn = delayed
		t.Cleanup(func() { agentd.Spawn = prev })
	}

	group := f.HaveGroup("late-routes")
	require.NoError(t, db.ReplaceAgentGroupPermissions(group.ID, []string{agentd.PermRoutesPublishHost, agentd.PermRoutesPublish, agentd.PermRoutesConsume}, "test"))
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "late-private", NetworkAccess: sandboxpolicy.NetworkAccessNone})
	require.NoError(t, err)
	spawn := f.AsHuman().SpawnWith(group.Name, map[string]any{
		"name": "late-worker", "harness": "codex", "sandbox_implementation": "tclaude-layer", "sandbox_profile": "late-private",
	})
	require.Equal(t, http.StatusOK, spawn.Code, spawn.Raw)
	if pending {
		require.Empty(t, spawn.ConvID)
		close(delayed.release)
		require.Eventually(t, func() bool {
			spawn.ConvID = delayed.convID(spawn.Label)
			if spawn.ConvID == "" {
				return false
			}
			if sweep {
				agentd.RunPendingSpawnSweepForTest()
			}
			member, err := db.FindMemberInGroup(group.ID, spawn.ConvID)
			return err == nil && member != nil
		}, 10*time.Second, 20*time.Millisecond)
		agentd.WaitForBackgroundForTest()
	}
	helper, ok := f.World.SpawnRouteHelper(spawn.ConvID)
	require.True(t, ok)
	require.Empty(t, helper.ConvID, "conversation must be attached after launch")
	require.NotEmpty(t, helper.Credential)
	require.Equal(t, []int64{group.ID, 0}, helper.GroupIDs)
	for _, host := range []bool{true, false} {
		body := map[string]any{"name": "web", "target": "tcp://127.0.0.1:43127", "launch_generation": helper.LaunchGeneration}
		if host {
			body["host"] = true
		} else {
			body["group"] = group.Name
		}
		rec, route := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", spawn.ConvID, body)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		if !host {
			rec, _ = serveRouteAgent(t, f, http.MethodPost, "/v1/routes/open", spawn.ConvID, map[string]any{
				"route_id": route["id"], "group": group.Name, "launch_generation": helper.LaunchGeneration,
			})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		}
	}
	req := testharness.JSONRequest(t, http.MethodGet, "/v1/routes?host=true", nil)
	req.Header.Set("X-Tclaude-Route-Helper-Credential", helper.Credential)
	rec := testharness.Serve(f.Mux, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), spawn.ConvID, "discovery supplies the late-bound conversation")
}
