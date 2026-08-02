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
