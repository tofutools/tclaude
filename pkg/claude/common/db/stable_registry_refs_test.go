package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpawnProfileReferencesSurviveRename(t *testing.T) {
	setupTestDB(t)

	profileID, err := CreateSpawnProfile(&SpawnProfile{Name: "original", Model: "sonnet"})
	require.NoError(t, err)
	_, err = CreateAgentGroup("crew", "")
	require.NoError(t, err)
	_, err = SetAgentGroupDefaultProfile("crew", "original")
	require.NoError(t, err)
	_, err = CreateRole(&Role{Name: "stable-ref-role", SpawnProfile: "original"})
	require.NoError(t, err)
	_, err = CreateGroupTemplate(&GroupTemplate{Name: "circle", Agents: []GroupTemplateAgent{{Name: "lead", SpawnProfile: "original"}}})
	require.NoError(t, err)
	require.NoError(t, SetDashboardProfileRef("tclaude.dash.default_profile", "tclaude.dash.default_profile_id", "original", profileID))

	require.NoError(t, UpdateSpawnProfile(&SpawnProfile{ID: profileID, Name: "renamed", Model: "sonnet"}))

	g, err := GetAgentGroupByName("crew")
	require.NoError(t, err)
	assert.Equal(t, "renamed", g.DefaultProfile)
	role, err := GetRole("stable-ref-role")
	require.NoError(t, err)
	assert.Equal(t, "renamed", role.SpawnProfile)
	tmpl, err := GetGroupTemplate("circle")
	require.NoError(t, err)
	require.Len(t, tmpl.Agents, 1)
	assert.Equal(t, "renamed", tmpl.Agents[0].SpawnProfile)
	assert.Equal(t, profileID, tmpl.Agents[0].SpawnProfileID)
	name, _, err := GetDashboardPref("tclaude.dash.default_profile")
	require.NoError(t, err)
	assert.Equal(t, "renamed", name)

	d, err := Open()
	require.NoError(t, err)
	for _, q := range []string{
		`SELECT default_profile_id FROM agent_groups WHERE name = 'crew'`,
		`SELECT spawn_profile_id FROM roles WHERE name = 'stable-ref-role'`,
		`SELECT spawn_profile_id FROM group_template_agents WHERE name = 'lead'`,
	} {
		var got int64
		require.NoError(t, d.QueryRow(q).Scan(&got))
		assert.Equal(t, profileID, got)
	}
}

func TestTemplateReferenceSurvivesRename(t *testing.T) {
	setupTestDB(t)

	templateID, err := CreateGroupTemplate(&GroupTemplate{Name: "original"})
	require.NoError(t, err)
	groupID, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	_, err = SetAgentGroupDeployMeta("crew", "ship", "original")
	require.NoError(t, err)
	require.NoError(t, UpsertWaveChoreography(&WaveChoreography{
		GroupID: groupID, GroupName: "crew", TemplateName: "original", TemplateID: templateID,
		Waves: []WaveGroup{},
	}))

	tmpl, err := GetGroupTemplate("original")
	require.NoError(t, err)
	tmpl.Name = "renamed"
	require.NoError(t, UpdateGroupTemplate(tmpl))

	g, err := GetAgentGroupByName("crew")
	require.NoError(t, err)
	assert.Equal(t, "renamed", g.SourceTemplate)
	byID, err := GetGroupTemplateByID(templateID)
	require.NoError(t, err)
	require.NotNil(t, byID)
	assert.Equal(t, "renamed", byID.Name)
	choreo, err := GetWaveChoreography(groupID)
	require.NoError(t, err)
	require.NotNil(t, choreo)
	assert.Equal(t, "renamed", choreo.TemplateName)
}

func TestDeletedRegistryReferenceCannotBeHijackedByNameReuse(t *testing.T) {
	setupTestDB(t)

	profileID, err := CreateSpawnProfile(&SpawnProfile{Name: "shared"})
	require.NoError(t, err)
	require.NoError(t, SetDashboardProfileRef("tclaude.dash.default_profile", "tclaude.dash.default_profile_id", "shared", profileID))
	_, err = CreateAgentGroup("crew", "")
	require.NoError(t, err)
	_, err = SetAgentGroupDefaultProfile("crew", "shared")
	require.NoError(t, err)
	_, err = DeleteSpawnProfile("shared")
	require.NoError(t, err)
	_, err = CreateSpawnProfile(&SpawnProfile{Name: "shared"})
	require.NoError(t, err)

	g, err := GetAgentGroupByName("crew")
	require.NoError(t, err)
	assert.Empty(t, g.DefaultProfile)
	_, nameExists, err := GetDashboardPref("tclaude.dash.default_profile")
	require.NoError(t, err)
	_, idExists, err := GetDashboardPref("tclaude.dash.default_profile_id")
	require.NoError(t, err)
	assert.False(t, nameExists)
	assert.False(t, idExists)
}

func TestStableRegistryRefs_ReconcileLegacyNameOnlyWrites(t *testing.T) {
	setupTestDB(t)

	p1, err := CreateSpawnProfile(&SpawnProfile{Name: "profile-one"})
	require.NoError(t, err)
	p2, err := CreateSpawnProfile(&SpawnProfile{Name: "profile-two"})
	require.NoError(t, err)
	t1, err := CreateGroupTemplate(&GroupTemplate{Name: "template-one", Agents: []GroupTemplateAgent{{Name: "worker", SpawnProfile: "profile-one"}}})
	require.NoError(t, err)
	t2, err := CreateGroupTemplate(&GroupTemplate{Name: "template-two"})
	require.NoError(t, err)
	_, err = CreateRole(&Role{Name: "legacy-write-role", SpawnProfile: "profile-one"})
	require.NoError(t, err)
	gID, err := CreateAgentGroup("legacy-write-group", "")
	require.NoError(t, err)
	_, err = SetAgentGroupDefaultProfile("legacy-write-group", "profile-one")
	require.NoError(t, err)
	_, err = SetAgentGroupDeployMeta("legacy-write-group", "", "template-one")
	require.NoError(t, err)
	require.NoError(t, SetDashboardProfileRef(
		"tclaude.dash.default_profile", "tclaude.dash.default_profile_id", "profile-one", p1))

	d, err := Open()
	require.NoError(t, err)
	// These statements intentionally mimic a v108 binary: only the legacy name
	// columns/preferences are known and written.
	mustExec(t, d, `UPDATE agent_groups SET default_profile = 'profile-two', source_template = 'template-two' WHERE id = ?`, gID)
	mustExec(t, d, `UPDATE roles SET spawn_profile = 'profile-two' WHERE name = 'legacy-write-role'`)
	mustExec(t, d, `UPDATE group_template_agents SET spawn_profile = 'profile-two' WHERE template_id = ?`, t1)
	mustExec(t, d, `UPDATE dashboard_prefs SET value = 'profile-two' WHERE key = 'tclaude.dash.default_profile'`)

	var groupProfileID, groupTemplateID, roleProfileID, agentProfileID int64
	require.NoError(t, d.QueryRow(`SELECT default_profile_id, source_template_id FROM agent_groups WHERE id = ?`, gID).Scan(&groupProfileID, &groupTemplateID))
	require.NoError(t, d.QueryRow(`SELECT spawn_profile_id FROM roles WHERE name = 'legacy-write-role'`).Scan(&roleProfileID))
	require.NoError(t, d.QueryRow(`SELECT spawn_profile_id FROM group_template_agents WHERE template_id = ?`, t1).Scan(&agentProfileID))
	assert.Equal(t, p2, groupProfileID)
	assert.Equal(t, t2, groupTemplateID)
	assert.Equal(t, p2, roleProfileID)
	assert.Equal(t, p2, agentProfileID)
	globalID, ok, err := GetDashboardPref("tclaude.dash.default_profile_id")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, fmt.Sprint(p2), globalID)

	g, err := GetAgentGroupByName("legacy-write-group")
	require.NoError(t, err)
	assert.Equal(t, "profile-two", g.DefaultProfile)
	assert.Equal(t, "template-two", g.SourceTemplate)
}

func TestStableRegistryRefs_InsertPreservesAuthoritativeHistoricalID(t *testing.T) {
	setupTestDB(t)

	templateID, err := CreateGroupTemplate(&GroupTemplate{Name: "historical"})
	require.NoError(t, err)
	_, err = CreateAgentGroup("source", "")
	require.NoError(t, err)
	_, err = SetAgentGroupDeployMeta("source", "", "historical")
	require.NoError(t, err)
	_, err = DeleteGroupTemplate("historical")
	require.NoError(t, err)

	source, err := GetAgentGroupByName("source")
	require.NoError(t, err)
	require.Equal(t, templateID, source.SourceTemplateID)
	_, err = CreateAgentGroupFrom("clone", *source)
	require.NoError(t, err)

	clone, err := GetAgentGroupByName("clone")
	require.NoError(t, err)
	assert.Equal(t, templateID, clone.SourceTemplateID, "insert trigger must preserve the explicit historical id")
	newID, err := CreateGroupTemplate(&GroupTemplate{Name: "historical"})
	require.NoError(t, err)
	require.NotEqual(t, templateID, newID)
	clone, err = GetAgentGroupByName("clone")
	require.NoError(t, err)
	assert.Equal(t, templateID, clone.SourceTemplateID, "name reuse must not hijack cloned provenance")
}

func TestStableRegistryRefs_StaleRoleUpdateRestoresAuthoritativeID(t *testing.T) {
	setupTestDB(t)

	profileID, err := CreateSpawnProfile(&SpawnProfile{Name: "before"})
	require.NoError(t, err)
	_, err = CreateRole(&Role{Name: "stale-role", SpawnProfile: "before"})
	require.NoError(t, err)
	loadedBeforeRename, err := GetRole("stale-role")
	require.NoError(t, err)
	require.Equal(t, profileID, loadedBeforeRename.SpawnProfileID)

	require.NoError(t, UpdateSpawnProfile(&SpawnProfile{ID: profileID, Name: "after"}))
	loadedBeforeRename.Descr = "unrelated edit"
	require.NoError(t, UpdateRole(loadedBeforeRename))

	got, err := GetRole("stale-role")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, profileID, got.SpawnProfileID)
	assert.Equal(t, "after", got.SpawnProfile)
	assert.Equal(t, "unrelated edit", got.Descr)
	var rawName string
	d, err := Open()
	require.NoError(t, err)
	require.NoError(t, d.QueryRow(`SELECT spawn_profile FROM roles WHERE name = 'stale-role'`).Scan(&rawName))
	assert.Equal(t, "after", rawName, "legacy binaries must see the canonical current name too")
}
