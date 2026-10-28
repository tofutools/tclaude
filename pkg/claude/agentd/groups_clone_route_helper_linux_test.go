//go:build linux

package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
