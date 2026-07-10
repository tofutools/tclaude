package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV108toV109BackfillsStableRegistryReferences(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	for table, cols := range map[string][]string{
		"agent_groups":          {"default_profile_id", "source_template_id"},
		"group_template_agents": {"spawn_profile_id"},
		"roles":                 {"spawn_profile_id"},
	} {
		for _, col := range cols {
			var have int
			require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&have))
			assert.Equal(t, 1, have, "%s.%s", table, col)
		}
	}

	profileID, err := CreateSpawnProfile(&SpawnProfile{Name: "legacy-profile"})
	require.NoError(t, err)
	templateID, err := CreateGroupTemplate(&GroupTemplate{Name: "legacy-template", Agents: []GroupTemplateAgent{{Name: "worker", SpawnProfile: "legacy-profile"}}})
	require.NoError(t, err)
	groupID, err := CreateAgentGroup("legacy-group", "")
	require.NoError(t, err)
	_, err = SetAgentGroupDefaultProfile("legacy-group", "legacy-profile")
	require.NoError(t, err)
	_, err = SetAgentGroupDeployMeta("legacy-group", "", "legacy-template")
	require.NoError(t, err)
	_, err = CreateRole(&Role{Name: "legacy-ref-role", SpawnProfile: "legacy-profile"})
	require.NoError(t, err)
	require.NoError(t, SetDashboardPref("tclaude.dash.default_profile", "legacy-profile"))
	require.NoError(t, DeleteDashboardPref("tclaude.dash.default_profile_id"))
	require.NoError(t, UpsertWaveChoreography(&WaveChoreography{
		GroupID: groupID, GroupName: "legacy-group", TemplateName: "legacy-template",
		Waves: []WaveGroup{{Wave: 0, Agents: []GroupTemplateAgent{{Name: "worker", SpawnProfile: "legacy-profile"}}}},
	}))
	mustExec(t, d, `UPDATE agent_groups SET default_profile_id = NULL, source_template_id = NULL WHERE id = ?`, groupID)
	mustExec(t, d, `UPDATE group_template_agents SET spawn_profile_id = NULL WHERE template_id = ?`, templateID)
	mustExec(t, d, `UPDATE roles SET spawn_profile_id = NULL WHERE name = 'legacy-ref-role'`)

	// Re-running a half-applied migration is intentionally safe and backfills
	// every legacy name reference.
	mustExec(t, d, `UPDATE schema_version SET version = 108`)
	require.NoError(t, migrateV108toV109(d))
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 109, version)

	var groupProfileID, groupTemplateID, agentProfileID, roleProfileID int64
	require.NoError(t, d.QueryRow(`SELECT default_profile_id, source_template_id FROM agent_groups WHERE id = ?`, groupID).Scan(&groupProfileID, &groupTemplateID))
	require.NoError(t, d.QueryRow(`SELECT spawn_profile_id FROM group_template_agents WHERE template_id = ?`, templateID).Scan(&agentProfileID))
	require.NoError(t, d.QueryRow(`SELECT spawn_profile_id FROM roles WHERE name = 'legacy-ref-role'`).Scan(&roleProfileID))
	assert.Equal(t, profileID, groupProfileID)
	assert.Equal(t, templateID, groupTemplateID)
	assert.Equal(t, profileID, agentProfileID)
	assert.Equal(t, profileID, roleProfileID)
	globalID, ok, err := GetDashboardPref("tclaude.dash.default_profile_id")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, fmt.Sprint(profileID), globalID)
	choreo, err := GetWaveChoreography(groupID)
	require.NoError(t, err)
	require.NotNil(t, choreo)
	assert.Equal(t, templateID, choreo.TemplateID)
	require.Len(t, choreo.Waves, 1)
	require.Len(t, choreo.Waves[0].Agents, 1)
	assert.Equal(t, profileID, choreo.Waves[0].Agents[0].SpawnProfileID)
}
