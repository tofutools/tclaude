package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// ruleMergeTemplate: dev builds and reports its risk, ci may fail, and a rule
// decides: CI failed → back to dev (two rounds, then give up), merge allowed
// and CI passed and the risk is not high → merge, else hold.
func ruleMergeTemplate() *model.Template {
	agentTask := func(prompt, next string) model.Node {
		return model.Node{Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Role: "dev", Prompt: prompt},
			Next: model.Next{model.DefaultOutcome: next}}
	}
	dev := agentTask("Build the change.", "ci")
	dev.Captures = []string{"risk"}
	ci := agentTask("Run CI.", "gate")
	ci.MayFail = true
	return &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: "merge-rule", Start: "start",
		Params: map[string]model.Param{"merge": {Type: "string"}},
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "dev"}},
			"dev":   dev,
			"ci":    ci,
			"gate": {Type: model.NodeTypeDecision, Name: "Merge?", Performer: &model.Performer{Kind: model.PerformerRule,
				Cases: []model.RuleCase{
					{When: `steps.ci.result == failed`, Then: "fix"},
					{When: `params.merge == yes and steps.ci.result == succeeded and outputs.risk != high`, Then: "merge"},
				}, Else: "hold"},
				Next:  model.Next{"fix": "dev", "merge": "merge", "hold": "held", "give-up": "gave-up"},
				Loops: map[string]model.LoopBound{"fix": {MaxIterations: 3, OnExhausted: "give-up"}}},
			"merge":   agentTask("Merge it.", "end"),
			"end":     {Type: model.NodeTypeEnd},
			"held":    {Type: model.NodeTypeEnd, Result: "canceled"},
			"gave-up": {Type: model.NodeTypeEnd, Result: "failed"},
		}}
}

type ruleEvent struct {
	Verdict string `json:"verdict"`
	Rule    struct {
		Case int               `json:"case"`
		When string            `json:"when"`
		Read map[string]string `json:"read"`
	} `json:"rule"`
}

func ruleDecisions(t *testing.T, f *testharness.Flow, runID string) []ruleEvent {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID+"/events", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page processRuntimeEventPage
	testharness.DecodeJSON(t, rec, &page)
	var out []ruleEvent
	for _, event := range page.Events {
		if event.Kind != "decision_recorded" || event.Actor != "rule" {
			continue
		}
		var decided ruleEvent
		require.NoError(t, json.Unmarshal(event.Payload, &decided))
		out = append(out, decided)
	}
	return out
}

func startRuleRun(t *testing.T, f *testharness.Flow, root, runID, merge string) {
	t.Helper()
	putProcessRuntimeTemplate(t, root, ruleMergeTemplate())
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": runID, "templateId": "merge-rule", "group": "crew", "authorizeProgramProfiles": []string{},
		"params": map[string]string{"merge": merge},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessDispatchForTest()
}

// Scenario: CI fails, so the rule sends the work back to dev without asking
// anyone; CI then passes and, with merging allowed, the rule merges. Each
// choice is an event by the rule, with the case that held and what it read.
func TestProcessRule_CIFailsThenPassesAndMerges(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const dev = "rdev-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", dev, "dana", "dev", "idle")
	const runID = "run_rule_merge"
	startRuleRun(t, f, root, runID, "yes")

	report := func(node string, attempt int, outcome string, extra map[string]any) {
		t.Helper()
		body := map[string]any{"runId": runID, "nodeId": node, "attempt": attempt, "outcome": outcome, "note": node + " " + outcome}
		for k, v := range extra {
			body[k] = v
		}
		rec := reportAs(t, f, dev, body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	report("dev", 1, "succeeded", map[string]any{"outputs": map[string]string{"risk": "low"}})
	report("ci", 1, "failed", nil)
	// CI failed, yet the run goes on: the rule sent the work back to dev.
	require.Equal(t, engine.RunRunning, showLoopRun(t, f, runID).Status)
	decided := ruleDecisions(t, f, runID)
	require.Len(t, decided, 1)
	require.Equal(t, "fix", decided[0].Verdict)
	require.Equal(t, `steps.ci.result == failed`, decided[0].Rule.When)
	require.Equal(t, map[string]string{"steps.ci.result": "failed"}, decided[0].Rule.Read)

	report("dev", 2, "succeeded", map[string]any{"outputs": map[string]string{"risk": "low"}})
	report("ci", 2, "succeeded", nil)
	decided = ruleDecisions(t, f, runID)
	require.Len(t, decided, 2)
	require.Equal(t, "merge", decided[1].Verdict)
	require.Equal(t, 1, decided[1].Rule.Case)
	require.Equal(t, map[string]string{"steps.ci.result": "succeeded", "params.merge": "yes", "outputs.risk": "low"}, decided[1].Rule.Read)

	report("merge", 1, "succeeded", nil)
	require.Equal(t, engine.RunCompleted, showLoopRun(t, f, runID).Status)
}

// Scenario: without the param, the same passing CI does not merge: the rule
// takes its else outcome and the run ends held.
func TestProcessRule_WithoutTheParamItHolds(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const dev = "rdev-1111-2222-3333-4445"
	havePerformerAgent(t, f, "crew", dev, "dana", "dev", "idle")
	const runID = "run_rule_hold"
	startRuleRun(t, f, root, runID, "no")
	rec := reportAs(t, f, dev, map[string]any{"runId": runID, "nodeId": "dev", "attempt": 1, "outcome": "succeeded", "outputs": map[string]string{"risk": "low"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = reportAs(t, f, dev, map[string]any{"runId": runID, "nodeId": "ci", "attempt": 1, "outcome": "succeeded", "note": "green"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	decided := ruleDecisions(t, f, runID)
	require.Len(t, decided, 1)
	require.Equal(t, "hold", decided[0].Verdict)
	require.Equal(t, -1, decided[0].Rule.Case)
	require.Equal(t, engine.RunCanceled, showLoopRun(t, f, runID).Status)
}
