package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestRoleProfileResolutionUsesIDLoadedBeforeRename(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	profileID, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "before"})
	require.NoError(t, err)
	_, err = db.CreateRole(&db.Role{Name: "stable-role-race", SpawnProfile: "before"})
	require.NoError(t, err)
	loadedBeforeRename, err := db.GetRole("stable-role-race")
	require.NoError(t, err)
	require.Equal(t, profileID, loadedBeforeRename.SpawnProfileID)

	require.NoError(t, db.UpdateSpawnProfile(&db.SpawnProfile{ID: profileID, Name: "after"}))
	_, fail := resolveTemplateAgentLaunch(nil, db.GroupTemplateAgent{}, loadedBeforeRename, home, "")
	require.Nil(t, fail, "the pre-rename role object must resolve its profile through the stable id")
}

func TestOperatorOnlyTemplateAndRoleProfilesRejectAgentCaller(t *testing.T) {
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

	role := &db.Role{Name: "trusted-role", SpawnProfile: "operator", SpawnProfileID: profileID}
	_, fail = resolveTemplateAgentLaunch(nil, db.GroupTemplateAgent{}, role, home, "agent-caller")
	require.NotNil(t, fail)
	assert.Equal(t, "profile_operator_only", fail.Kind)
	assert.Contains(t, fail.Msg, `role "trusted-role"`)
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
