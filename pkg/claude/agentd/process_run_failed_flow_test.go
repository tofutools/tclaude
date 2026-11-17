package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tk_82f97468: a step that fails with nothing in its process to handle it
// ends the run there, and the run says which step failed and why: in the run
// view and the run list, to the run's overseer always, and to the operator
// (a Messages card) only for a run they started or one no agent oversees.

func failingBuildTemplate() *model.Template {
	return &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: "build-once", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "build"}},
			"build": {Type: model.NodeTypeTask, Name: "Build", Performer: &model.Performer{Kind: model.PerformerAgent, Role: "implementor", Prompt: "Build it."},
				Next: model.Next{model.DefaultOutcome: "end"}},
			"end": {Type: model.NodeTypeEnd},
		}}
}

type failedRunWire struct {
	Status  engine.RunStatus    `json:"status"`
	Failure *engine.StepFailure `json:"failure"`
}

// failRun has the implementor report its build failed, and waits for the
// run to end and its end to be told.
func failRun(t *testing.T, f *testharness.Flow, implementorConv, runID string) failedRunWire {
	t.Helper()
	agentd.WaitForProcessDispatchForTest()
	rec := reportAs(t, f, implementorConv, map[string]any{"runId": runID, "nodeId": "build", "attempt": 1,
		"outcome": "failed", "note": "the tests do not compile"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	agentd.WaitForProcessRunRuntimeForTest()
	executor.WaitTerminalHooks()
	view := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID, nil)
	require.Equal(t, http.StatusOK, view.Code, view.Body.String())
	var run failedRunWire
	testharness.DecodeJSON(t, view, &run)
	return run
}

func operatorCards(t *testing.T, runID string) []*db.HumanMessage {
	t.Helper()
	all, err := db.ListHumanMessagesPage(db.HumanMessageFilter{}, 0, 200)
	require.NoError(t, err)
	var out []*db.HumanMessage
	for _, m := range all {
		if m.ProcessRunID == runID {
			out = append(out, m)
		}
	}
	return out
}

func failureMail(t *testing.T, conv, runID string) []*db.AgentMessage {
	t.Helper()
	var out []*db.AgentMessage
	for _, m := range processInbox(t, conv) {
		if strings.Contains(m.Subject, "run "+runID+" ") && strings.Contains(m.Subject, "failed at step build") {
			out = append(out, m)
		}
	}
	return out
}

func TestProcessRunFailed_TheRunSaysWhichStepFailedAndWhy(t *testing.T) {
	f, root := processRuntimeFlow(t)
	group := f.HaveGroup("crew")
	const leadConv, buildConv = "flea-1111-2222-3333-4444", "fbld-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "working")
	havePerformerAgent(t, f, "crew", buildConv, "ivan", "implementor", "idle")
	putProcessRuntimeTemplate(t, root, failingBuildTemplate())

	t.Run("started by the operator: the lead and the operator are told", func(t *testing.T) {
		const runID = "run_failed_by_operator"
		created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
			"id": runID, "templateId": "build-once", "group": "crew", "authorizeProgramProfiles": []string{}})
		require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
		run := failRun(t, f, buildConv, runID)
		require.Equal(t, engine.RunFailed, run.Status, "nothing handles the failure: the run stops there")
		require.NotNil(t, run.Failure)
		assert.Equal(t, "build", run.Failure.NodeID)
		assert.Equal(t, 1, run.Failure.Attempt)
		assert.Contains(t, run.Failure.Reason, "the tests do not compile")

		list := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs", nil)
		require.Equal(t, http.StatusOK, list.Code)
		var page struct {
			Runs []struct {
				ID      string              `json:"id"`
				Failure *engine.StepFailure `json:"failure"`
			} `json:"runs"`
		}
		testharness.DecodeJSON(t, list, &page)
		found := false
		for _, r := range page.Runs {
			if r.ID == runID {
				found = true
				require.NotNil(t, r.Failure, "the run list names the failed step")
				assert.Equal(t, "build", r.Failure.NodeID)
				assert.Contains(t, r.Failure.Reason, "the tests do not compile")
			}
		}
		require.True(t, found)

		mail := failureMail(t, leadConv, runID)
		require.Len(t, mail, 1, "the overseer (the group's lead) is told")
		assert.Contains(t, mail[0].Body, "the tests do not compile")
		cards := operatorCards(t, runID)
		require.Len(t, cards, 1, "the operator started it: a Messages card")
		assert.Contains(t, cards[0].Subject, "failed at build")
		assert.Contains(t, cards[0].Body, "the tests do not compile")
		assert.Equal(t, "build", cards[0].ProcessNodeID)
	})

	t.Run("started by an agent: only its overseer is told", func(t *testing.T) {
		require.NoError(t, db.AddAgentGroupOwner(group.ID, leadConv, "test"))
		const runID = "run_failed_by_agent"
		created := agentReq(t, f, leadConv, http.MethodPost, "/v1/process/runs", map[string]any{
			"id": runID, "templateId": "build-once", "group": "crew", "authorizeProgramProfiles": []string{}})
		require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
		run := failRun(t, f, buildConv, runID)
		require.Equal(t, engine.RunFailed, run.Status)
		require.NotNil(t, run.Failure)
		assert.Len(t, failureMail(t, leadConv, runID), 1, "the overseer is told")
		assert.Empty(t, operatorCards(t, runID), "an agent's run with an agent overseer: the operator is not flooded")
	})
}
