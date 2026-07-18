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

// JOH-242: the advisory process runtime. These flow tests drive the daemon's
// instantiate / process endpoints with the tmux/spawn simulators and assert at
// real surfaces: the group's runtime process state, the per-agent "## Process"
// block in each spawned agent's startup briefing (its inbox), the advance
// transition + entering-role nudges, the advance gating, and cleanup on group
// delete.

// processStateResult mirrors the GET /v1/groups/{name}/process response.
type processStateResult struct {
	CurrentPhase string `json:"current_phase"`
	PhaseIndex   int    `json:"phase_index"`
	PhaseCount   int    `json:"phase_count"`
	Phases       []struct {
		Name     string   `json:"name"`
		Roles    []string `json:"roles"`
		Criteria string   `json:"criteria"`
		Current  bool     `json:"current"`
	} `json:"phases"`
	Transitions []struct {
		From  string `json:"from"`
		To    string `json:"to"`
		Actor string `json:"actor"`
	} `json:"transitions"`
}

// processTemplateBody builds a 3-phase template whose roster has one agent per
// role. Phase "review" lists the reserved "all", so every member is active in
// it. No work_pattern, so each agent's sole inbox message is its startup
// briefing (soleInboxMessage).
func processTemplateBody(name string) map[string]any {
	return map[string]any{
		"name":            name,
		"default_context": "HOUSE RULES",
		"agents": []templateAgentSpec{
			{Name: "arch", Role: "architect"},
			{Name: "dev", Role: "dev"},
			{Name: "rev", Role: "reviewer"},
		},
		"process": []map[string]any{
			{"name": "design", "roles": []string{"architect"}, "criteria": "a plan exists"},
			{"name": "build", "roles": []string{"dev"}, "criteria": "code compiles"},
			{"name": "review", "roles": []string{"reviewer", "all"}, "criteria": "approved"},
		},
	}
}

// convByName maps the instantiate result's template-agent names to spawned
// conv-ids.
func convByName(res instantiateResult) map[string]string {
	out := map[string]string{}
	for _, a := range res.Agents {
		out[a.Name] = a.ConvID
	}
	return out
}

// Scenario: instantiate a template with a process. The group's runtime process
// state stands up at the FIRST phase, and every spawned agent's startup
// briefing carries a "## Process" block that calls out which phases THAT
// agent's role is active in.
func TestProcess_InstantiateStandsUpStateAndBriefs(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", processTemplateBody("quest")).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/quest/instantiate",
		map[string]any{"group_name": "party", "task": "ship it"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 3, res.Spawned, "three agents spawned: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	// Runtime state stands up at the first phase.
	g, err := db.GetAgentGroupByName("party")
	require.NoError(t, err)
	require.NotNil(t, g)
	st, err := db.GetGroupProcessState(g.ID)
	require.NoError(t, err)
	require.NotNil(t, st, "process state created at instantiate")
	assert.Equal(t, "design", st.CurrentPhase, "starts at the first phase")

	// The GET endpoint surfaces it (phase 1/3).
	gr := humanReq(t, f, http.MethodGet, "/v1/groups/party/process", nil)
	require.Equalf(t, http.StatusOK, gr.Code, "GET process: %s", gr.Body.String())
	var view processStateResult
	testharness.DecodeJSON(t, gr, &view)
	assert.Equal(t, "design", view.CurrentPhase)
	assert.Equal(t, 0, view.PhaseIndex)
	assert.Equal(t, 3, view.PhaseCount)
	require.Len(t, view.Transitions, 1, "one initial transition")
	assert.Equal(t, "", view.Transitions[0].From, "initial transition is from empty")
	assert.Equal(t, "design", view.Transitions[0].To)

	// Every agent's startup briefing carries the ## Process block, and the
	// per-role callout differs by role.
	convs := convByName(res)
	archMsg := soleInboxMessage(t, convs["arch"]).Body
	devMsg := soleInboxMessage(t, convs["dev"]).Body
	assert.Contains(t, archMsg, "## Process", "arch briefing has the process block")
	assert.Contains(t, devMsg, "## Process", "dev briefing has the process block")
	assert.Contains(t, archMsg, `Your role ("architect") is active in phase(s): design`,
		"arch is called out active in design")
	assert.Contains(t, devMsg, `Your role ("dev") is active in phase(s): build`,
		"dev is called out active in build")
	// The block renders the whole ordered phase map with criteria.
	assert.Contains(t, devMsg, "**design**", "phase map lists design")
	assert.Contains(t, devMsg, "code compiles", "phase map carries criteria prose")
}

// Scenario: advancing the process to the next phase records a transition and
// nudges ONLY the members whose role is active in the phase just entered.
func TestProcess_AdvanceRecordsAndNudgesEnteringRolesOnly(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", processTemplateBody("quest")).Code, "create template")
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/quest/instantiate",
		map[string]any{"group_name": "party"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	agentd.WaitForBackgroundForTest()
	convs := convByName(res)

	// Advance design → build. Phase "build" is active for the "dev" role only.
	ar := humanReq(t, f, http.MethodPost, "/v1/groups/party/process/advance", map[string]any{})
	require.Equalf(t, http.StatusOK, ar.Code, "advance: %s", ar.Body.String())
	var adv struct {
		From     string `json:"from"`
		To       string `json:"to"`
		Notified int    `json:"notified"`
	}
	testharness.DecodeJSON(t, ar, &adv)
	assert.Equal(t, "design", adv.From)
	assert.Equal(t, "build", adv.To)
	assert.Equal(t, 1, adv.Notified, "only the one dev (entering role) is nudged")
	agentd.WaitForBackgroundForTest()

	// The dev got a [process] nudge naming old → new + the criteria; arch and
	// rev did not (they are not active in "build").
	assert.True(t, hasProcessNudge(t, convs["dev"], "build"), "dev (entering role) got the phase nudge")
	assert.Contains(t, processNudgeBody(t, convs["dev"]), "code compiles", "nudge carries the phase criteria")
	assert.False(t, hasProcessNudge(t, convs["arch"], "build"), "arch (not entering) got no nudge")
	assert.False(t, hasProcessNudge(t, convs["rev"], "build"), "rev (not entering) got no nudge")

	// The transition is recorded.
	st, err := db.GetGroupProcessState(mustGroupID(t, "party"))
	require.NoError(t, err)
	require.NotNil(t, st)
	assert.Equal(t, "build", st.CurrentPhase, "current phase advanced")
	trs, err := db.ListGroupProcessTransitions(st.GroupID)
	require.NoError(t, err)
	require.Len(t, trs, 2, "initial + advance transitions")
	assert.Equal(t, "design", trs[1].FromPhase)
	assert.Equal(t, "build", trs[1].ToPhase)
}

// Scenario: advance gating. A plain member without process.advance is refused;
// a group owner passes via the owner bypass; a member explicitly granted
// process.advance passes too. The human always passes.
func TestProcess_AdvanceGating(t *testing.T) {
	f := newFlow(t)

	g := f.HaveGroup("squad")
	const owner = "proc-owner-bbbb-cccc-dddd"
	const worker = "proc-workr-bbbb-cccc-dddd"
	f.HaveConvWithTitle(owner, "owner")
	f.HaveAliveSession(owner, "lbl-owner", "tmux-owner", f.TestCwd("owner"))
	f.HaveMemberWithRole("squad", owner, "lead")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-worker", "tmux-worker", f.TestCwd("worker"))
	f.HaveMemberWithRole("squad", worker, "dev")
	require.NoError(t, db.AddAgentGroupOwner(g.ID, owner, "test"), "seed owner")

	phases := []db.ProcessPhase{
		{Name: "design", Roles: []string{"architect"}},
		{Name: "build", Roles: []string{"dev"}},
		{Name: "review", Roles: []string{"reviewer"}},
	}
	require.NoError(t, db.InitGroupProcess(g.ID, phases, "human"), "seed process state")

	// A plain member without the slug is refused.
	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/groups/squad/process/advance", map[string]any{}), worker))
	assert.Equalf(t, http.StatusForbidden, rec.Code,
		"non-owner without process.advance should be 403; body=%s", rec.Body.String())

	// An owner passes via the structural owner bypass.
	rec = testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/groups/squad/process/advance", map[string]any{}), owner))
	require.Equalf(t, http.StatusOK, rec.Code, "owner advance should pass; body=%s", rec.Body.String())
	agentd.WaitForBackgroundForTest()

	// A plain member explicitly granted process.advance passes.
	require.NoError(t, db.GrantAgentPermission(worker, agentd.PermProcessAdvance, "test"), "grant slug")
	rec = testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/groups/squad/process/advance", map[string]any{}), worker))
	require.Equalf(t, http.StatusOK, rec.Code, "granted member advance should pass; body=%s", rec.Body.String())
	agentd.WaitForBackgroundForTest()
}

// Scenario: a template with NO process spawns a group with no process state and
// no "## Process" block; the GET endpoint 404s.
func TestProcess_NoProcessDegradesEverywhere(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{"name": "plain", "default_context": "HOUSE RULES", "agents": []templateAgentSpec{{Name: "solo", Role: "dev"}}}).Code, "create template")
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/plain/instantiate",
		map[string]any{"group_name": "plain-grp"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	agentd.WaitForBackgroundForTest()

	st, err := db.GetGroupProcessState(mustGroupID(t, "plain-grp"))
	require.NoError(t, err)
	assert.Nil(t, st, "no process → no state")

	assert.NotContains(t, soleInboxMessage(t, res.Agents[0].ConvID).Body, "## Process",
		"no process → no block in the briefing")

	gr := humanReq(t, f, http.MethodGet, "/v1/groups/plain-grp/process", nil)
	assert.Equal(t, http.StatusNotFound, gr.Code, "GET process 404s when there is no process")

	// Advancing a process-less group is a 404, not a 500.
	ar := humanReq(t, f, http.MethodPost, "/v1/groups/plain-grp/process/advance", map[string]any{})
	assert.Equal(t, http.StatusNotFound, ar.Code, "advance 404s when there is no process")
}

// Scenario: deleting a group sweeps its process state + transitions.
func TestProcess_GroupDeleteCleansState(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", processTemplateBody("quest")).Code, "create template")
	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates/quest/instantiate",
		map[string]any{"group_name": "doomed"}).Code, "instantiate")
	agentd.WaitForBackgroundForTest()

	gid := mustGroupID(t, "doomed")
	st, err := db.GetGroupProcessState(gid)
	require.NoError(t, err)
	require.NotNil(t, st, "state exists before delete")

	require.NoError(t, db.DeleteAgentGroup("doomed"), "delete group")

	st, err = db.GetGroupProcessState(gid)
	require.NoError(t, err)
	assert.Nil(t, st, "process state swept on group delete")
	trs, err := db.ListGroupProcessTransitions(gid)
	require.NoError(t, err)
	assert.Empty(t, trs, "transitions swept on group delete")
}

// --- helpers ---

func mustGroupID(t *testing.T, name string) int64 {
	t.Helper()
	g, err := db.GetAgentGroupByName(name)
	require.NoError(t, err)
	require.NotNilf(t, g, "group %q exists", name)
	return g.ID
}

// hasProcessNudge reports whether conv received a "[process] phase: <phase>"
// message.
func hasProcessNudge(t *testing.T, conv, phase string) bool {
	t.Helper()
	msgs, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	for _, m := range msgs {
		if strings.Contains(m.Subject, "[process] phase: "+phase) {
			return true
		}
	}
	return false
}

// processNudgeBody returns the body of conv's first [process] nudge.
func processNudgeBody(t *testing.T, conv string) string {
	t.Helper()
	msgs, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	for _, m := range msgs {
		if strings.HasPrefix(m.Subject, "[process] phase: ") {
			return m.Body
		}
	}
	return ""
}
