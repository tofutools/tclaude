package agentd

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// A run start's footprint and slug follow the scope its body names.
func TestAgentPeerViewRunScope(t *testing.T) {
	route := agentPeerViewRoutes["POST /v1/process/runs"]
	require.True(t, route.runScope)

	got, values, err := agentPeerViewRunScope(route, map[string]string{}, []byte(`{"templateId":"x","group":"crew"}`))
	require.NoError(t, err)
	require.Equal(t, agentViewGroup, got.target)
	require.Equal(t, []string{PermProcessRunsStart}, got.slugs)
	require.Equal(t, "crew", values["name"])

	got, values, err = agentPeerViewRunScope(route, map[string]string{}, []byte(`{"templateId":"x","agent":"agt_1"}`))
	require.NoError(t, err)
	require.Equal(t, agentViewAgent, got.target)
	require.Equal(t, []string{PermAgentProcess}, got.slugs)
	require.Equal(t, "agt_1", values["id"])

	for _, body := range []string{`{"templateId":"x"}`, `{"group":"a","agent":"agt_1"}`, `not json`, ``} {
		_, _, err := agentPeerViewRunScope(route, map[string]string{}, []byte(body))
		require.Error(t, err, body)
	}

	other := agentPeerViewRoutes["GET /api/fleet"]
	same, _, err := agentPeerViewRunScope(other, nil, []byte(`{"group":"crew"}`))
	require.NoError(t, err)
	require.Equal(t, other.target, same.target, "only a run start reads its scope from the body")
}

func TestProcessPeerOwner(t *testing.T) {
	id, err := proto.NewIdentity()
	require.NoError(t, err)
	inst := id.ID()
	require.Equal(t, "peer:"+inst, processPeerOwner(inst, nil))
	agent := processPeerOwner(inst, &proto.PeerViewAgent{ID: "agt_x"})
	require.Equal(t, "peer:"+inst+"/agent:agt_x", agent)
	for _, owner := range []string{"peer:" + inst, agent} {
		require.True(t, processOwnerIsPeer(owner))
		require.False(t, processOwnerMayTask(owner, processPerformerCandidate{AgentID: "agt_x"}), "a peer owner tasks no one outside its scope")
	}
	require.False(t, processOwnerIsPeer(processOwnerOperator))
	require.False(t, processOwnerIsPeer("agent:agt_x"))
	require.Equal(t, "the operator of a linked node", processPeerOwnerLabel("peer:"+inst), "an unknown peer is not named")
	require.Equal(t, "a linked node", processPeerOwnerLabel("peer:not an instance]"))
	require.Equal(t, "agent agt_x of a linked node", processPeerOwnerLabel(agent))
}
