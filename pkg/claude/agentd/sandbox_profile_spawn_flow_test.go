package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestSandboxProfileSpawnFreezesValuesAndExplicitSelectionIsHumanOnly(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	profileID, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "literal-env",
		Environment: []db.SandboxEnvironmentEntry{{
			Name: "LITERAL", Value: "spaces '$HOME' $(touch nope); `echo nope`\nnext",
		}},
	})
	require.NoError(t, err)

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "sandbox_profile": "literal-env",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	snapshot, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	require.Len(t, snapshot.Applied, 1)
	assert.Equal(t, profileID, snapshot.Applied[0].ID)
	require.Len(t, snapshot.Effective.Environment, 1)
	assert.Equal(t, "LITERAL", snapshot.Effective.Environment[0].Name)
	assert.Contains(t, snapshot.Effective.Environment[0].Value, "$(touch nope)")

	persisted, err := db.AgentEffectiveSandboxConfigForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	assert.Equal(t, snapshot.Effective.Environment, persisted.Effective.Environment)

	require.NoError(t, db.GrantAgentPermission(spawn.ConvID, agentd.PermGroupsSpawn, "test"))
	denied := f.AsAgent(spawn.ConvID).SpawnWith("crew", map[string]any{
		"name": "child", "sandbox_profile": "literal-env",
	})
	require.Equal(t, http.StatusForbidden, denied.Code)
	assert.Contains(t, string(denied.Raw), "sandbox_profile_restricted")
}
