package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The inline editor's ⑂ on an agent step makes its agent decide (fixture case
// branch-review-decides: the build-review-ship starter's review step). These
// runs check that the run does what the editor says: the decision goes to the
// agent its Who names, whichever Who that is, and the run follows the
// outcome the agent reports.

// editorAgentDecision is that template, with review's Who as the editor's
// Who picker sets it: the selectors cleared, then edit applied.
func editorAgentDecision(t *testing.T, edit func(*model.Performer)) *model.Template {
	t.Helper()
	tmpl, err := agentd.MiniEditCaseTemplateForTest("branch-review-decides")
	require.NoError(t, err)
	review := tmpl.Nodes["review"]
	require.Equal(t, model.NodeTypeDecision, review.Type)
	require.Equal(t, model.PerformerAgent, review.Performer.Kind)
	performer := *review.Performer
	performer.Assignee, performer.Role, performer.Anyone, performer.Lead = "", "", false, false
	edit(&performer)
	review.Performer = &performer
	tmpl.Nodes["review"] = review
	// The starter's merge runs gh, a command a run must authorize; these
	// runs stop before it, so an agent stands in.
	merge := tmpl.Nodes["merge"]
	merge.Performer = &model.Performer{Kind: model.PerformerAgent, Role: "dev", Prompt: "Merge the PR."}
	tmpl.Nodes["merge"] = merge
	return tmpl
}

func startEditorRun(t *testing.T, f *testharness.Flow, root, runID string, tmpl *model.Template, g *db.AgentGroup) {
	t.Helper()
	putProcessRuntimeTemplate(t, root, tmpl)
	t.Cleanup(agentd.SetProcessRunScopeForTest(runID, groupScope(g)))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": runID, "templateId": tmpl.ID, "params": map[string]string{"issue": "#7"}, "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessDispatchForTest()
}

func reportReview(t *testing.T, f *testharness.Flow, conv, runID string, attempt int, verdict string) int {
	t.Helper()
	return reportAs(t, f, conv, map[string]any{
		"runId": runID, "nodeId": "review", "attempt": attempt, "outcome": "succeeded", "verdict": verdict, "note": verdict + " because",
	}).Code
}

// Scenario: each Who the editor offers on a decision (a specific agent, a
// role, anyone free, the lead, a new agent with a profile) is handed the
// review, and its "approved" moves the run on to the operator's approval.
func TestEditorAgentDecision_EveryWhoDecidesAndItsVerdictRoutes(t *testing.T) {
	const dev = "edev-1111-2222-3333-4444"
	cases := []struct {
		name       string
		resolution string
		who        func(p *model.Performer, carol string)
		// decider is the agent expected to decide; "" for a spawned one.
		decider func(carol, lena string) string
	}{
		{"specific agent", "assignee", func(p *model.Performer, carol string) { p.Assignee = carol }, func(carol, _ string) string { return carol }},
		{"role", "role", func(p *model.Performer, _ string) { p.Role = "reviewer" }, func(carol, _ string) string { return carol }},
		{"anyone free", "anyone", func(p *model.Performer, _ string) { p.Anyone = true }, nil},
		{"the lead", "lead", func(p *model.Performer, _ string) { p.Lead = true }, func(_, lena string) string { return lena }},
		{"a new agent", "spawned", func(p *model.Performer, _ string) {
			p.Fallback = &model.PerformerFallback{Action: model.FallbackSpawn, Profile: "opus-review"}
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, root := processRuntimeFlow(t)
			g := f.HaveGroup("crew")
			_, err := db.SetAgentGroupDefaultCwd("crew", t.TempDir())
			require.NoError(t, err)
			_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "opus-review", Harness: "claude", Model: "opus"})
			require.NoError(t, err)
			spawns := recordSpawns(t)
			havePerformerAgent(t, f, "crew", dev, "alice", "dev", "idle")
			carol := havePerformerAgent(t, f, "crew", "erev-1111-2222-3333-4444", "carol", "reviewer", "idle")
			lena := havePerformerAgent(t, f, "crew", "elea-1111-2222-3333-4444", "lena", "lead", "working")
			runID := "run_editor_decides"
			startEditorRun(t, f, root, runID, editorAgentDecision(t, func(p *model.Performer) { tc.who(p, carol) }), g)

			reportLoopStep(t, f, dev, runID, "implement", 1)
			view := showPerformerRun(t, f, runID)
			decider, _, state := commandFor(t, view, "review")
			assert.Equal(t, db.ProcessDispatchDelivered, state)
			assert.Equal(t, tc.resolution, resolutionOf(t, view, "review"))
			if tc.decider != nil {
				assert.Equal(t, tc.decider(carol, lena), decider)
			}
			if tc.resolution == "spawned" {
				require.Len(t, spawns(), 1)
				assert.Equal(t, "opus", spawns()[0].Model, "spawned with the profile the editor picked")
			}
			assert.Empty(t, showLoopRun(t, f, runID).AwaitingDecisions, "nobody is asked: the agent decides")
			conv, err := db.CurrentConvForAgent(decider)
			require.NoError(t, err)
			if tc.resolution != "spawned" { // a spawned agent's brief is its startup prompt
				inbox := processInbox(t, conv)
				require.NotEmpty(t, inbox)
				assert.Contains(t, inbox[len(inbox)-1].Body, "Choose exactly one verdict: approved, changes, give-up.")
			}

			require.Equal(t, http.StatusOK, reportReview(t, f, conv, runID, 1, "approved"))
			after := showLoopRun(t, f, runID)
			require.Len(t, after.AwaitingDecisions, 1, "approved goes on to the operator's approval")
			assert.Equal(t, "approve", after.AwaitingDecisions[0].NodeID)
		})
	}
}

// Scenario: "changes" sends the work back to implement with the reviewer's
// reasoning, for at most the editor's three rounds; asked once more, the run
// fails, as the editor's "then fail the run" says.
func TestEditorAgentDecision_ChangesGoBackThenTheRunFails(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const dev, reviewer = "fdev-1111-2222-3333-4444", "frev-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", dev, "alice", "dev", "idle")
	havePerformerAgent(t, f, "crew", reviewer, "carol", "reviewer", "idle")
	const runID = "run_editor_changes"
	startEditorRun(t, f, root, runID, editorAgentDecision(t, func(p *model.Performer) { p.Role = "reviewer" }), g)

	for round := 1; round <= 3; round++ {
		reportLoopStep(t, f, dev, runID, "implement", round)
		require.Equal(t, http.StatusOK, reportReview(t, f, reviewer, runID, round, "changes"), "round %d", round)
		if round < 3 {
			view := showLoopRun(t, f, runID)
			require.Len(t, view.Commands, 1, "round %d", round)
			assert.Equal(t, [2]any{"implement", round + 1}, [2]any{view.Commands[0].NodeID, view.Commands[0].Attempt})
			devInbox := processInbox(t, dev)
			assert.Contains(t, devInbox[len(devInbox)-1].Body, "changes because", "the reasoning travels with the round")
		}
	}
	assert.Equal(t, engine.RunFailed, showLoopRun(t, f, runID).Status, "out of rounds, the run fails")
}
