package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func startEpicRun(t *testing.T, f *testharness.Flow, root, runID string) {
	t.Helper()
	putProcessRuntimeTemplate(t, root, epicTemplate())
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": runID, "templateId": "epic", "group": "crew", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	showOversightRun(t, f, runID)
}

func lastInboxSubject(t *testing.T, conv string) string {
	t.Helper()
	inbox := processInbox(t, conv)
	require.NotEmpty(t, inbox)
	return inbox[len(inbox)-1].Subject + "\n" + inbox[len(inbox)-1].Body
}

// Scenario: the API a step needs was removed under it. The run's overseer
// (the group's lead) parks the step with that reason: its performer is told
// to stop, a late report of that attempt is refused, and the step waits as
// blocked. A performer of the run cannot park it. The operator retries it and
// the step is handed out again as attempt 2.
func TestProcessSteer_OverseerParksAStepThatIsOutAndTheOperatorRetriesIt(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	advance := moveOversightClock(t)
	const leadConv, devConv = "sbl1-1111-2222-3333-4444", "sbd1-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "working")
	havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	startEpicRun(t, f, root, "run_sb1")
	require.Len(t, processInbox(t, devConv), 1, "the step is handed out")
	var shown struct {
		Commands []struct {
			NodeID   string `json:"nodeId"`
			Parkable bool   `json:"parkable"`
		} `json:"commands"`
	}
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_sb1", nil)
	testharness.DecodeJSON(t, rec, &shown)
	require.Len(t, shown.Commands, 1)
	assert.True(t, shown.Commands[0].Parkable, "the run view says the step can be parked")

	block := map[string]any{"nodeId": "work", "attempt": 1, "reason": "The API this step needs was removed."}
	refused := agentReq(t, f, devConv, http.MethodPost, "/v1/process/runs/run_sb1/block", block)
	assert.Equal(t, http.StatusForbidden, refused.Code, "a performer cannot park the run's steps")
	stale := agentReq(t, f, leadConv, http.MethodPost, "/v1/process/runs/run_sb1/block", map[string]any{"nodeId": "work", "attempt": 2, "reason": "x"})
	assert.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())

	parked := agentReq(t, f, leadConv, http.MethodPost, "/v1/process/runs/run_sb1/block", block)
	require.Equal(t, http.StatusAccepted, parked.Code, parked.Body.String())
	agentd.WaitForProcessDispatchForTest()
	view := showOversightRun(t, f, "run_sb1")
	require.Len(t, view.Blocked, 1)
	assert.Equal(t, "work", view.Blocked[0].NodeID)
	assert.Contains(t, lastInboxSubject(t, devConv), "no longer needs your report", "the performer is released")
	late := reportAs(t, f, devConv, map[string]any{"runId": "run_sb1", "nodeId": "work", "attempt": 1, "outcome": "succeeded", "note": "done anyway"})
	assert.NotEqual(t, http.StatusOK, late.Code, "the parked attempt's report is stale")

	events := processRunEventPage(t, f, "run_sb1")
	var marked bool
	for _, e := range events.Events {
		marked = marked || (e.Kind == "block_marked" && strings.Contains(e.Actor, "agent:"))
	}
	assert.True(t, marked, "the log says who parked the step")

	// Left parked past its patience, the engine says who parked it rather
	// than that its retries are spent.
	advance(time.Hour)
	agentd.SweepProcessStuckSignalsForTest()
	raised := engineConcerns(showOversightRun(t, f, "run_sb1"))
	require.Len(t, raised, 1)
	assert.Contains(t, raised[0].Reason, "was parked by lena")

	resolveBlocked(t, f, "run_sb1", map[string]any{"nodeId": "work", "attempt": 1, "action": "retry", "note": "API restored"})
	agentd.WaitForProcessDispatchForTest()
	inbox := processInbox(t, devConv)
	assert.Contains(t, inbox[len(inbox)-1].Body, "attempt 2", "the step is handed out again")
	assert.Contains(t, inbox[len(inbox)-1].Body, "retried with this note:\n\nAPI restored",
		"the operator's answer to the park reason reaches the performer with the retried step")
}

// Scenario: the operator gives a run up while its first step is out. The
// performer is told to stop and the run ends canceled. The overseer cannot
// cancel; it raises a concern, and the operator's card always offers the
// cancel, which ends a second run the same way.
func TestProcessSteer_CancelEndsTheRunFromTheAPIAndFromTheCard(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const leadConv, devConv = "scl1-1111-2222-3333-4444", "scd1-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "working")
	havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	startEpicRun(t, f, root, "run_sc1")

	refused := agentReq(t, f, leadConv, http.MethodPost, "/v1/process/runs/run_sc1/cancel", map[string]any{"note": "no"})
	assert.Equal(t, http.StatusForbidden, refused.Code, "an overseer steers a run but does not end it")

	canceled := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_sc1/cancel", map[string]any{"note": "superseded by run_sc2"})
	require.Equal(t, http.StatusAccepted, canceled.Code, canceled.Body.String())
	agentd.WaitForProcessDispatchForTest()
	assert.Equal(t, engine.RunCanceled, showOversightRun(t, f, "run_sc1").Status)
	assert.Contains(t, lastInboxSubject(t, devConv), "Stop working on this process node")
	again := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_sc1/cancel", map[string]any{})
	assert.NotEqual(t, http.StatusAccepted, again.Code, "an ended run cannot be canceled again")

	startEpicRun(t, f, root, "run_sc2")
	concern := raiseConcernAs(t, f, devConv, "run_sc2", "work", "The epic was dropped upstream.", http.StatusCreated)
	concern = answerConcernAs(t, f, leadConv, "run_sc2", concern.ID, "escalate", "Needs your call.", http.StatusOK)
	assert.Equal(t, db.ProcessConcernLevelOperator, concern.Level)
	require.NotZero(t, concern.MessageID)
	assert.Contains(t, concern.Choices, "Cancel the run", "the card always offers the cancel")
	pickOnCard(t, concern.MessageID, "Cancel the run", "dropped", http.StatusOK)
	agentd.WaitForProcessDispatchForTest()
	view := showOversightRun(t, f, "run_sc2")
	assert.Equal(t, engine.RunCanceled, view.Status)
	assert.Equal(t, "cancel", view.concern(t, concern.ID).Resolution)
}

// Scenario: the review finds the spec was wrong. The operator sends the run
// back to the work step while the ship step is out: ship's performer is told
// to stop, work is handed out again as attempt 2, and the review's notes,
// reported before the send-back, no longer feed the work's prompt. An
// overseer cannot send back; a step that has not run cannot be sent back to.
func TestProcessSteer_SendBackRerunsAStepAndDropsOutputsReportedAfterIt(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const leadConv, devConv = "sdl1-1111-2222-3333-4444", "sdd1-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "working")
	havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	dev := func(prompt string, captures ...string) model.Node {
		node := model.Node{Performer: agentStep(prompt, func(p *model.Performer) { p.Profile = "dev" })}
		node.Captures = captures
		return node
	}
	tmpl := chainTemplate("rework", []string{"work", "review", "ship"}, map[string]model.Node{
		"work":   dev("Build it. Review notes so far: {{ outputs.notes }}"),
		"review": dev("Review it.", "notes"),
		"ship":   dev("Ship it."),
	})
	putProcessRuntimeTemplate(t, root, tmpl)
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_sd1", "templateId": "rework", "group": "crew", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessDispatchForTest()
	reportStep(t, f, devConv, "run_sd1", "work", 1, nil)
	agentd.WaitForProcessDispatchForTest()
	reportStep(t, f, devConv, "run_sd1", "review", 1, map[string]any{"outputs": map[string]any{"notes": "the spec contradicts itself"}})
	agentd.WaitForProcessDispatchForTest()
	assert.Contains(t, lastInboxSubject(t, devConv), "ship (attempt 1)")

	refused := agentReq(t, f, leadConv, http.MethodPost, "/v1/process/runs/run_sd1/send-back", map[string]any{"nodeId": "work"})
	assert.Equal(t, http.StatusForbidden, refused.Code, "an overseer raises a concern; the operator sends back")
	notYet := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_sd1/send-back", map[string]any{"nodeId": "ship"})
	assert.Equal(t, http.StatusConflict, notYet.Code, notYet.Body.String())

	// The operator's card offers going back to the concern's finished step.
	concern := raiseConcernAs(t, f, devConv, "run_sd1", "work", "The spec this was built on contradicts itself.", http.StatusCreated)
	concern = answerConcernAs(t, f, leadConv, "run_sd1", concern.ID, "escalate", "Needs a rework call.", http.StatusOK)
	require.NotZero(t, concern.MessageID)
	assert.Contains(t, concern.Choices, "Send back to work")
	pickOnCard(t, concern.MessageID, "Send back to work", "spec fixed", http.StatusOK)
	agentd.WaitForProcessDispatchForTest()
	assert.Equal(t, "send_back", showOversightRun(t, f, "run_sd1").concern(t, concern.ID).Resolution)
	inbox := processInbox(t, devConv)
	var released, again string
	for _, m := range inbox {
		if strings.Contains(m.Body, "no longer needs your report") && strings.Contains(m.Body, "ship") {
			released = m.Body
		}
		if strings.Contains(m.Subject, "work (attempt 2)") {
			again = m.Body
		}
	}
	assert.NotEmpty(t, released, "ship's performer is released")
	require.NotEmpty(t, again, "work is handed out again")
	assert.NotContains(t, again, "the spec contradicts itself", "notes reported after the step no longer stand")
	assert.Contains(t, again, "none reported yet")

	events := processRunEventPage(t, f, "run_sd1")
	var sent bool
	for _, e := range events.Events {
		sent = sent || (e.Kind == "sent_back" && e.NodeID == "work")
	}
	assert.True(t, sent, "the log records the send-back")
}
