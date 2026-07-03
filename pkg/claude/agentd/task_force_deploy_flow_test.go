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

// JOH-245: `task-force deploy` — the first-class "deploy a task force against
// a mission" verb over the instantiate path. These flow tests drive the
// daemon's /v1/templates/{name}/deploy endpoint with the tmux/spawn
// simulators and assert at real surfaces: the group's composed context (the
// mission folded under "## Mission"), the group row's deployment provenance
// (mission / source_template — where the dashboard reads it), the per-agent
// spawn results, the interpolated work-pattern deliveries, and the derived
// group name (+ collision uniquify).

// deployResult mirrors the JSON the deploy endpoint returns — the
// instantiate shape plus the deploy framing.
type deployResult struct {
	Group            string `json:"group"`
	Template         string `json:"template"`
	Spawned          int    `json:"spawned"`
	Failed           int    `json:"failed"`
	Deployed         bool   `json:"deployed"`
	Mission          string `json:"mission"`
	PatternDelivered int      `json:"pattern_delivered"`
	PatternErrors    []string `json:"pattern_errors"`
	Agents           []struct {
		Name      string   `json:"name"`
		FinalName string   `json:"final_name"`
		ConvID    string   `json:"conv_id"`
		Owner     bool     `json:"owner"`
		Granted   []string `json:"granted"`
		Error     string   `json:"error"`
	} `json:"agents"`
}

// Scenario: deploy a 2-agent template against an explicit-group-named mission.
// The mission renders into the group context under "## Mission", the group row
// records mission + source_template, the whole team spawns, and a work-pattern
// step interpolates the mission via {{mission}}.
func TestTaskForceDeploy_RendersMissionAndRecordsProvenance(t *testing.T) {
	f := newFlow(t)

	const boilerplate = "HOUSE-RULES: worktrees + PRs."
	createBody := map[string]any{
		"name":            "strike-team",
		"descr":           "a lead and a dev",
		"default_context": boilerplate,
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", InitialMessage: "You lead.", IsOwner: true, Permissions: []string{agentd.PermGroupsSpawn}},
			{Name: "dev", Role: "dev"},
		},
		"work_pattern": []map[string]string{
			{"send_to": "lead", "value": "Own the mission: {{mission}}"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	const mission = "Ship the passwordless-login epic."
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/strike-team/deploy",
		map[string]any{"group_name": "raid", "mission": mission})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())

	var res deployResult
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, "raid", res.Group)
	assert.True(t, res.Deployed, "response is framed as a deploy")
	assert.Equal(t, mission, res.Mission, "response echoes the mission")
	assert.Equal(t, 2, res.Spawned, "both agents spawned")
	assert.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	assert.Equal(t, 1, res.PatternDelivered, "the one work-pattern step was delivered")
	assert.Empty(t, res.PatternErrors, "no work-pattern errors")
	agentd.WaitForBackgroundForTest()

	// The group carries the mission under "## Mission" (NOT "## Task") plus the
	// template boilerplate — the real surface the dashboard reads and every
	// agent's inbox briefing carries.
	g, err := db.GetAgentGroupByName("raid")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Contains(t, g.DefaultContext, "## Mission", "mission renders under its own header")
	assert.Contains(t, g.DefaultContext, mission, "group context carries the mission")
	assert.Contains(t, g.DefaultContext, boilerplate, "group context keeps template boilerplate")
	assert.NotContains(t, g.DefaultContext, "## Task", "deploy uses Mission, not Task")

	// Deployment provenance is readable back where the dashboard reads it.
	assert.Equal(t, mission, g.Mission, "group row records the mission")
	assert.Equal(t, "strike-team", g.SourceTemplate, "group row records the source template")

	// The lead's inbox got the work-pattern step with the mission interpolated.
	leadConv := ""
	for _, a := range res.Agents {
		if a.Name == "lead" {
			leadConv = a.ConvID
			assert.True(t, a.Owner, "lead is the group owner")
		}
	}
	require.NotEmpty(t, leadConv)
	msgs, err := db.ListAgentMessagesForConv(leadConv, 100)
	require.NoError(t, err)
	joined := ""
	for _, m := range msgs {
		joined += m.Body + "\n"
	}
	assert.Contains(t, joined, "Own the mission: "+mission,
		"work-pattern step interpolates {{mission}}")
}

// Scenario: deploy with no --group derives a group name from the mission text
// (slugged), and the derived group is created.
func TestTaskForceDeploy_DerivesGroupNameFromMission(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{"name": "crew", "agents": []templateAgentSpec{{Name: "lead"}}}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/crew/deploy",
		map[string]any{"mission": "Fix the flaky CI pipeline"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())

	var res deployResult
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, "fix-the-flaky-ci-pipeline", res.Group, "group name derived from the mission slug")
	agentd.WaitForBackgroundForTest()

	g, err := db.GetAgentGroupByName("fix-the-flaky-ci-pipeline")
	require.NoError(t, err)
	require.NotNil(t, g, "derived-name group was created")
	assert.Equal(t, "crew", g.SourceTemplate)
}

// Scenario: a derived group name that collides with an existing group is
// uniquified with a -2 suffix rather than 409-ing (the human named nothing, so
// deploy picks a free name for them).
func TestTaskForceDeploy_DerivedNameCollisionUniquifies(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{"name": "crew", "agents": []templateAgentSpec{{Name: "lead"}}}).Code, "create template")

	// Pre-claim the slug the next deploy would derive.
	_, err := db.CreateAgentGroup("harden-auth", "existing")
	require.NoError(t, err)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/crew/deploy",
		map[string]any{"mission": "Harden auth"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())

	var res deployResult
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, "harden-auth-2", res.Group, "collision uniquified with -2")
	agentd.WaitForBackgroundForTest()
}

// Scenario: a bare-URL mission (a Linear link, no readable words) has nothing
// to slug, so the derived group name falls back to the template name. The URL
// is still stored verbatim as the mission (no title pull — out of scope).
func TestTaskForceDeploy_BareURLMissionFallsBackToTemplateName(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{"name": "quest-party", "agents": []templateAgentSpec{{Name: "lead"}}}).Code, "create template")

	const url = "https://linear.app/tofutools/issue/JOH-245"
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/quest-party/deploy",
		map[string]any{"mission": url})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())

	var res deployResult
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, "quest-party", res.Group, "bare-URL mission → name after the template")
	agentd.WaitForBackgroundForTest()

	g, err := db.GetAgentGroupByName("quest-party")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, url, g.Mission, "the Linear link is stored verbatim (no title pull)")
	assert.Contains(t, g.DefaultContext, url, "the link renders into the mission section verbatim")
}

// Scenario: an explicit group_name that is already taken is a hard 409 — the
// human named it, so deploy does not silently pick another (mirrors
// instantiate).
func TestTaskForceDeploy_ExplicitTakenGroupNameConflicts(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{"name": "crew", "agents": []templateAgentSpec{{Name: "lead"}}}).Code, "create template")
	_, err := db.CreateAgentGroup("taken", "existing")
	require.NoError(t, err)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/crew/deploy",
		map[string]any{"mission": "x", "group_name": "taken"})
	assert.Equalf(t, http.StatusConflict, rec.Code, "explicit taken name should 409; body=%s", rec.Body.String())
}

// Scenario: deploy with no mission is a 400 — the mission is the whole point.
func TestTaskForceDeploy_MissingMissionRejected(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{"name": "crew", "agents": []templateAgentSpec{{Name: "lead"}}}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/crew/deploy",
		map[string]any{"group_name": "g1"})
	assert.Equalf(t, http.StatusBadRequest, rec.Code, "missing mission should 400; body=%s", rec.Body.String())

	// Nothing was created.
	g, err := db.GetAgentGroupByName("g1")
	require.NoError(t, err)
	assert.Nil(t, g, "no group created for a rejected deploy")
}
