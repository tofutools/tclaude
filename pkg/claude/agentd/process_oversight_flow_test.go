package agentd_test

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tk_e2fa2bd0: run oversight. A run has an overseer (set on it, else its
// group's lead, else whoever started it). Anyone in the run raises concerns;
// a concern climbs overseer → the group's escalation agent → operator, and an
// unanswered holder is passed over while the run pauses. The operator answers
// a decision card whose pick the daemon applies and posts to the overseer.

type oversightRunView struct {
	ID       string           `json:"id"`
	Status   engine.RunStatus `json:"status"`
	Action   string           `json:"action"`
	Paused   *pauseWire       `json:"paused"`
	Overseer *struct {
		Ref               string `json:"ref"`
		AgentID           string `json:"agentId"`
		Name              string `json:"name"`
		Via               string `json:"via"`
		EscalationAgentID string `json:"escalationAgentId"`
	} `json:"overseer"`
	Concerns []oversightConcernWire `json:"concerns"`
	Blocked  []struct {
		NodeID string `json:"nodeId"`
	} `json:"blocked"`
}

type oversightConcernWire struct {
	ID         int64    `json:"id"`
	NodeID     string   `json:"nodeId"`
	Source     string   `json:"source"`
	RaisedBy   string   `json:"raisedBy"`
	Reason     string   `json:"reason"`
	State      string   `json:"state"`
	Level      string   `json:"level"`
	Holder     string   `json:"holder"`
	HolderName string   `json:"holderName"`
	MessageID  int64    `json:"messageId"`
	Choices    []string `json:"choices"`
	Resolution string   `json:"resolution"`
	ResolvedBy string   `json:"resolvedBy"`
	Note       string   `json:"note"`
}

func showOversightRun(t *testing.T, f *testharness.Flow, runID string) oversightRunView {
	t.Helper()
	agentd.WaitForProcessDispatchForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view oversightRunView
	testharness.DecodeJSON(t, rec, &view)
	return view
}

func (v oversightRunView) concern(t *testing.T, id int64) oversightConcernWire {
	t.Helper()
	for _, c := range v.Concerns {
		if c.ID == id {
			return c
		}
	}
	require.Failf(t, "concern not in the run view", "concern #%d", id)
	return oversightConcernWire{}
}

func raiseConcernAs(t *testing.T, f *testharness.Flow, conv, runID, node, reason string, want int) oversightConcernWire {
	t.Helper()
	rec := agentReq(t, f, conv, http.MethodPost, "/v1/process/runs/"+runID+"/concerns", map[string]any{"reason": reason, "nodeId": node})
	require.Equal(t, want, rec.Code, rec.Body.String())
	var response struct {
		Concern oversightConcernWire `json:"concern"`
	}
	if want == http.StatusCreated {
		testharness.DecodeJSON(t, rec, &response)
	}
	return response.Concern
}

func answerConcernAs(t *testing.T, f *testharness.Flow, conv, runID string, id int64, action, note string, want int) oversightConcernWire {
	t.Helper()
	path := "/v1/process/runs/" + runID + "/concerns/" + strconv.FormatInt(id, 10) + "/answer"
	rec := agentReq(t, f, conv, http.MethodPost, path, map[string]any{"action": action, "note": note})
	require.Equal(t, want, rec.Code, rec.Body.String())
	var response struct {
		Concern oversightConcernWire `json:"concern"`
	}
	if want == http.StatusOK {
		testharness.DecodeJSON(t, rec, &response)
	}
	return response.Concern
}

// oversightInbox is the [process] mail a conv got about concerns.
func oversightInbox(t *testing.T, conv string) []*db.AgentMessage {
	t.Helper()
	var out []*db.AgentMessage
	for _, m := range processInbox(t, conv) {
		if strings.Contains(m.Subject, "concern #") {
			out = append(out, m)
		}
	}
	return out
}

// pickOnCard answers the operator's decision card through the dashboard, as
// the Messages tab does.
func pickOnCard(t *testing.T, messageID int64, choice, note string, want int) {
	t.Helper()
	m, err := db.GetHumanMessage(messageID)
	require.NoError(t, err)
	require.NotNil(t, m)
	index := slices.Index(m.Options, choice)
	require.GreaterOrEqual(t, index, 0, "choice %q is not on the card %v", choice, m.Options)
	rec := testharness.Serve(dashMessageMux(t), testharness.JSONRequest(t, http.MethodPost, "/api/human-messages/reply",
		map[string]any{"id": messageID, "choice": index + 1, "body": note}))
	require.Equal(t, want, rec.Code, rec.Body.String())
}

// moveOversightClock returns a clock the test moves, installed for the test.
func moveOversightClock(t *testing.T) func(time.Duration) {
	t.Helper()
	var mu sync.Mutex
	now := time.Now()
	t.Cleanup(agentd.SetProcessOversightClockForTest(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}))
	return func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
}

// Scenario: a performer raises a concern; it goes to the group's lead, who
// oversees the run, pauses it and escalates. The group has no escalation
// agent, so the operator gets a decision card, and the run shows as needing
// the operator. The operator picks Resume: the run goes on, the lead is told,
// and the run log has every step. Someone outside the run cannot raise one.
func TestProcessOversight_ConcernEscalatesFromOverseerToOperatorWhoResumes(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const leadConv, aliceConv, outsiderConv = "ovld-1111-2222-3333-4444", "ovaa-1111-2222-3333-4444", "ovou-1111-2222-3333-4444"
	lead := havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "working")
	havePerformerAgent(t, f, "crew", aliceConv, "alice", "dev", "idle")
	havePerformerAgent(t, f, "", outsiderConv, "otto", "", "idle")
	putProcessRuntimeTemplate(t, root, epicTemplate())

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_ov", "templateId": "epic", "group": "crew", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	run := showOversightRun(t, f, "run_ov")
	require.NotNil(t, run.Overseer)
	assert.Equal(t, lead, run.Overseer.AgentID)
	assert.Equal(t, "lead", run.Overseer.Via, "the group's lead oversees a run nobody gave an overseer")
	assert.Empty(t, run.Concerns)

	raiseConcernAs(t, f, outsiderConv, "run_ov", "work", "not my run", http.StatusForbidden)

	concern := raiseConcernAs(t, f, aliceConv, "run_ov", "work", "The spec contradicts itself on retries.", http.StatusCreated)
	assert.Equal(t, db.ProcessConcernLevelOverseer, concern.Level)
	assert.Equal(t, "agent:"+lead, concern.Holder)
	assert.Equal(t, "lena", concern.HolderName)
	mail := oversightInbox(t, leadConv)
	require.Len(t, mail, 1, "the overseer is told")
	assert.Contains(t, mail[0].Body, "The spec contradicts itself on retries.")
	assert.Contains(t, mail[0].Body, "tclaude process answer-concern run_ov")

	// Only the holder (or process.runs.manage) answers it.
	answerConcernAs(t, f, aliceConv, "run_ov", concern.ID, "continue", "", http.StatusForbidden)

	// The overseer may pause the run it oversees without process.runs.manage.
	rec := agentReq(t, f, leadConv, http.MethodPost, "/v1/process/runs/run_ov/pause", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = agentReq(t, f, aliceConv, http.MethodPost, "/v1/process/runs/run_ov/resume", map[string]any{})
	assert.Equal(t, http.StatusForbidden, rec.Code, "a performer is not the overseer: %s", rec.Body.String())

	moved := answerConcernAs(t, f, leadConv, "run_ov", concern.ID, "escalate", "Needs a product call.", http.StatusOK)
	assert.Equal(t, "open", moved.State)
	assert.Equal(t, db.ProcessConcernLevelOperator, moved.Level, "no escalation agent: straight to the operator")
	require.NotZero(t, moved.MessageID)
	assert.Equal(t, []string{"Resume", "Reassign", "Keep paused"}, moved.Choices)

	card, err := db.GetHumanMessage(moved.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "crew", card.GroupName)
	assert.Equal(t, "run_ov", card.ProcessRunID)
	assert.Contains(t, card.Body, "The spec contradicts itself on retries.")
	assert.Contains(t, card.Body, "Needs a product call.")
	assert.Contains(t, card.Body, "posted to its overseer, lena")

	_, view := epicContext(t, f, "run_ov")
	require.Len(t, view.Groups, 1)
	assert.Contains(t, view.Groups[0].NeedsYou, "run_ov", "a concern with the operator puts the run in needs-you")

	pickOnCard(t, moved.MessageID, "Resume", "Retries: at most three.", http.StatusOK)
	run = showOversightRun(t, f, "run_ov")
	assert.Nil(t, run.Paused, "Resume releases the overseer's pause")
	settled := run.concern(t, concern.ID)
	assert.Equal(t, "resolved", settled.State)
	assert.Equal(t, "resume", settled.Resolution)
	assert.Equal(t, "human:operator", settled.ResolvedBy)
	assert.Equal(t, "Retries: at most three.", settled.Note)
	card, err = db.GetHumanMessage(moved.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "Resume", card.Decision)
	pickOnCard(t, moved.MessageID, "Keep paused", "", http.StatusConflict)

	mail = oversightInbox(t, leadConv)
	require.Len(t, mail, 2, "the overseer hears the operator's pick")
	assert.Contains(t, mail[1].Body, "settled by the operator: resume")
	assert.Contains(t, mail[1].Body, "Retries: at most three.")

	kinds, _ := performerEventKinds(t, f, "run_ov")
	for _, kind := range []string{db.ProcessEventConcernRaised, db.ProcessEventConcernEscalated, db.ProcessEventConcernResolved, "run_paused", "run_resumed"} {
		assert.Equal(t, 1, countKind(kinds, kind), "the run log records %s once: %v", kind, kinds)
	}
	assert.Less(t, slices.Index(kinds, db.ProcessEventConcernRaised), slices.Index(kinds, db.ProcessEventConcernEscalated))
	assert.Less(t, slices.Index(kinds, db.ProcessEventConcernEscalated), slices.Index(kinds, db.ProcessEventConcernResolved))
}

// Scenario: the overseer does not answer in time. The run pauses and the
// concern passes to the group's escalation agent (a member with the role
// "escalation" holding process.runs.manage); it does not answer either, and
// the concern reaches the operator, who keeps the run paused.
func TestProcessOversight_UnansweredHoldersArePassedOverAndTheRunPauses(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	advance := moveOversightClock(t)
	const leadConv, escConv, aliceConv = "ovl2-1111-2222-3333-4444", "ove2-1111-2222-3333-4444", "ova2-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "working")
	esc := havePerformerAgent(t, f, "crew", escConv, "esme", "escalation", "idle")
	require.NoError(t, db.SetAgentPermissionOverride(escConv, agentd.PermProcessRunsManage, db.PermEffectGrant, "test"))
	havePerformerAgent(t, f, "crew", aliceConv, "alice", "dev", "idle")
	putProcessRuntimeTemplate(t, root, epicTemplate())

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_ov2", "templateId": "epic", "group": "crew", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	run := showOversightRun(t, f, "run_ov2")
	require.NotNil(t, run.Overseer)
	assert.Equal(t, esc, run.Overseer.EscalationAgentID)

	concern := raiseConcernAs(t, f, aliceConv, "run_ov2", "work", "CI is red on main.", http.StatusCreated)
	require.Equal(t, db.ProcessConcernLevelOverseer, concern.Level)

	// Not yet due: nothing moves.
	agentd.SweepProcessConcernsForTest()
	assert.Nil(t, showOversightRun(t, f, "run_ov2").Paused)

	advance(agentd.ProcessConcernPatienceForTest() + time.Second)
	agentd.SweepProcessConcernsForTest()
	run = showOversightRun(t, f, "run_ov2")
	require.NotNil(t, run.Paused, "an unanswered overseer pauses the run")
	assert.Equal(t, "tclaude:oversight", run.Paused.By)
	passed := run.concern(t, concern.ID)
	assert.Equal(t, db.ProcessConcernLevelEscalation, passed.Level)
	assert.Equal(t, "agent:"+esc, passed.Holder)
	mail := oversightInbox(t, escConv)
	require.Len(t, mail, 1)
	assert.Contains(t, mail[0].Body, "escalation agent")
	assert.Contains(t, mail[0].Body, "lena did not answer")

	// The overseer it passed over no longer holds it.
	answerConcernAs(t, f, leadConv, "run_ov2", concern.ID, "continue", "", http.StatusForbidden)

	advance(agentd.ProcessConcernPatienceForTest() + time.Second)
	agentd.SweepProcessConcernsForTest()
	run = showOversightRun(t, f, "run_ov2")
	atOperator := run.concern(t, concern.ID)
	assert.Equal(t, db.ProcessConcernLevelOperator, atOperator.Level)
	require.NotZero(t, atOperator.MessageID)
	card, err := db.GetHumanMessage(atOperator.MessageID)
	require.NoError(t, err)
	assert.Contains(t, card.Body, "esme did not answer")
	assert.Contains(t, card.Body, "The run is paused")

	pickOnCard(t, atOperator.MessageID, "Keep paused", "Wait for the CI fix.", http.StatusOK)
	run = showOversightRun(t, f, "run_ov2")
	assert.NotNil(t, run.Paused)
	assert.Equal(t, "keep_paused", run.concern(t, concern.ID).Resolution)

	kinds, events := performerEventKinds(t, f, "run_ov2")
	assert.Equal(t, 2, countKind(kinds, db.ProcessEventConcernEscalated))
	for _, event := range events {
		if event.Kind == db.ProcessEventConcernEscalated {
			assert.Contains(t, string(event.Payload), "did not answer")
		}
	}
}

// Scenario: an unscoped run started by the operator is overseen by the
// operator, so a concern about its blocked step goes straight to the card.
// The operator skips the step and the run completes.
func TestProcessOversight_OperatorOverseerSkipsABlockedStep(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processBlockingTemplate("ov-parking", 1))
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(_ context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		return processObservation(dispatch.Command(), engine.ProgramFailed, 1), nil
	}))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_ov3", "templateId": "ov-parking", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	run := showOversightRun(t, f, "run_ov3")
	require.Len(t, run.Blocked, 1)
	require.NotNil(t, run.Overseer)
	assert.Equal(t, "human:operator", run.Overseer.Ref)

	rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_ov3/concerns",
		map[string]any{"reason": "task-01 keeps failing; it is flaky, not broken.", "nodeId": "task-01"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var raised struct {
		Concern oversightConcernWire `json:"concern"`
	}
	testharness.DecodeJSON(t, rec, &raised)
	assert.Equal(t, db.ProcessConcernSourceOperator, raised.Concern.Source)
	assert.Equal(t, db.ProcessConcernLevelOperator, raised.Concern.Level)
	assert.Equal(t, []string{"Resume", "Skip this step", "Keep paused", "Cancel the run"}, raised.Concern.Choices)

	pickOnCard(t, raised.Concern.MessageID, "Skip this step", "flaky", http.StatusOK)
	run = showOversightRun(t, f, "run_ov3")
	assert.Equal(t, engine.RunCompleted, run.Status)
	assert.Equal(t, "skip", run.concern(t, raised.Concern.ID).Resolution)
}

// Scenario: the overseer is set at start and changed later; only
// process.runs.manage sets it, and the run shows who it is.
func TestProcessOversight_OverseerIsSetAtStartAndChangedLater(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const leadConv, aliceConv = "ovl4-1111-2222-3333-4444", "ova4-1111-2222-3333-4444"
	lead := havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "idle")
	alice := havePerformerAgent(t, f, "crew", aliceConv, "alice", "dev", "idle")
	putProcessRuntimeTemplate(t, root, epicTemplate())

	rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_ov4", "templateId": "epic", "group": "crew", "authorizeProgramProfiles": []string{}, "overseer": "nobody-by-that-name",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	rec = processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_ov4", "templateId": "epic", "group": "crew", "authorizeProgramProfiles": []string{}, "overseer": "alice",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	run := showOversightRun(t, f, "run_ov4")
	require.NotNil(t, run.Overseer)
	assert.Equal(t, alice, run.Overseer.AgentID)
	assert.Equal(t, "set", run.Overseer.Via)

	rec = agentReq(t, f, aliceConv, http.MethodPost, "/v1/process/runs/run_ov4/overseer", map[string]any{"overseer": "lena"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "overseeing a run does not let it hand the run on: %s", rec.Body.String())

	rec = processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_ov4/overseer", map[string]any{"overseer": "default"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	run = showOversightRun(t, f, "run_ov4")
	assert.Equal(t, lead, run.Overseer.AgentID)
	assert.Equal(t, "lead", run.Overseer.Via)

	kinds, _ := performerEventKinds(t, f, "run_ov4")
	assert.Equal(t, 2, countKind(kinds, db.ProcessEventOverseerSet))
}

// Scenario: the group's lead oversees a run and the group has no escalation
// agent, so its own concern would go straight to the operator. That takes
// human.notify, as reaching the operator always does; with it, the card is
// posted.
func TestProcessOversight_ALoneOverseerNeedsHumanNotifyToReachTheOperator(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const leadConv = "ovl5-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "idle")
	putProcessRuntimeTemplate(t, root, epicTemplate())
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_ov5", "templateId": "epic", "group": "crew", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	raiseConcernAs(t, f, leadConv, "run_ov5", "", "Nobody above me on this run.", http.StatusForbidden)
	assert.Empty(t, showOversightRun(t, f, "run_ov5").Concerns)

	require.NoError(t, db.SetAgentPermissionOverride(leadConv, agentd.PermHumanNotify, db.PermEffectGrant, "test"))
	concern := raiseConcernAs(t, f, leadConv, "run_ov5", "", "Nobody above me on this run.", http.StatusCreated)
	assert.Equal(t, db.ProcessConcernLevelOperator, concern.Level)
	assert.NotZero(t, concern.MessageID)
}

// Scenario: the operator cancels a run from one concern's card while another
// concern is still open. The sweep settles the other one as run_ended and
// takes its card down, so nothing is left asking about a run that is over.
func TestProcessOversight_ConcernsOfAnEndedRunAreSettled(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processBlockingTemplate("ov-parking", 1))
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(_ context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		return processObservation(dispatch.Command(), engine.ProgramFailed, 1), nil
	}))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_ov6", "templateId": "ov-parking", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	raise := func(reason string) oversightConcernWire {
		rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_ov6/concerns", map[string]any{"reason": reason, "nodeId": "task-01"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var raised struct {
			Concern oversightConcernWire `json:"concern"`
		}
		testharness.DecodeJSON(t, rec, &raised)
		return raised.Concern
	}
	require.Len(t, showOversightRun(t, f, "run_ov6").Blocked, 1)
	first, second := raise("task-01 cannot pass."), raise("And the vendor is down.")

	pickOnCard(t, first.MessageID, "Cancel the run", "", http.StatusOK)
	require.Equal(t, engine.RunCanceled, showOversightRun(t, f, "run_ov6").Status)
	assert.Equal(t, "open", showOversightRun(t, f, "run_ov6").concern(t, second.ID).State)

	agentd.SweepProcessConcernsForTest()
	other := showOversightRun(t, f, "run_ov6").concern(t, second.ID)
	assert.Equal(t, "resolved", other.State)
	assert.Equal(t, "run_ended", other.Resolution)
	card, err := db.GetHumanMessage(second.MessageID)
	require.NoError(t, err)
	assert.False(t, card.IsOpenDecision(), "the ended run's card is taken down")
}
