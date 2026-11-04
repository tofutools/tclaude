package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A deployed task force's mission is kept apart from the group's standing
// context and delivered to every member whatever include_group_context says,
// the way role and process blocks are. A profile that leaves the group context
// out drops the standing guidance, never the job the force was deployed for.

const (
	missionBoilerplate = "MISSION-TEST STANDING GUIDANCE"
	missionText        = "Ship the passwordless-login epic."
)

func deployMissionForce(t *testing.T, f *testharness.Flow) deployResult {
	t.Helper()
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "mission-team", "default_context": missionBoilerplate,
		"agents": []templateAgentSpec{{Name: "dev", Role: "dev", InitialMessage: "Do it."}},
	}).Code, "create template")
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/mission-team/deploy",
		map[string]any{"group_name": "mission-force", "mission": missionText})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res deployResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equalf(t, 1, res.Spawned, "member spawned: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()
	return res
}

// Nothing configured: a member's brief is what it always was — the standing
// context with the mission section after it.
func TestDeployMission_NothingConfiguredBriefUnchanged(t *testing.T) {
	f := newFlow(t)
	res := deployMissionForce(t, f)
	body := inboxBodies(t, res.Agents[0].ConvID)
	assert.Contains(t, body, missionBoilerplate+"\n\n## Mission\n\n"+missionText)
}

func TestDeployMission_DeliveredWhenGroupContextIsOff(t *testing.T) {
	f := newFlow(t)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "no-context", "harness": "claude", "include_group_default_context": false,
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "no-context").Code)
	res := deployMissionForce(t, f)

	body := inboxBodies(t, res.Agents[0].ConvID)
	assert.Contains(t, body, "## Mission\n\n"+missionText, "the deployed member still gets its mission")
	assert.NotContains(t, body, missionBoilerplate, "the standing context follows the profile")

	// A later direct spawn into the force gets the same treatment.
	spawn := f.AsHuman().SpawnWith("mission-force", map[string]any{"name": "late", "initial_message": "Help out."})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	late := inboxBodies(t, spawn.ConvID)
	assert.Contains(t, late, "## Mission\n\n"+missionText)
	assert.NotContains(t, late, missionBoilerplate)
}

// A direct spawn into a deployed force with nothing configured is briefed as
// before the split: standing context, then the mission.
func TestDeployMission_DirectSpawnIntoForceUnchanged(t *testing.T) {
	f := newFlow(t)
	deployMissionForce(t, f)
	spawn := f.AsHuman().SpawnWith("mission-force", map[string]any{"name": "late", "initial_message": "Help out."})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	assert.Contains(t, inboxBodies(t, spawn.ConvID), missionBoilerplate+"\n\n## Mission\n\n"+missionText)
}
