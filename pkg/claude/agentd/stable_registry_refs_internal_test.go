package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestOperatorOnlyTemplateProfilesRejectAgentCaller(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	profileID, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "operator", OperatorOnly: true,
	})
	require.NoError(t, err)

	agentSpec := db.GroupTemplateAgent{
		Name: "worker", SpawnProfile: "operator", SpawnProfileID: profileID,
	}
	_, fail := resolveTemplateAgentLaunch(nil, agentSpec, nil, home, "agent-caller")
	require.NotNil(t, fail)
	assert.Equal(t, "profile_operator_only", fail.Kind)
	_, fail = resolveTemplateAgentLaunch(nil, agentSpec, nil, home, "")
	require.Nil(t, fail, "the human trust root may use an operator-only profile")

}

func TestOperatorOnlyWaveResultPreservesTypedFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	profileID, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "operator", OperatorOnly: true,
	})
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)
	g, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)

	result := spawnWaveAgents(g, []db.GroupTemplateAgent{{
		Name: "worker", SpawnProfile: "operator", SpawnProfileID: profileID,
	}}, nil, "", home, "", "", nil,
		"agent-caller", "", "", nil, false, "", nil, "", nil)
	require.Len(t, result.Results, 1)
	assert.Equal(t, "profile_operator_only", result.Results[0].ErrorKind)
	assert.Contains(t, result.Results[0].Error, "restricted to human/operator spawns")
	assert.Equal(t, 1, result.Failed)
	assert.Zero(t, result.Spawned)
}
