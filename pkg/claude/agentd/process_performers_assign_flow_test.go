package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func assignNode(t *testing.T, f *testharness.Flow, runID, nodeID, agent string) *httptest.ResponseRecorder {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodPost,
		"/v1/process/runs/"+runID+"/nodes/"+nodeID+"/assign", map[string]any{"agent": agent})
	agentd.WaitForProcessDispatchForTest()
	return rec
}

// Scenario: a person moves a delivered agent node to another group member.
// The new performer gets the prompt, the previous one is told to stop and can
// no longer report, and the new performer's report completes the run.
func TestProcessPerformerAssign_ReassignsDeliveredAttempt(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const first, second = "asg1-1111-2222-3333-4444", "asg2-1111-2222-3333-4444"
	alice := havePerformerAgent(t, f, "crew", first, "alice", "dev", "idle")
	bob := havePerformerAgent(t, f, "crew", second, "bob", "reviewer", "idle")
	view := startPerformerRun(t, f, root, "run_assign", performerTemplate("assign", devPerformer(), nil),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})
	require.Equal(t, alice, view.Commands[0].Performer.AgentID)

	rec := assignNode(t, f, "run_assign", performerNode, "bob")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	moved := showPerformerRun(t, f, "run_assign")
	require.NotNil(t, moved.Commands[0].Performer)
	assert.Equal(t, bob, moved.Commands[0].Performer.AgentID)
	assert.Equal(t, "reassigned", moved.Commands[0].Performer.Resolution)
	assert.Equal(t, db.ProcessDispatchDelivered, moved.Commands[0].State)

	aliceInbox := processInbox(t, first)
	require.Len(t, aliceInbox, 2)
	assert.Contains(t, aliceInbox[1].Subject, "withdrawn")
	bobInbox := processInbox(t, second)
	require.Len(t, bobInbox, 1)
	assert.Contains(t, bobInbox[0].Body, "Implement the feature")

	again := assignNode(t, f, "run_assign", performerNode, bob)
	require.Equal(t, http.StatusOK, again.Code, again.Body.String())
	assert.Len(t, processInbox(t, second), 1, "assigning the current performer again is a no-op")

	stale := reportAs(t, f, first, map[string]any{"runId": "run_assign", "nodeId": performerNode, "outcome": "succeeded"})
	assert.Equal(t, http.StatusForbidden, stale.Code, stale.Body.String())
	ok := reportAs(t, f, second, map[string]any{"outcome": "succeeded"})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Equal(t, engine.RunCompleted, showPerformerRun(t, f, "run_assign").Status)

	done := assignNode(t, f, "run_assign", performerNode, "alice")
	assert.Equal(t, http.StatusConflict, done.Code, done.Body.String())
	assert.Contains(t, done.Body.String(), "process_assign_not_outstanding")
}

// Scenario: a node waits for a performer because nobody has its role; a
// person assigns it to a member directly.
func TestProcessPerformerAssign_ResolvesNeedsPerformer(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const conv = "asg3-1111-2222-3333-4444"
	carol := havePerformerAgent(t, f, "crew", conv, "carol", "reviewer", "idle")
	view := startPerformerRun(t, f, root, "run_assign_wait", performerTemplate("assign-wait", devPerformer(), nil),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})
	require.Equal(t, "needs_performer", view.Action)

	rec := assignNode(t, f, "run_assign_wait", performerNode, carol)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assigned := showPerformerRun(t, f, "run_assign_wait")
	assert.Equal(t, "awaiting_performer", assigned.Action)
	assert.Equal(t, carol, assigned.Commands[0].Performer.AgentID)
	require.Len(t, processInbox(t, conv), 1)
}

// Scenario: assignments outside the run's scope, or to nothing assignable,
// are refused and change nothing.
func TestProcessPerformerAssign_Refusals(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	f.HaveGroup("elsewhere")
	const member, outsider = "asg4-1111-2222-3333-4444", "asg5-1111-2222-3333-4444"
	alice := havePerformerAgent(t, f, "crew", member, "alice", "dev", "idle")
	havePerformerAgent(t, f, "elsewhere", outsider, "olga", "dev", "idle")
	startPerformerRun(t, f, root, "run_assign_no", performerTemplate("assign-no", devPerformer(), nil),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})

	foreign := assignNode(t, f, "run_assign_no", performerNode, "olga")
	assert.Equal(t, http.StatusUnprocessableEntity, foreign.Code, foreign.Body.String())
	assert.Contains(t, foreign.Body.String(), "not an active member of the run's group")

	unknown := assignNode(t, f, "run_assign_no", "start", "alice")
	assert.Equal(t, http.StatusConflict, unknown.Code, unknown.Body.String())
	assert.Contains(t, unknown.Body.String(), "process_assign_not_outstanding")

	empty := assignNode(t, f, "run_assign_no", performerNode, " ")
	assert.Equal(t, http.StatusUnprocessableEntity, empty.Code, empty.Body.String())

	view := showPerformerRun(t, f, "run_assign_no")
	assert.Equal(t, alice, view.Commands[0].Performer.AgentID, "refusals change nothing")
	assert.Empty(t, processInbox(t, outsider))

	// An agent-scoped run stays with its bound agent.
	startPerformerRun(t, f, root, "run_assign_bound", performerTemplate("assign-bound", devPerformer(), nil),
		agentd.ProcessRunScopeForTest{Kind: "agent", AgentID: alice})
	bound := assignNode(t, f, "run_assign_bound", performerNode, "olga")
	assert.Equal(t, http.StatusUnprocessableEntity, bound.Code, bound.Body.String())
	assert.Contains(t, bound.Body.String(), "bound to")
}
