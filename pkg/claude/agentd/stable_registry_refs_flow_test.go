package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The dashboard and CLI continue to exchange portable names even though the
// persisted links are IDs. This pins the important UI contract: every read
// presents the registry entry's current name after a rename.
func TestStableRegistryRefs_WirePresentsCurrentNames(t *testing.T) {
	f := newFlow(t)

	profileID, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "before"})
	require.NoError(t, err)
	_, err = db.CreateAgentGroup("crew", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultProfile("crew", "before")
	require.NoError(t, err)
	_, err = db.CreateGroupTemplate(&db.GroupTemplate{
		Name: "circle", Agents: []db.GroupTemplateAgent{{Name: "worker", SpawnProfile: "before"}},
	})
	require.NoError(t, err)
	require.NoError(t, db.SetDashboardProfileRef(
		"tclaude.dash.default_profile", "tclaude.dash.default_profile_id", "before", profileID))
	require.NoError(t, db.UpdateSpawnProfile(&db.SpawnProfile{ID: profileID, Name: "after"}))

	groups := humanReq(t, f, http.MethodGet, "/v1/groups", nil)
	require.Equal(t, http.StatusOK, groups.Code)
	var groupRows []struct {
		Name           string `json:"name"`
		DefaultProfile string `json:"default_profile"`
	}
	require.NoError(t, json.Unmarshal(groups.Body.Bytes(), &groupRows))
	require.Len(t, groupRows, 1)
	assert.Equal(t, "after", groupRows[0].DefaultProfile)

	template := humanReq(t, f, http.MethodGet, "/v1/templates/circle", nil)
	require.Equal(t, http.StatusOK, template.Code)
	var templateBody struct {
		Agents []struct {
			SpawnProfile string `json:"spawn_profile"`
		} `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(template.Body.Bytes(), &templateBody))
	require.Len(t, templateBody.Agents, 1)
	assert.Equal(t, "after", templateBody.Agents[0].SpawnProfile)

	global := humanReq(t, f, http.MethodGet, "/v1/spawn-profile-default", nil)
	require.Equal(t, http.StatusOK, global.Code)
	var globalBody struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal(global.Body.Bytes(), &globalBody))
	assert.Equal(t, "after", globalBody.Name)
}

func TestStableRegistryRefs_RoleProfileLaunchSurvivesRename(t *testing.T) {
	f := newFlow(t)

	profileID, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "role-before", Model: "haiku"})
	require.NoError(t, err)
	_, err = db.CreateRole(&db.Role{Name: "stable-role", SpawnProfile: "role-before"})
	require.NoError(t, err)
	_, err = db.CreateGroupTemplate(&db.GroupTemplate{
		Name: "role-circle", Agents: []db.GroupTemplateAgent{{Name: "worker", RoleRef: "stable-role"}},
	})
	require.NoError(t, err)

	role, err := db.GetRole("stable-role")
	require.NoError(t, err)
	require.Equal(t, profileID, role.SpawnProfileID)
	require.NoError(t, db.UpdateSpawnProfile(&db.SpawnProfile{ID: profileID, Name: "role-after", Model: "haiku"}))

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/role-circle/instantiate",
		map[string]any{"group_name": "role-crew"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var result struct {
		Spawned int `json:"spawned"`
		Failed  int `json:"failed"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	assert.Equal(t, 1, result.Spawned)
	assert.Zero(t, result.Failed)
}

// TCL-311 + persistent-registry-reference integration: an ID-backed template
// profile reference must still feed the per-field launch resolver after the
// profile is renamed. The
// compatible foreign-tier effort participates, while the incompatible model
// is skipped and disclosed under the profile's current name.
func TestStableRegistryRefs_TemplateLaunchResolutionAndNotesSurviveProfileRename(t *testing.T) {
	f := newFlow(t)

	profileID, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "before", Harness: "codex", Model: "gpt-5", Effort: "high",
	})
	require.NoError(t, err)
	_, err = db.CreateGroupTemplate(&db.GroupTemplate{
		Name: "stable-launch", Agents: []db.GroupTemplateAgent{{
			Name: "worker", Harness: "claude", SpawnProfile: "before",
		}},
	})
	require.NoError(t, err)
	tmpl, err := db.GetGroupTemplate("stable-launch")
	require.NoError(t, err)
	require.Equal(t, profileID, tmpl.Agents[0].SpawnProfileID)
	require.NoError(t, db.UpdateSpawnProfile(&db.SpawnProfile{
		ID: profileID, Name: "after", Harness: "codex", Model: "gpt-5", Effort: "high",
	}))

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/stable-launch/instantiate",
		map[string]any{"group_name": "stable-crew"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var result struct {
		Spawned int `json:"spawned"`
		Failed  int `json:"failed"`
		Agents  []struct {
			ConvID string   `json:"conv_id"`
			Notes  []string `json:"notes"`
		} `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	require.Equal(t, 1, result.Spawned)
	require.Zero(t, result.Failed)
	require.Len(t, result.Agents, 1)

	spawnModel, ok := f.World.SpawnModel(result.Agents[0].ConvID)
	require.True(t, ok)
	assert.Empty(t, spawnModel)
	spawnEffort, ok := f.World.SpawnEffort(result.Agents[0].ConvID)
	require.True(t, ok)
	assert.Equal(t, "high", spawnEffort)
	assert.Contains(t, result.Agents[0].Notes,
		`profile "after" model ignored (not valid for claude)`)
}
