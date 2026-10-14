package agentd_test

import (
	"cmp"
	"context"
	"encoding/json"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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

// tcl-09xlwr: agent performer dispatch and report-back. These flows drive a
// real run through the daemon mux with simulated agents: performers are
// resolved from the run's scope, handed their node through the agent inbox,
// and report through POST /v1/process/report as authenticated agent callers.
// Assertions read the run view (`tclaude process show`), the run's evidence,
// and each agent's inbox.

const performerNode = "work"

type performerRunView struct {
	ID         string           `json:"id"`
	Status     engine.RunStatus `json:"status"`
	Action     string           `json:"action"`
	Checkpoint engine.Checkpoint
	Commands   []struct {
		CommandID string             `json:"commandId"`
		NodeID    string             `json:"nodeId"`
		Kind      engine.CommandKind `json:"kind"`
		Attempt   int                `json:"attempt"`
		State     string             `json:"state"`
		Performer *performerView     `json:"performer"`
	} `json:"commands"`
	Performers []performerView `json:"performers"`
}

type performerView struct {
	NodeID     string `json:"nodeId"`
	Attempt    int    `json:"attempt"`
	State      string `json:"state"`
	AgentID    string `json:"agentId"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Resolution string `json:"resolution"`
	WaitReason string `json:"waitReason"`
	Outcome    string `json:"outcome"`
	Note       string `json:"note"`
}

// performerTemplate is start → work (agent) → end.
func performerTemplate(id string, performer model.Performer, retry *model.RetryPolicy) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "start",
		Nodes: map[string]model.Node{
			"start":       {Type: model.NodeTypeStart, Next: model.Next{"next": performerNode}},
			performerNode: {Type: model.NodeTypeTask, Performer: &performer, Retry: retry, Next: model.Next{"next": "end"}},
			"end":         {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

func devPerformer() model.Performer {
	return model.Performer{Kind: model.PerformerAgent, Profile: "dev", Prompt: "Implement the feature and open a PR."}
}

// havePerformerAgent registers one simulated agent: a titled conversation, a
// live pane in the given hook status unless "offline", and group membership
// with a role when group is set.
func havePerformerAgent(t *testing.T, f *testharness.Flow, group, conv, name, role, status string) string {
	t.Helper()
	f.HaveConvWithTitle(conv, name)
	if status != "offline" {
		f.HaveAliveSession(conv, "spwn-"+name, "tmux-"+name, f.TestCwd(name))
		setConvStatus(t, conv, status)
	}
	if group != "" {
		f.HaveMemberWithRole(group, conv, role)
	} else {
		f.HaveEnrolledAgent(conv)
	}
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	return agentID
}

// startPerformerRun stores the template, binds the run's scope, and creates
// the run as the human. Dispatch happens as part of creation.
func startPerformerRun(t *testing.T, f *testharness.Flow, root, runID string, tmpl *model.Template,
	scope agentd.ProcessRunScopeForTest) performerRunView {
	t.Helper()
	putProcessRuntimeTemplate(t, root, tmpl)
	t.Cleanup(agentd.SetProcessRunScopeForTest(runID, scope))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": runID, "templateId": tmpl.ID, "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessDispatchForTest()
	return showPerformerRun(t, f, runID)
}

func showPerformerRun(t *testing.T, f *testharness.Flow, runID string) performerRunView {
	t.Helper()
	agentd.WaitForProcessDispatchForTest()
	return peekPerformerRun(t, f, runID)
}

// peekPerformerRun reads the run view without waiting for background passes,
// for a test that holds a run's live owner on purpose.
func peekPerformerRun(t *testing.T, f *testharness.Flow, runID string) performerRunView {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view performerRunView
	testharness.DecodeJSON(t, rec, &view)
	return view
}

func reportAs(t *testing.T, f *testharness.Flow, conv string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rec := agentReq(t, f, conv, http.MethodPost, "/v1/process/report", body)
	agentd.WaitForProcessDispatchForTest()
	return rec
}

func processInbox(t *testing.T, conv string) []*db.AgentMessage {
	t.Helper()
	rows, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	var out []*db.AgentMessage
	for _, row := range rows {
		if strings.HasPrefix(row.Subject, "[process]") {
			out = append(out, row)
		}
	}
	slices.SortFunc(out, func(a, b *db.AgentMessage) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

type performerEvent struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
	Actor   string          `json:"actor"`
}

func performerEventKinds(t *testing.T, f *testharness.Flow, runID string) ([]string, []performerEvent) {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID+"/events", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page struct {
		Events []performerEvent `json:"events"`
	}
	testharness.DecodeJSON(t, rec, &page)
	kinds := make([]string, 0, len(page.Events))
	for _, event := range page.Events {
		kinds = append(kinds, event.Kind)
	}
	return kinds, page.Events
}

func soleAgentCommand(t *testing.T, view performerRunView) (string, int) {
	t.Helper()
	require.Len(t, view.Commands, 1, "%+v", view)
	require.Equal(t, engine.CommandAgent, view.Commands[0].Kind)
	return view.Commands[0].CommandID, view.Commands[0].Attempt
}

// Scenario: a group-scoped run dispatches its agent node to the idle member
// whose role matches, through that member's inbox; the member reports
// success and the run completes. Busy and other-role members are not chosen,
// and the performer is recorded per attempt for avatars.
func TestProcessPerformer_GroupRoleDispatchAndReportCompletesRun(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const busy, idle, reviewer = "busy-1111-2222-3333-4444", "idle-1111-2222-3333-4444", "revw-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", busy, "bob", "dev", "working")
	alice := havePerformerAgent(t, f, "crew", idle, "alice", "dev", "idle")
	havePerformerAgent(t, f, "crew", reviewer, "carol", "reviewer", "idle")

	view := startPerformerRun(t, f, root, "run_group_role", performerTemplate("group-role", devPerformer(), nil),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})

	commandID, attempt := soleAgentCommand(t, view)
	assert.Equal(t, 1, attempt)
	assert.Equal(t, "awaiting_performer", view.Action)
	assert.Equal(t, db.ProcessDispatchDelivered, view.Commands[0].State)
	require.NotNil(t, view.Commands[0].Performer)
	assert.Equal(t, alice, view.Commands[0].Performer.AgentID, "the idle dev is preferred over the busy one")
	assert.Equal(t, "role", view.Commands[0].Performer.Resolution)
	assert.Equal(t, "alice", view.Commands[0].Performer.Name)
	assert.Equal(t, "idle", view.Commands[0].Performer.Status)

	inbox := processInbox(t, idle)
	require.Len(t, inbox, 1, "the node is delivered through the agent inbox")
	body := inbox[0].Body
	assert.Contains(t, body, "Run:     run_group_role")
	assert.Contains(t, body, "Node:    work")
	assert.Contains(t, body, "Attempt: 1")
	assert.Contains(t, body, "Implement the feature and open a PR.")
	assert.Contains(t, body, "tclaude process report succeeded --run run_group_role --node work --attempt 1")
	assert.Empty(t, processInbox(t, busy))
	assert.Empty(t, processInbox(t, reviewer))

	rec := reportAs(t, f, idle, map[string]any{
		"runId": "run_group_role", "nodeId": performerNode, "attempt": 1, "outcome": "succeeded", "note": "PR #12 open",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"status":"accepted"`)

	done := showPerformerRun(t, f, "run_group_role")
	assert.Equal(t, engine.RunCompleted, done.Status)
	require.Len(t, done.Performers, 1)
	assert.Equal(t, performerView{NodeID: performerNode, Attempt: 1, State: db.ProcessDispatchClosed,
		AgentID: alice, Name: "alice", Status: "idle", Resolution: "role", Outcome: "succeeded", Note: "PR #12 open"},
		done.Performers[0])

	kinds, events := performerEventKinds(t, f, "run_group_role")
	assert.Equal(t, []string{"run_created", "agent_prepared", "agent_reported", "engine_advanced"}, kinds)
	assert.Equal(t, "agent:"+alice, events[2].Actor, "the report is attributed to the authenticated performer")
	var reported struct {
		Observation engine.ProgramObservation `json:"observation"`
		Performer   string                    `json:"performer"`
	}
	require.NoError(t, json.Unmarshal(events[2].Payload, &reported))
	assert.Equal(t, commandID, reported.Observation.CommandID)
	assert.Equal(t, alice, reported.Performer)
}

// Scenario: a report is bound to the exact delivered attempt. A different
// agent is refused, a human cannot report as a performer, an agent with no
// open node gets a clear not-found, a repeated identical report is
// acknowledged without being applied twice, and a contradicting one is refused.
func TestProcessPerformer_ReportAuthenticationAndIdempotency(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const performer, other = "perf-1111-2222-3333-4444", "othr-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", performer, "alice", "dev", "idle")
	havePerformerAgent(t, f, "crew", other, "bob", "reviewer", "idle")
	startPerformerRun(t, f, root, "run_auth", performerTemplate("auth", devPerformer(), nil),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})

	wrong := reportAs(t, f, other, map[string]any{"runId": "run_auth", "nodeId": performerNode, "outcome": "succeeded"})
	assert.Equal(t, http.StatusForbidden, wrong.Code, wrong.Body.String())
	assert.Contains(t, wrong.Body.String(), "process_report_wrong_performer")

	nothing := reportAs(t, f, other, map[string]any{"outcome": "succeeded"})
	assert.Equal(t, http.StatusNotFound, nothing.Code, nothing.Body.String())

	human := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/report", map[string]any{"outcome": "succeeded"})
	assert.Equal(t, http.StatusUnauthorized, human.Code, human.Body.String())

	assert.Equal(t, engine.RunRunning, showPerformerRun(t, f, "run_auth").Status, "refused reports change nothing")

	// A note the executor would refuse is refused before it is recorded, so
	// the performer can simply report again.
	bad := reportAs(t, f, performer, map[string]any{"outcome": "failed", "note": "boom \u001b[31mred"})
	assert.Equal(t, http.StatusUnprocessableEntity, bad.Code, bad.Body.String())
	assert.Contains(t, bad.Body.String(), "process_report_note")

	// The performer's single open node needs no selector.
	first := reportAs(t, f, performer, map[string]any{"outcome": "succeeded"})
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	assert.Contains(t, first.Body.String(), `"status":"accepted"`)

	again := reportAs(t, f, performer, map[string]any{"runId": "run_auth", "nodeId": performerNode, "attempt": 1, "outcome": "succeeded"})
	require.Equal(t, http.StatusOK, again.Code, again.Body.String())
	assert.Contains(t, again.Body.String(), `"status":"duplicate"`)

	contradict := reportAs(t, f, performer, map[string]any{"runId": "run_auth", "nodeId": performerNode, "outcome": "failed"})
	assert.Equal(t, http.StatusConflict, contradict.Code, contradict.Body.String())
	assert.Contains(t, contradict.Body.String(), "process_report_conflict")

	kinds, _ := performerEventKinds(t, f, "run_auth")
	assert.Equal(t, 1, countKind(kinds, "agent_reported"), "the duplicate was not applied twice: %v", kinds)
	assert.Equal(t, engine.RunCompleted, showPerformerRun(t, f, "run_auth").Status)
}

func countKind(kinds []string, kind string) int {
	n := 0
	for _, k := range kinds {
		if k == kind {
			n++
		}
	}
	return n
}

// Scenario: a failed report inside a fresh-attempt retry budget mints attempt
// 2, which goes to a fresh performer. The first performer's late report for
// attempt 1 is refused as stale, and the new performer completes the run.
func TestProcessPerformer_FreshAttemptRetryAndStaleReport(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const first, second = "frst-1111-2222-3333-4444", "scnd-1111-2222-3333-4444"
	alice := havePerformerAgent(t, f, "crew", first, "alice", "dev", "idle")
	bob := havePerformerAgent(t, f, "crew", second, "bob", "dev", "working")
	retry := &model.RetryPolicy{MaxAttempts: 2, OnFail: model.RetryModeFreshAttempt}
	view := startPerformerRun(t, f, root, "run_fresh", performerTemplate("fresh", devPerformer(), retry),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})
	require.Equal(t, alice, view.Commands[0].Performer.AgentID)

	failed := reportAs(t, f, first, map[string]any{"runId": "run_fresh", "nodeId": performerNode, "attempt": 1,
		"outcome": "failed", "note": "could not reproduce"})
	require.Equal(t, http.StatusOK, failed.Code, failed.Body.String())

	retried := showPerformerRun(t, f, "run_fresh")
	_, attempt := soleAgentCommand(t, retried)
	assert.Equal(t, 2, attempt)
	require.NotNil(t, retried.Commands[0].Performer)
	assert.Equal(t, bob, retried.Commands[0].Performer.AgentID, "a fresh attempt goes to a fresh performer")
	require.Len(t, processInbox(t, second), 1)
	assert.Contains(t, processInbox(t, second)[0].Body, "Attempt: 2")
	assert.NotContains(t, processInbox(t, second)[0].Body, "could not reproduce", "fresh attempts carry no feedback")

	late := reportAs(t, f, first, map[string]any{"runId": "run_fresh", "nodeId": performerNode, "attempt": 1, "outcome": "succeeded"})
	assert.Equal(t, http.StatusConflict, late.Code, late.Body.String())
	assert.Contains(t, late.Body.String(), "process_report_stale")

	notYours := reportAs(t, f, first, map[string]any{"runId": "run_fresh", "nodeId": performerNode, "attempt": 2, "outcome": "succeeded"})
	assert.Equal(t, http.StatusForbidden, notYours.Code, notYours.Body.String())

	ok := reportAs(t, f, second, map[string]any{"runId": "run_fresh", "nodeId": performerNode, "attempt": 2, "outcome": "succeeded"})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	done := showPerformerRun(t, f, "run_fresh")
	assert.Equal(t, engine.RunCompleted, done.Status)
	require.Len(t, done.Performers, 2, "one performer per attempt")
	assert.Equal(t, []string{alice, bob}, []string{done.Performers[0].AgentID, done.Performers[1].AgentID})
	assert.Equal(t, []string{"failed", "succeeded"}, []string{done.Performers[0].Outcome, done.Performers[1].Outcome})
}

// Scenario: feedback-same-session hands the next attempt back to the same live
// performer, together with what its failed report said.
func TestProcessPerformer_FeedbackSameSessionReturnsToPerformer(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const first, second = "fbak-1111-2222-3333-4444", "fbot-1111-2222-3333-4444"
	alice := havePerformerAgent(t, f, "crew", first, "alice", "dev", "idle")
	havePerformerAgent(t, f, "crew", second, "bob", "dev", "idle")
	retry := &model.RetryPolicy{MaxAttempts: 2, OnFail: model.RetryModeFeedbackSameSession}
	startPerformerRun(t, f, root, "run_feedback", performerTemplate("feedback", devPerformer(), retry),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})

	failed := reportAs(t, f, first, map[string]any{"outcome": "failed", "note": "tests are red on CI"})
	require.Equal(t, http.StatusOK, failed.Code, failed.Body.String())

	retried := showPerformerRun(t, f, "run_feedback")
	require.NotNil(t, retried.Commands[0].Performer)
	assert.Equal(t, alice, retried.Commands[0].Performer.AgentID)
	assert.Equal(t, "same_session", retried.Commands[0].Performer.Resolution)
	inbox := processInbox(t, first)
	require.Len(t, inbox, 2)
	assert.Contains(t, inbox[1].Body, "Attempt: 2")
	assert.Contains(t, inbox[1].Body, "tests are red on CI", "the failure is handed back as feedback")
	assert.Empty(t, processInbox(t, second))
}

// Scenario: the daemon restarts while a performer works. The in-flight
// dispatch is rediscovered without being re-delivered, and the report still
// lands. A command whose dispatch row was never written (a crash between the
// planning commit and the row insert) is rediscovered and delivered.
func TestProcessPerformer_RestartRediscovery(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const performer = "rstr-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", performer, "alice", "dev", "idle")
	startPerformerRun(t, f, root, "run_restart", performerTemplate("restart", devPerformer(), nil),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})
	require.Len(t, processInbox(t, performer), 1)

	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	agentd.SweepProcessAgentDispatchesForTest()
	assert.Len(t, processInbox(t, performer), 1, "a delivered dispatch is not re-sent after a restart")
	assert.Equal(t, db.ProcessDispatchDelivered, showPerformerRun(t, f, "run_restart").Commands[0].State)

	ok := reportAs(t, f, performer, map[string]any{"outcome": "succeeded"})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Equal(t, engine.RunCompleted, showPerformerRun(t, f, "run_restart").Status)

	// A planned agent command with no dispatch row at all.
	tmpl := performerTemplate("restart-lost", devPerformer(), nil)
	record := putProcessRuntimeTemplate(t, root, tmpl)
	definition, err := engine.Prepare(tmpl, map[string]string{})
	require.NoError(t, err)
	checkpoint, err := engine.Initialize("run_lost_row", definition)
	require.NoError(t, err)
	checkpoint, command, _, err := engine.AdvanceAndPlan(checkpoint, definition)
	require.NoError(t, err)
	require.NotNil(t, command)
	createProcessRunFixtureWithCheckpoint(t, "run_lost_row", record.Ref, tmpl, checkpoint)
	t.Cleanup(agentd.SetProcessRunScopeForTest("run_lost_row", agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID}))

	agentd.SweepProcessAgentDispatchesForTest()
	lost := showPerformerRun(t, f, "run_lost_row")
	assert.Equal(t, db.ProcessDispatchDelivered, lost.Commands[0].State)
	assert.Equal(t, "awaiting_performer", lost.Action, "an agent command is never shown as needing reconciliation")
	assert.Len(t, processInbox(t, performer), 2)
}

// Scenario: an offline performer still receives the node — its inbox keeps it
// — and the node simply waits. Once that performer is retired, the dispatch is
// re-resolved to another member, and the retired agent can no longer report.
func TestProcessPerformer_OfflineWaitsRetiredIsReplaced(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const offline, replacement = "offl-1111-2222-3333-4444", "rplc-1111-2222-3333-4444"
	alice := havePerformerAgent(t, f, "crew", offline, "alice", "dev", "offline")
	view := startPerformerRun(t, f, root, "run_offline", performerTemplate("offline", devPerformer(), nil),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})
	require.NotNil(t, view.Commands[0].Performer)
	assert.Equal(t, alice, view.Commands[0].Performer.AgentID)
	assert.Equal(t, "offline", view.Commands[0].Performer.Status)
	assert.Equal(t, "awaiting_performer", view.Action)
	require.Len(t, processInbox(t, offline), 1)

	agentd.SweepProcessAgentDispatchesForTest()
	assert.Equal(t, alice, showPerformerRun(t, f, "run_offline").Commands[0].Performer.AgentID,
		"an offline performer is not replaced")

	f.HaveRetiredAgent(offline)
	bob := havePerformerAgent(t, f, "crew", replacement, "bob", "dev", "idle")
	agentd.SweepProcessAgentDispatchesForTest()
	moved := showPerformerRun(t, f, "run_offline")
	require.NotNil(t, moved.Commands[0].Performer)
	assert.Equal(t, bob, moved.Commands[0].Performer.AgentID, "a retired performer's node is re-resolved")
	assert.Equal(t, 1, moved.Commands[0].Attempt, "re-resolution keeps the attempt")
	require.Len(t, processInbox(t, replacement), 1)

	ok := reportAs(t, f, replacement, map[string]any{"outcome": "succeeded"})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Equal(t, engine.RunCompleted, showPerformerRun(t, f, "run_offline").Status)
}

// Scenario: nobody in the group can take the node and the template does not
// allow spawning, so the node visibly waits for a performer. When a matching
// member joins, the next sweep delivers to it.
func TestProcessPerformer_NeedsPerformerUntilMemberJoins(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	havePerformerAgent(t, f, "crew", "rvwr-1111-2222-3333-4444", "carol", "reviewer", "idle")
	view := startPerformerRun(t, f, root, "run_needs", performerTemplate("needs", devPerformer(), nil),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})
	assert.Equal(t, "needs_performer", view.Action)
	assert.Equal(t, db.ProcessDispatchNeedsPerformer, view.Commands[0].State)
	require.NotNil(t, view.Commands[0].Performer)
	assert.Contains(t, view.Commands[0].Performer.WaitReason, `no group member has role "dev"`)

	const joiner = "join-1111-2222-3333-4444"
	dave := havePerformerAgent(t, f, "crew", joiner, "dave", "dev", "idle")
	agentd.SweepProcessAgentDispatchesForTest()
	joined := showPerformerRun(t, f, "run_needs")
	assert.Equal(t, db.ProcessDispatchDelivered, joined.Commands[0].State)
	assert.Equal(t, dave, joined.Commands[0].Performer.AgentID)
	assert.Empty(t, joined.Commands[0].Performer.WaitReason)
}

// Scenario: when the template allows it and nobody in the group can take the
// node, a fresh performer is spawned into the group with the node's profile,
// and its startup brief is the node prompt.
func TestProcessPerformer_SpawnsWhenTemplateAllows(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	_, err := db.SetAgentGroupDefaultCwd("crew", t.TempDir())
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "dev", Harness: "claude"})
	require.NoError(t, err)
	performer := devPerformer()
	performer.Spawn = model.SpawnAllowed
	view := startPerformerRun(t, f, root, "run_spawn", performerTemplate("spawn", performer, nil),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})
	require.NotNil(t, view.Commands[0].Performer, "%+v", view)
	assert.Equal(t, db.ProcessDispatchDelivered, view.Commands[0].State, "%+v", view.Commands[0].Performer)
	assert.Equal(t, "spawned", view.Commands[0].Performer.Resolution)
	spawned := view.Commands[0].Performer.AgentID
	require.NotEmpty(t, spawned)

	conv, err := db.CurrentConvForAgent(spawned)
	require.NoError(t, err)
	require.NotEmpty(t, conv)
	member, err := db.FindMemberInGroup(g.ID, conv)
	require.NoError(t, err)
	require.NotNil(t, member, "the spawned performer joins the run's group")

	ok := reportAs(t, f, conv, map[string]any{"outcome": "succeeded"})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Equal(t, engine.RunCompleted, showPerformerRun(t, f, "run_spawn").Status)
}

// Scenario: an agent-scoped run is performed by its bound agent regardless of
// group roles. An assignee naming anyone else is refused as a wait, and a
// retired bound agent is not silently replaced (the run's scope owns that).
func TestProcessPerformer_AgentScopeUsesBoundAgent(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const bound, member = "bond-1111-2222-3333-4444", "mmbr-1111-2222-3333-4444"
	alice := havePerformerAgent(t, f, "", bound, "alice", "", "working")
	havePerformerAgent(t, f, "crew", member, "bob", "dev", "idle")

	view := startPerformerRun(t, f, root, "run_bound", performerTemplate("bound", devPerformer(), nil),
		agentd.ProcessRunScopeForTest{Kind: "agent", AgentID: alice})
	require.NotNil(t, view.Commands[0].Performer)
	assert.Equal(t, alice, view.Commands[0].Performer.AgentID)
	assert.Equal(t, "bound", view.Commands[0].Performer.Resolution)
	assert.Empty(t, processInbox(t, member))

	f.HaveRetiredAgent(bound)
	agentd.SweepProcessAgentDispatchesForTest()
	assert.Equal(t, alice, showPerformerRun(t, f, "run_bound").Commands[0].Performer.AgentID,
		"an agent-scoped run is never re-resolved to another agent")

	mismatch := devPerformer()
	mismatch.Assignee = "bob"
	other := startPerformerRun(t, f, root, "run_bound_mismatch", performerTemplate("bound-mismatch", mismatch, nil),
		agentd.ProcessRunScopeForTest{Kind: "agent", AgentID: peerAgent(t, member)})
	assert.Equal(t, db.ProcessDispatchDelivered, other.Commands[0].State, "assignee names the bound agent")
	wrong := devPerformer()
	wrong.Assignee = "carol"
	refused := startPerformerRun(t, f, root, "run_bound_wrong", performerTemplate("bound-wrong", wrong, nil),
		agentd.ProcessRunScopeForTest{Kind: "agent", AgentID: peerAgent(t, member)})
	assert.Equal(t, "needs_performer", refused.Action)
	assert.Contains(t, refused.Commands[0].Performer.WaitReason, "not the agent this run is bound to")
}

func peerAgent(t *testing.T, conv string) string {
	t.Helper()
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	return agentID
}

// Scenario: an unscoped run has no members, so only an explicit assignee can
// take an agent node; without one the node waits with an actionable reason.
func TestProcessPerformer_UnscopedRunNeedsAssignee(t *testing.T) {
	f, root := processRuntimeFlow(t)
	const conv = "unsc-1111-2222-3333-4444"
	alice := havePerformerAgent(t, f, "", conv, "alice", "", "idle")

	plain := startPerformerRun(t, f, root, "run_unscoped", performerTemplate("unscoped", devPerformer(), nil),
		agentd.ProcessRunScopeForTest{Kind: "none"})
	assert.Equal(t, "needs_performer", plain.Action)
	assert.Contains(t, plain.Commands[0].Performer.WaitReason, "name an assignee")

	assigned := devPerformer()
	assigned.Assignee = "alice"
	view := startPerformerRun(t, f, root, "run_assigned", performerTemplate("assigned", assigned, nil),
		agentd.ProcessRunScopeForTest{Kind: "none"})
	require.NotNil(t, view.Commands[0].Performer)
	assert.Equal(t, alice, view.Commands[0].Performer.AgentID)
	assert.Equal(t, "assignee", view.Commands[0].Performer.Resolution)

	// An agent-owned run reaches only agents its owner shares a group with:
	// the daemon delivers on the owner's behalf.
	f.HaveGroup("elsewhere")
	owner := havePerformerAgent(t, f, "elsewhere", "ownr-1111-2222-3333-4444", "owen", "lead", "idle")
	foreign := startPerformerRun(t, f, root, "run_foreign", performerTemplate("foreign", assigned, nil),
		agentd.ProcessRunScopeForTest{Kind: "none", Owner: "agent:" + owner})
	assert.Equal(t, "needs_performer", foreign.Action)
	assert.Contains(t, foreign.Commands[0].Performer.WaitReason, "shares no group with the run owner")
	assert.Len(t, processInbox(t, conv), 1, "only the operator-owned run reached alice")
}

// Scenario: a person settles an agent's node in its place (human
// preemption). The run moves on, the performer is told the attempt no longer
// needs it, and its late report is refused as stale.
func TestProcessPerformer_HumanPreemption(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const performer = "prmt-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", performer, "alice", "dev", "idle")
	startPerformerRun(t, f, root, "run_preempt", performerTemplate("preempt", devPerformer(), nil),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})

	rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_preempt/record-outcome", map[string]any{
		"nodeId": performerNode, "outcome": "succeeded", "note": "done by hand",
	})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	agentd.SweepProcessAgentDispatchesForTest()

	view := showPerformerRun(t, f, "run_preempt")
	assert.Equal(t, engine.RunCompleted, view.Status)
	require.Len(t, view.Performers, 1)
	assert.Equal(t, db.ProcessDispatchClosed, view.Performers[0].State)
	assert.Empty(t, view.Performers[0].Outcome, "the performer never reported")

	inbox := processInbox(t, performer)
	require.Len(t, inbox, 2, "the performer is told the attempt was settled without it")
	assert.Contains(t, inbox[1].Subject, "withdrawn")

	late := reportAs(t, f, performer, map[string]any{"runId": "run_preempt", "nodeId": performerNode, "outcome": "succeeded"})
	assert.Equal(t, http.StatusConflict, late.Code, late.Body.String())
	assert.Contains(t, late.Body.String(), "process_report_stale")
}

// Scenario: a sibling program fails outright while an agent holds its node.
// The doomed run cannot finish around an outstanding attempt, so the agent's
// attempt is withdrawn, the agent is told to stop, and the run fails.
func TestProcessPerformer_DoomedRunWithdrawsAgentAttempt(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const performer = "wdrw-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", performer, "alice", "dev", "idle")
	agentNode := devPerformer()
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "withdraw", Start: "start",
		Nodes: map[string]model.Node{
			"start":       {Type: model.NodeTypeStart, Next: model.Next{"next": "fork"}},
			"fork":        {Type: model.NodeTypeParallel, Next: model.Next{"agent": performerNode, "program": "check"}},
			performerNode: {Type: model.NodeTypeTask, Performer: &agentNode, Next: model.Next{"next": "join"}},
			"check": {Type: model.NodeTypeTask, Next: model.Next{"next": "join"},
				Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: "false"}},
			"join": {Type: model.NodeTypeEnd, Join: model.JoinAll, Result: "success"},
		},
	}
	putProcessRuntimeTemplate(t, root, tmpl)
	t.Cleanup(agentd.SetProcessRunScopeForTest("run_withdraw", agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID}))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_withdraw", "templateId": "withdraw", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessRunRuntimeForTest()
	agentd.SweepProcessAgentDispatchesForTest()

	view := showPerformerRun(t, f, "run_withdraw")
	assert.Equal(t, engine.RunFailed, view.Status, "%+v", view)
	assert.Empty(t, view.Commands)
	require.Len(t, view.Performers, 1)
	assert.Equal(t, db.ProcessDispatchClosed, view.Performers[0].State)
	inbox := processInbox(t, performer)
	require.Len(t, inbox, 2)
	assert.Contains(t, inbox[1].Subject, "withdrawn")
	assert.Contains(t, inbox[1].Body, "Stop working")

	late := reportAs(t, f, performer, map[string]any{"runId": "run_withdraw", "nodeId": performerNode, "outcome": "succeeded"})
	assert.Equal(t, http.StatusConflict, late.Code, late.Body.String())
}

// Scenario: a person settles an agent's node while a sibling program branch is
// still running. The run is claimed by its live owner, so the outcome is
// routed through that owner instead of being refused as claimed.
func TestProcessPerformer_HumanPreemptionWhileSiblingProgramRuns(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const performer = "pclm-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", performer, "alice", "dev", "idle")
	agentNode := devPerformer()
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "preempt-claimed", Start: "start",
		Nodes: map[string]model.Node{
			"start":       {Type: model.NodeTypeStart, Next: model.Next{"next": "fork"}},
			"fork":        {Type: model.NodeTypeParallel, Next: model.Next{"agent": performerNode, "program": "slow"}},
			performerNode: {Type: model.NodeTypeTask, Performer: &agentNode, Next: model.Next{"next": "join"}},
			"slow": {Type: model.NodeTypeTask, Next: model.Next{"next": "join"},
				Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: "true"}},
			"join": {Type: model.NodeTypeEnd, Join: model.JoinAll, Result: "success"},
		},
	}
	putProcessRuntimeTemplate(t, root, tmpl)
	// The sibling program holds the run's live owner until the test lets go.
	release := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		<-release
		return processObservation(dispatch.Command(), engine.ProgramSucceeded, 0), nil
	}))
	t.Cleanup(letGo) // before restoring the hook, which waits for the runtime
	t.Cleanup(agentd.SetProcessRunScopeForTest("run_preempt_claimed", agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID}))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_preempt_claimed", "templateId": "preempt-claimed", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	require.Eventually(t, func() bool {
		return len(peekPerformerRun(t, f, "run_preempt_claimed").Performers) == 1
	}, 5*time.Second, 10*time.Millisecond, "the agent node is delivered while the program runs: %+v",
		peekPerformerRun(t, f, "run_preempt_claimed"))

	rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_preempt_claimed/record-outcome", map[string]any{
		"nodeId": performerNode, "outcome": "succeeded", "note": "done by hand",
	})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, engine.RunRunning, peekPerformerRun(t, f, "run_preempt_claimed").Status, "the program is still running")

	letGo()
	agentd.WaitForProcessRunRuntimeForTest()
	agentd.SweepProcessAgentDispatchesForTest()
	view := showPerformerRun(t, f, "run_preempt_claimed")
	assert.Equal(t, engine.RunCompleted, view.Status, "%+v", view)
	require.Len(t, view.Performers, 1)
	assert.Equal(t, db.ProcessDispatchClosed, view.Performers[0].State)
}
