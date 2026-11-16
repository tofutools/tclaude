//ci:reads docs/examples/ways-of-working.yaml

package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tk_e46af423: an agreement gate is a decision several deciders take
// together. All of them choosing agree takes that edge; otherwise the first
// dissent's outcome, in decider order, and the work goes back with every
// decider's notes. The run view shows the tally while votes are out.

type gateRunView struct {
	Status            engine.RunStatus `json:"status"`
	AwaitingDecisions []struct {
		NodeID   string   `json:"nodeId"`
		Verdicts []string `json:"verdicts"`
		Attempt  int      `json:"attempt"`
	} `json:"awaitingDecisions"`
	Gates []struct {
		NodeID  string `json:"nodeId"`
		Agree   string `json:"agree"`
		Agreed  int    `json:"agreed"`
		Total   int    `json:"total"`
		Summary string `json:"summary"`
		Votes   []struct {
			NodeID  string `json:"nodeId"`
			Decider string `json:"decider"`
			Verdict string `json:"verdict"`
		} `json:"votes"`
	} `json:"gates"`
}

func showGateRun(t *testing.T, f *testharness.Flow, runID string) gateRunView {
	t.Helper()
	agentd.WaitForProcessDispatchForTest()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view gateRunView
	testharness.DecodeJSON(t, rec, &view)
	return view
}

func gateSummary(t *testing.T, view gateRunView) string {
	t.Helper()
	require.Len(t, view.Gates, 1, "%+v", view)
	return view.Gates[0].Summary
}

func voteAs(t *testing.T, f *testharness.Flow, conv, runID, node string, attempt int, verdict, note string) {
	t.Helper()
	rec := reportAs(t, f, conv, map[string]any{"runId": runID, "nodeId": node, "attempt": attempt,
		"outcome": "succeeded", "verdict": verdict, "note": note})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Scenario: a reviewer agent and a person decide together. The person asks
// for changes, the agent approves: the work goes back, and the developer is
// told what both said. Next round both approve and the run completes.
func TestProcessAgreementGate_AgentAndHumanDecideTogether(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const devConv, revConv = "gdev-1111-2222-3333-4444", "grev-1111-2222-3333-4444"
	dev := havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	rev := havePerformerAgent(t, f, "crew", revConv, "rita", "reviewer", "idle")
	tmpl := &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: "agree-mixed", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "build"}},
			"build": {Type: model.NodeTypeTask, Next: model.Next{"next": "review"},
				Performer: agentStep("Build it.", func(p *model.Performer) { p.Role = "dev" })},
			"review": {Type: model.NodeTypeDecision, Agree: "approve",
				Deciders: []model.Performer{
					{Kind: model.PerformerAgent, Role: "reviewer", Prompt: "Review it."},
					{Kind: model.PerformerHuman, Ask: "Does it look right to you?"},
				},
				Next:  model.Next{"approve": "done", "changes": "build", "escalate": "stopped"},
				Loops: map[string]model.LoopBound{"changes": {MaxIterations: 2, OnExhausted: "escalate"}}},
			"done":    {Type: model.NodeTypeEnd, Result: "success"},
			"stopped": {Type: model.NodeTypeEnd, Result: "canceled"},
		}}

	const runID = "run_agree_mixed"
	// Started in the group for real, so the run is in the group's context.
	startLoopRun(t, f, root, runID, tmpl)
	view := showPerformerRun(t, f, runID)
	agentID, attempt, _ := commandFor(t, view, "build")
	require.Equal(t, dev, agentID)
	reportStep(t, f, devConv, runID, "build", attempt, nil)

	view = showPerformerRun(t, f, runID)
	agentID, voteAttempt, _ := commandFor(t, view, "review.vote.1")
	require.Equal(t, rev, agentID)
	assert.Contains(t, processInbox(t, revConv)[0].Body, "Choose exactly one verdict: approve, changes, escalate.")
	gate := showGateRun(t, f, runID)
	require.Len(t, gate.AwaitingDecisions, 1)
	assert.Equal(t, "review.vote.2", gate.AwaitingDecisions[0].NodeID, "the person's vote waits on the person")
	assert.Equal(t, "0/2 agreed, waiting on rita, human", gateSummary(t, gate))

	// Nobody decides the gate itself.
	assert.Equal(t, http.StatusConflict, decideLoop(t, f, runID, map[string]any{"nodeId": "review", "verdict": "approve"}))
	assert.Equal(t, http.StatusConflict, decideLoop(t, f, runID, map[string]any{"nodeId": "review.tally", "verdict": "approve"}))

	require.Equal(t, http.StatusAccepted, decideLoop(t, f, runID, map[string]any{"nodeId": "review.vote.2",
		"verdict": "changes", "evidence": "the error message is unclear"}))
	assert.Equal(t, "0/2 agreed, human chose changes, waiting on rita", gateSummary(t, showGateRun(t, f, runID)))
	voteAs(t, f, revConv, runID, "review.vote.1", voteAttempt, "approve", "code is fine")

	view = showPerformerRun(t, f, runID)
	agentID, attempt, _ = commandFor(t, view, "build")
	require.Equal(t, dev, agentID)
	assert.Equal(t, 2, attempt)
	assert.Empty(t, showGateRun(t, f, runID).Gates, "no votes are out while the work is redone")
	inbox := processInbox(t, devConv)
	require.Len(t, inbox, 2)
	body := inbox[1].Body
	assert.Contains(t, body, `The deciders of "review" did not all choose approve, so this work runs again`)
	assert.Contains(t, body, "1. rita: approve\n   code is fine")
	assert.Contains(t, body, "2. human: changes\n   the error message is unclear")
	// The run's graph counts the gate's rounds (a new round starts its body's
	// edges over, so the way the gate went shows once it settles for good).
	round, _ := gateContext(t, f, runID)
	assert.Equal(t, 2, round["build"], "build is in round 2 of the gate's loop")
	assert.Equal(t, 2, round["review"])
	reportStep(t, f, devConv, runID, "build", attempt, nil)

	view = showPerformerRun(t, f, runID)
	_, voteAttempt, _ = commandFor(t, view, "review.vote.1")
	assert.Contains(t, processInbox(t, revConv)[1].Body, "so it is up for a vote again", "a decider is told the vote is open again")
	voteAs(t, f, revConv, runID, "review.vote.1", voteAttempt, "approve", "still fine")
	gate = showGateRun(t, f, runID)
	assert.Equal(t, "1/2 agreed, waiting on human", gateSummary(t, gate))
	require.Len(t, gate.AwaitingDecisions, 1)
	require.Equal(t, http.StatusAccepted, decideLoop(t, f, runID, map[string]any{"nodeId": "review.vote.2",
		"verdict": "approve", "attempt": gate.AwaitingDecisions[0].Attempt}))
	assert.Equal(t, engine.RunCompleted, showGateRun(t, f, runID).Status)
	_, edge := gateContext(t, f, runID)
	assert.Equal(t, string(engine.EdgeArrived), edge["review/approve"])
	assert.Equal(t, string(engine.EdgeNotTaken), edge["review/escalate"])
}

// gateContext reads the run's projection off /v1/process/context: each node's
// round, and each edge's state keyed "from/label".
func gateContext(t *testing.T, f *testharness.Flow, runID string) (map[string]int, map[string]string) {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/context", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var context struct {
		Runs map[string]struct {
			Nodes []struct {
				ID    string `json:"id"`
				Round int    `json:"round"`
			} `json:"nodes"`
			Edges []struct {
				From  string `json:"from"`
				Label string `json:"label"`
				State string `json:"state"`
			} `json:"edges"`
		} `json:"runs"`
	}
	testharness.DecodeJSON(t, rec, &context)
	run, ok := context.Runs[runID]
	require.True(t, ok, "run %s in the context", runID)
	rounds, edges := map[string]int{}, map[string]string{}
	for _, node := range run.Nodes {
		rounds[node.ID] = node.Round
	}
	for _, edge := range run.Edges {
		edges[edge.From+"/"+edge.Label] = edge.State
	}
	return rounds, edges
}

// waysOfWorking is docs/examples/ways-of-working.yaml, the 8-step template.
func waysOfWorking(t *testing.T) *model.Template {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", "docs", "examples", "ways-of-working.yaml"))
	require.NoError(t, err)
	parsed, err := model.Parse(data)
	require.NoError(t, err)
	for _, d := range parsed.Diagnostics {
		require.NotEqual(t, model.SeverityError, d.Severity, "%+v", d)
	}
	return parsed.Template
}

// Scenario: the whole way of working, end to end. The lead frames the task, a
// planner plans it, a usability vet passes the plan, a developer builds it, two
// reviewers who did not build it vote (one asks for changes, so the developer
// does another round with both reviewers' notes), and the lead who framed it
// merges the PR the developer reported.
func TestProcessWaysOfWorking_EightStepsEndToEnd(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const leadConv, planConv, uxConv = "wlead-111-2222-3333-4444", "wplan-111-2222-3333-4444", "wux-1111-2222-3333-4444"
	const devConv, ritaConv, raviConv = "wdev-1111-2222-3333-4444", "writa-111-2222-3333-4444", "wravi-111-2222-3333-4444"
	lena := havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "idle")
	pia := havePerformerAgent(t, f, "crew", planConv, "pia", "planner", "idle")
	uma := havePerformerAgent(t, f, "crew", uxConv, "uma", "ux", "idle")
	dev := havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	rita := havePerformerAgent(t, f, "crew", ritaConv, "rita", "reviewer", "idle")
	ravi := havePerformerAgent(t, f, "crew", raviConv, "ravi", "reviewer", "idle")
	convOf := map[string]string{lena: leadConv, pia: planConv, uma: uxConv, dev: devConv, rita: ritaConv, ravi: raviConv}

	tmpl := waysOfWorking(t)
	putProcessRuntimeTemplate(t, root, tmpl)
	const runID = "run_ways_of_working"
	t.Cleanup(agentd.SetProcessRunScopeForTest(runID, groupScope(g)))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": runID, "templateId": tmpl.ID, "params": map[string]string{"task": "saved filters"},
		"authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	// step runs node's outstanding command: it checks who has it and what they
	// were told, then reports for them.
	step := func(node, want string, told []string, extra map[string]any) {
		t.Helper()
		view := showPerformerRun(t, f, runID)
		agentID, attempt, _ := commandFor(t, view, node)
		require.Equal(t, want, agentID, "who performs %s", node)
		inbox := processInbox(t, convOf[agentID])
		require.NotEmpty(t, inbox)
		for _, text := range told {
			assert.Contains(t, inbox[len(inbox)-1].Body, text, "%s's prompt", node)
		}
		reportStep(t, f, convOf[agentID], runID, node, attempt, extra)
	}

	// 1-4: brief, plan, vet, build.
	step("brief", lena, []string{"Frame this task for the team: saved filters."},
		map[string]any{"note": "Goal: users keep their filters."})
	step("plan", pia, []string{"Goal: users keep their filters."},
		map[string]any{"note": "1. store filters per user\n2. restore on load"})
	step("vet", uma, []string{"1. store filters per user"}, map[string]any{"verdict": "ok", "note": "clear enough"})
	step("build", dev, []string{"1. store filters per user"}, map[string]any{"outputs": map[string]any{"pr": "PR #77"}})

	// 5-7: two reviewers, neither of them the developer; ravi asks for changes.
	view := showPerformerRun(t, f, runID)
	first, _, _ := commandFor(t, view, "review.vote.1")
	second, _, _ := commandFor(t, view, "review.vote.2")
	assert.ElementsMatch(t, []string{rita, ravi}, []string{first, second}, "two different reviewers, not the developer")
	assert.Contains(t, gateSummary(t, showGateRun(t, f, runID)), "0/2 agreed, waiting on ")
	step("review.vote.1", first, []string{"PR #77"}, map[string]any{"verdict": "approve", "note": "looks good"})
	assert.Equal(t, "1/2 agreed, waiting on "+map[string]string{rita: "rita", ravi: "ravi"}[second],
		gateSummary(t, showGateRun(t, f, runID)))
	step("review.vote.2", second, []string{"for correctness and tests"},
		map[string]any{"verdict": "changes", "note": "add a test for restoring"})

	// The developer does round 2, told what both reviewers said.
	step("build", dev, []string{"Sent back for round 2 of 3", ": approve\n   looks good", ": changes\n   add a test for restoring"},
		map[string]any{"outputs": map[string]any{"pr": "PR #77 (with restore test)"}})
	view = showPerformerRun(t, f, runID)
	first, _, _ = commandFor(t, view, "review.vote.1")
	second, _, _ = commandFor(t, view, "review.vote.2")
	assert.ElementsMatch(t, []string{rita, ravi}, []string{first, second})
	step("review.vote.1", first, []string{"PR #77 (with restore test)"}, map[string]any{"verdict": "approve", "note": "ok"})
	step("review.vote.2", second, nil, map[string]any{"verdict": "approve", "note": "tests added"})

	// 8: the lead who framed it merges.
	view = showPerformerRun(t, f, runID)
	assert.Equal(t, "same_as", resolutionOf(t, view, "merge"))
	step("merge", lena, []string{"Both reviewers approved", "PR #77 (with restore test)"}, nil)
	assert.Equal(t, engine.RunCompleted, showPerformerRun(t, f, runID).Status)
}

// Scenario: the usability vet escalates the plan instead of passing it, and
// the run fails, so a board task it runs for is blocked with the reason.
func TestProcessWaysOfWorking_EscalationFailsTheRun(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const leadConv, planConv, uxConv = "elead-111-2222-3333-4444", "eplan-111-2222-3333-4444", "eux-1111-2222-3333-4444"
	lena := havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "idle")
	pia := havePerformerAgent(t, f, "crew", planConv, "pia", "planner", "idle")
	uma := havePerformerAgent(t, f, "crew", uxConv, "uma", "ux", "idle")
	convOf := map[string]string{lena: leadConv, pia: planConv, uma: uxConv}

	tmpl := waysOfWorking(t)
	putProcessRuntimeTemplate(t, root, tmpl)
	const runID = "run_ways_escalate"
	t.Cleanup(agentd.SetProcessRunScopeForTest(runID, groupScope(g)))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": runID, "templateId": tmpl.ID, "params": map[string]string{"task": "a new billing model"},
		"authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	for _, s := range []struct {
		node, who string
		extra     map[string]any
	}{
		{"brief", lena, map[string]any{"note": "Goal: usage billing."}},
		{"plan", pia, map[string]any{"note": "1. meter usage"}},
		{"vet", uma, map[string]any{"verdict": "escalate", "note": "this needs the operator"}},
	} {
		view := showPerformerRun(t, f, runID)
		agentID, attempt, _ := commandFor(t, view, s.node)
		require.Equal(t, s.who, agentID, s.node)
		reportStep(t, f, convOf[agentID], runID, s.node, attempt, s.extra)
	}
	assert.Equal(t, engine.RunFailed, showPerformerRun(t, f, runID).Status)
}
