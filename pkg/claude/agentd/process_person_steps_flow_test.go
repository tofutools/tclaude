package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Steps a person performs: a task node whose performer is kind human. It
// waits on the person exactly as a decision does, and they finish it with
// tclaude process decide: a choice, a note and the outputs it asks for.

type personStepView struct {
	ID       string           `json:"id"`
	Status   engine.RunStatus `json:"status"`
	Action   string           `json:"action"`
	Commands []struct {
		NodeID string             `json:"nodeId"`
		Kind   engine.CommandKind `json:"kind"`
		State  string             `json:"state"`
	} `json:"commands"`
	AwaitingDecisions []struct {
		NodeID   string   `json:"nodeId"`
		Kind     string   `json:"kind"`
		Verdicts []string `json:"verdicts"`
		Passing  []string `json:"passing"`
		Attempt  int      `json:"attempt"`
		Ask      string   `json:"ask"`
		Prompt   string   `json:"prompt"`
		Outputs  []string `json:"outputs"`
	} `json:"awaitingDecisions"`
	Failure *engine.StepFailure `json:"failure"`
}

func showPersonStepRun(t *testing.T, f *testharness.Flow, runID string) personStepView {
	t.Helper()
	agentd.WaitForProcessDispatchForTest()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view personStepView
	testharness.DecodeJSON(t, rec, &view)
	return view
}

func decidePersonStep(t *testing.T, f *testharness.Flow, runID string, body map[string]any) int {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+runID+"/decide", body)
	agentd.WaitForProcessDispatchForTest()
	return rec.Code
}

// personStepTemplate is start → pack (a person) → ship (an agent) → end.
func personStepTemplate(id string, person model.Performer, captures []string) *model.Template {
	ship := devPerformer()
	ship.Prompt = "Ship it with tracking {{ outputs.tracking }}."
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "start",
		Params: map[string]model.Param{"customer": {Type: "string", Default: "nobody"}},
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "pack"}},
			"pack":  {Type: model.NodeTypeTask, Performer: &person, Captures: captures, Next: model.Next{"next": "ship"}},
			"ship":  {Type: model.NodeTypeTask, Performer: &ship, Next: model.Next{"next": "end"}},
			"end":   {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

// Scenario: the person's step opens with its ask, the run waits on them, a
// "done" without the output it asks for is refused, and the output they fill
// in reaches the next agent's prompt.
func TestProcessPersonStep_DoneWithOutputFeedsNextStep(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("stall")
	const worker = "pstp-1111-2222-3333-4444"
	havePerformerAgent(t, f, "stall", worker, "alice", "dev", "idle")
	person := model.Performer{Kind: model.PerformerHuman, Ask: "Pack the order for {{ params.customer }}.",
		Prompt: "Use the small box."}
	putProcessRuntimeTemplate(t, root, personStepTemplate("person-done", person, []string{"tracking"}))
	t.Cleanup(agentd.SetProcessRunScopeForTest("run_person_done", agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID}))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_person_done", "templateId": "person-done", "authorizeProgramProfiles": []string{},
		"params": map[string]string{"customer": "Ada"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	view := showPersonStepRun(t, f, "run_person_done")
	assert.Equal(t, "awaiting_decision", view.Action)
	require.Len(t, view.Commands, 1)
	assert.Equal(t, engine.CommandHuman, view.Commands[0].Kind)
	assert.Equal(t, "awaiting_person", view.Commands[0].State)
	require.Len(t, view.AwaitingDecisions, 1)
	step := view.AwaitingDecisions[0]
	assert.Equal(t, "pack", step.NodeID)
	assert.Equal(t, "task", step.Kind)
	assert.Equal(t, []string{"done", "failed"}, step.Verdicts)
	assert.Equal(t, []string{"done"}, step.Passing)
	assert.Equal(t, 1, step.Attempt)
	assert.Equal(t, "Pack the order for Ada.", step.Ask)
	assert.Equal(t, "Use the small box.", step.Prompt)
	assert.Equal(t, []string{"tracking"}, step.Outputs)
	assert.Empty(t, processInbox(t, worker), "nothing goes to an agent while the person works")

	assert.Equal(t, http.StatusUnprocessableEntity,
		decidePersonStep(t, f, "run_person_done", map[string]any{"nodeId": "pack", "verdict": "done"}),
		"a done step must fill in its output")
	assert.Equal(t, http.StatusUnprocessableEntity,
		decidePersonStep(t, f, "run_person_done", map[string]any{"nodeId": "pack", "verdict": "shipped"}),
		"a choice the step does not offer is refused")
	assert.Equal(t, http.StatusConflict,
		decidePersonStep(t, f, "run_person_done", map[string]any{"nodeId": "pack", "verdict": "done", "attempt": 2,
			"outputs": map[string]string{"tracking": "TRK-1"}}),
		"a stale attempt is refused")
	assert.Equal(t, "awaiting_decision", showPersonStepRun(t, f, "run_person_done").Action, "a refusal records nothing")

	assert.Equal(t, http.StatusAccepted, decidePersonStep(t, f, "run_person_done", map[string]any{
		"nodeId": "pack", "verdict": "done", "evidence": "packed in the small box",
		"outputs": map[string]string{"tracking": "TRK-42"},
	}))
	inbox := processInbox(t, worker)
	require.Len(t, inbox, 1, "the next step goes to the agent")
	assert.Contains(t, inbox[0].Body, "TRK-42", "the person's output reaches the next prompt")

	kinds, events := performerEventKinds(t, f, "run_person_done")
	assert.Contains(t, kinds, "human_prepared")
	require.Contains(t, kinds, "human_reported")
	for _, event := range events {
		if event.Kind == "human_reported" {
			assert.Equal(t, "human:operator", event.Actor)
			assert.Contains(t, string(event.Payload), `"choice":"done"`)
		}
	}
	ok := reportAs(t, f, worker, map[string]any{"outcome": "succeeded"})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Equal(t, engine.RunCompleted, showPersonStepRun(t, f, "run_person_done").Status)
}

// Scenario: the person finishes the step with a choice that fails it. With
// no retries the run stops and names the step, with their note as the reason.
func TestProcessPersonStep_FailingChoiceFailsRunNamingStep(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("stall")
	person := model.Performer{Kind: model.PerformerHuman, Ask: "Pack the order.",
		Choices: []string{"packed", "out of stock"}, ChoiceOutcomes: map[string]string{"packed": "pass", "out of stock": "fail"}}
	startPerformerRun(t, f, root, "run_person_fail", personStepTemplate("person-fail", person, nil),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})
	view := showPersonStepRun(t, f, "run_person_fail")
	require.Len(t, view.AwaitingDecisions, 1)
	assert.Equal(t, []string{"packed", "out of stock"}, view.AwaitingDecisions[0].Verdicts)
	assert.Equal(t, []string{"packed"}, view.AwaitingDecisions[0].Passing)

	assert.Equal(t, http.StatusAccepted, decidePersonStep(t, f, "run_person_fail", map[string]any{
		"nodeId": "pack", "verdict": "out of stock", "evidence": "no pears left",
	}))
	done := showPersonStepRun(t, f, "run_person_fail")
	assert.Equal(t, engine.RunFailed, done.Status)
	require.NotNil(t, done.Failure)
	assert.Equal(t, "pack", done.Failure.NodeID)
	assert.Equal(t, "no pears left", done.Failure.Reason)
}

// Scenario: a run paused before the step opens holds it; resumed, it opens.
// Cancelling the run then withdraws the open step.
func TestProcessPersonStep_PauseHoldsItCancelWithdrawsIt(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("stall")
	const worker = "pstq-1111-2222-3333-4444"
	havePerformerAgent(t, f, "stall", worker, "alice", "dev", "idle")
	person := model.Performer{Kind: model.PerformerHuman, Ask: "Check the shipment."}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "person-pause", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "work"}},
			"work":  {Type: model.NodeTypeTask, Performer: new(devPerformer()), Next: model.Next{"next": "check"}},
			"check": {Type: model.NodeTypeTask, Performer: &person, Next: model.Next{"next": "end"}},
			"end":   {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
	startPerformerRun(t, f, root, "run_person_pause", tmpl, agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})
	rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_person_pause/pause", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	ok := reportAs(t, f, worker, map[string]any{"outcome": "succeeded"})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Empty(t, showPersonStepRun(t, f, "run_person_pause").AwaitingDecisions, "a paused run opens no step")

	rec = processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_person_pause/resume", map[string]any{})
	require.Less(t, rec.Code, 300, rec.Body.String())
	view := showPersonStepRun(t, f, "run_person_pause")
	require.Len(t, view.AwaitingDecisions, 1, "resumed, the step opens")
	assert.Equal(t, "check", view.AwaitingDecisions[0].NodeID)

	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	assert.Len(t, showPersonStepRun(t, f, "run_person_pause").AwaitingDecisions, 1, "the open step survives a restart")

	rec = processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_person_pause/cancel", map[string]any{})
	require.Less(t, rec.Code, 300, rec.Body.String())
	canceled := showPersonStepRun(t, f, "run_person_pause")
	assert.Equal(t, engine.RunCanceled, canceled.Status)
	assert.Empty(t, canceled.Commands)
	assert.Empty(t, canceled.AwaitingDecisions)
	kinds, _ := performerEventKinds(t, f, "run_person_pause")
	assert.Contains(t, kinds, "human_withdrawn")
	assert.Equal(t, http.StatusConflict, decidePersonStep(t, f, "run_person_pause",
		map[string]any{"nodeId": "check", "verdict": "done"}), "a withdrawn step cannot be finished")
}

// Scenario: only the operator performs a person's step today, so a template
// naming anyone else is refused when the run starts.
func TestProcessPersonStep_OtherAssigneeIsRefused(t *testing.T) {
	f, root := processRuntimeFlow(t)
	person := model.Performer{Kind: model.PerformerHuman, Ask: "Pack it.", Assignee: "bob"}
	putProcessRuntimeTemplate(t, root, personStepTemplate("person-bob", person, nil))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_person_bob", "templateId": "person-bob", "authorizeProgramProfiles": []string{},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, created.Code, created.Body.String())
	assert.Contains(t, created.Body.String(), "only the operator performs a person's step today")
}

// Scenario: a program step finishes straight into a person's step. The step
// opens and waits on the person; it is never run as a program.
func TestProcessPersonStep_AfterProgramWaitsOnThePerson(t *testing.T) {
	f, root := processRuntimeFlow(t)
	person := model.Performer{Kind: model.PerformerHuman, Ask: "Check the build."}
	putProcessRuntimeTemplate(t, root, &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "person-after-program", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "build"}},
			"build": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerProgram, Run: "true"},
				Next: model.Next{"next": "check"}},
			"check": {Type: model.NodeTypeTask, Performer: &person, Next: model.Next{"next": "end"}},
			"end":   {Type: model.NodeTypeEnd, Result: "success"},
		},
	})
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_person_after_program", "templateId": "person-after-program", "authorizeProgramProfiles": []string{""},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessRunRuntimeForTest()

	view := showPersonStepRun(t, f, "run_person_after_program")
	assert.Equal(t, engine.RunRunning, view.Status)
	require.Len(t, view.AwaitingDecisions, 1, "%+v", view)
	assert.Equal(t, "check", view.AwaitingDecisions[0].NodeID)
	assert.Equal(t, 1, view.AwaitingDecisions[0].Attempt, "the step was not attempted as a program")

	assert.Equal(t, http.StatusAccepted, decidePersonStep(t, f, "run_person_after_program",
		map[string]any{"nodeId": "check", "verdict": "done"}))
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, engine.RunCompleted, showPersonStepRun(t, f, "run_person_after_program").Status)
}
