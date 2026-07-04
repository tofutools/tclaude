package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-247: re-brief a deployed force. These flow tests drive the daemon's
// /v1/groups/{name}/rebrief endpoint with the tmux/spawn simulators and assert
// at real surfaces: the force's live members receive the RE-delivered
// work-pattern steps (a second copy, mission interpolated), the gating (human /
// owner pass, plain member refused unless granted templates.instantiate), and the clear
// 4xx degradations (not-a-force, source-template-deleted).

// countInboxContains returns how many of conv's inbox messages carry needle in
// their body — used to prove a re-brief delivered ANOTHER copy of a step on top
// of the deploy's original.
func countInboxContains(t *testing.T, conv, needle string) int {
	t.Helper()
	msgs, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	n := 0
	for _, m := range msgs {
		if strings.Contains(m.Body, needle) {
			n++
		}
	}
	return n
}

// rebriefResult mirrors the POST /v1/groups/{name}/rebrief response.
type rebriefResult struct {
	Group            string   `json:"group"`
	Template         string   `json:"template"`
	Mission          string   `json:"mission"`
	PatternDelivered int      `json:"pattern_delivered"`
	PatternErrors    []string `json:"pattern_errors"`
}

// deployStrikeTeam deploys a 2-agent (owner lead + dev) template with a
// broadcast work-pattern step against a mission, returning the deploy result so
// a test can read the per-agent convs.
func deployStrikeTeam(t *testing.T, f *testharness.Flow, mission string) deployResult {
	t.Helper()
	createBody := map[string]any{
		"name":  "strike-team",
		"descr": "a lead and a dev",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", IsOwner: true},
			{Name: "dev", Role: "dev"},
		},
		"work_pattern": []map[string]string{
			{"send_to": "all", "value": "Mission brief: {{mission}}"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/strike-team/deploy",
		map[string]any{"group_name": "raid", "mission": mission})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res deployResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 2, res.Spawned, "both agents spawned: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()
	return res
}

// convForAgent returns the spawned conv-id of the named template agent.
func convForAgent(res deployResult, name string) string {
	for _, a := range res.Agents {
		if a.Name == name {
			return a.ConvID
		}
	}
	return ""
}

// Scenario: re-briefing a deployed force re-delivers the source template's work
// pattern (mission interpolated) to the LIVE roster — every member ends up with
// a SECOND copy of the broadcast step on top of the deploy's original.
func TestRebrief_RedeliversWorkPatternToLiveMembers(t *testing.T) {
	f := newFlow(t)

	const mission = "Ship the passwordless-login epic."
	res := deployStrikeTeam(t, f, mission)
	leadConv := convForAgent(res, "lead")
	devConv := convForAgent(res, "dev")
	require.NotEmpty(t, leadConv)
	require.NotEmpty(t, devConv)

	const step = "Mission brief: " + mission
	require.Equal(t, 1, countInboxContains(t, leadConv, step), "deploy delivered one copy to the lead")
	require.Equal(t, 1, countInboxContains(t, devConv, step), "deploy delivered one copy to the dev")

	// Re-brief the force.
	rec := humanReq(t, f, http.MethodPost, "/v1/groups/raid/rebrief", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "rebrief: %s", rec.Body.String())
	var rb rebriefResult
	testharness.DecodeJSON(t, rec, &rb)
	assert.Equal(t, "raid", rb.Group)
	assert.Equal(t, "strike-team", rb.Template)
	assert.Equal(t, mission, rb.Mission)
	assert.Equal(t, 2, rb.PatternDelivered, "broadcast step re-delivered to both live members")
	assert.Empty(t, rb.PatternErrors, "no work-pattern errors: %v", rb.PatternErrors)
	agentd.WaitForBackgroundForTest()

	assert.Equal(t, 2, countInboxContains(t, leadConv, step), "lead got a second copy after rebrief")
	assert.Equal(t, 2, countInboxContains(t, devConv, step), "dev got a second copy after rebrief")
}

// Scenario: re-brief gating mirrors process-advance but on templates.instantiate. A
// plain member without the slug is refused; the group owner passes structurally;
// a member explicitly granted templates.instantiate passes; the human always passes.
func TestRebrief_Gating(t *testing.T) {
	f := newFlow(t)

	res := deployStrikeTeam(t, f, "Harden auth")
	leadConv := convForAgent(res, "lead") // the owner
	devConv := convForAgent(res, "dev")   // a plain member
	require.NotEmpty(t, leadConv)
	require.NotEmpty(t, devConv)

	// A plain member without templates.instantiate is refused.
	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/groups/raid/rebrief", nil), devConv))
	assert.Equalf(t, http.StatusForbidden, rec.Code,
		"non-owner without templates.instantiate should be 403; body=%s", rec.Body.String())

	// The owner (the lead) passes via the structural owner bypass.
	rec = testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/groups/raid/rebrief", nil), leadConv))
	require.Equalf(t, http.StatusOK, rec.Code, "owner rebrief should pass; body=%s", rec.Body.String())
	agentd.WaitForBackgroundForTest()

	// A plain member explicitly granted templates.instantiate passes.
	require.NoError(t, db.GrantAgentPermission(devConv, agentd.PermTemplatesUse, "test"), "grant slug")
	rec = testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/groups/raid/rebrief", nil), devConv))
	require.Equalf(t, http.StatusOK, rec.Code, "granted member rebrief should pass; body=%s", rec.Body.String())
	agentd.WaitForBackgroundForTest()
}

// Scenario: re-briefing after the source template has been deleted is a clear
// 4xx — nothing is sent (no partial weirdness).
func TestRebrief_TemplateDeletedIsClear4xx(t *testing.T) {
	f := newFlow(t)

	res := deployStrikeTeam(t, f, "Ship it")
	leadConv := convForAgent(res, "lead")
	require.NotEmpty(t, leadConv)
	before := countInboxContains(t, leadConv, "Mission brief:")

	// Delete the source template out from under the force.
	_, err := db.DeleteGroupTemplate("strike-team")
	require.NoError(t, err)

	rec := humanReq(t, f, http.MethodPost, "/v1/groups/raid/rebrief", nil)
	assert.Equalf(t, http.StatusUnprocessableEntity, rec.Code,
		"deleted template should 422; body=%s", rec.Body.String())
	agentd.WaitForBackgroundForTest()

	assert.Equal(t, before, countInboxContains(t, leadConv, "Mission brief:"),
		"a 4xx rebrief delivered nothing")
}

// Scenario: re-briefing a group that was never deployed from a template (no
// source template) is a 400 — there is nothing to re-brief.
func TestRebrief_NotAForceRejected(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("hand-built")
	rec := humanReq(t, f, http.MethodPost, "/v1/groups/hand-built/rebrief", nil)
	assert.Equalf(t, http.StatusBadRequest, rec.Code,
		"a non-force group should 400; body=%s", rec.Body.String())
}

// Scenario: re-briefing a force whose template has no work pattern is a clear
// 4xx — the group is a force, but there is no pattern to re-deliver.
func TestRebrief_NoWorkPatternIsClear4xx(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{"name": "crew", "agents": []templateAgentSpec{{Name: "lead", IsOwner: true}}}).Code, "create template")
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/crew/deploy",
		map[string]any{"group_name": "party", "mission": "do a thing"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	agentd.WaitForBackgroundForTest()

	rec = humanReq(t, f, http.MethodPost, "/v1/groups/party/rebrief", nil)
	assert.Equalf(t, http.StatusUnprocessableEntity, rec.Code,
		"a force with no work pattern should 422; body=%s", rec.Body.String())
}
