package agentd_test

import (
	"fmt"
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

// tk_9f7b4513: a message step sends a message and goes on. To the operator
// it is a Messages card, to agents of the run's group an inbox message;
// params and step outputs fill its text. A recipient it cannot reach fails
// the step, naming who, unless the step may fail.

func messageTemplate(id string, to []string, mayFail bool) *model.Template {
	return &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "start",
		Params: map[string]model.Param{"what": {Type: "string"}},
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "build"}},
			"build": {Type: model.NodeTypeTask, Name: "Build", Captures: []string{"pr"},
				Performer: &model.Performer{Kind: model.PerformerAgent, Role: "implementor", Prompt: "Build it."},
				Next:      model.Next{model.DefaultOutcome: "tell"}},
			"tell": {Type: model.NodeTypeTask, Name: "Tell the reviewer", MayFail: mayFail,
				Performer: &model.Performer{Kind: model.PerformerMessage, To: to,
					Subject: "Ready: {{params.what}}", Text: "{{params.what}} is up for review: {{ outputs.pr }}"},
				Next: model.Next{model.DefaultOutcome: "end"}},
			"end": {Type: model.NodeTypeEnd},
		}}
}

type messageRunWire struct {
	Status  engine.RunStatus    `json:"status"`
	Failure *engine.StepFailure `json:"failure"`
}

// runMessageProcess starts tmpl in crew, has the implementor report its
// build with a PR, and returns the run once it has settled.
func runMessageProcess(t *testing.T, f *testharness.Flow, buildConv, runID, templateID string) messageRunWire {
	t.Helper()
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": runID, "templateId": templateID, "group": "crew", "authorizeProgramProfiles": []string{},
		"params": map[string]string{"what": "the login fix"}})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessDispatchForTest()
	rec := reportAs(t, f, buildConv, map[string]any{"runId": runID, "nodeId": "build", "attempt": 1,
		"outcome": "succeeded", "outputs": map[string]string{"pr": "https://example.test/pr/7"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	agentd.WaitForProcessRunRuntimeForTest()
	executor.WaitTerminalHooks()
	view := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID, nil)
	require.Equal(t, http.StatusOK, view.Code, view.Body.String())
	var run messageRunWire
	testharness.DecodeJSON(t, view, &run)
	return run
}

func messageMail(t *testing.T, conv, runID string) []*db.AgentMessage {
	t.Helper()
	rows, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	var out []*db.AgentMessage
	for _, m := range rows {
		if strings.Contains(m.Body, "(run "+runID+")") {
			out = append(out, m)
		}
	}
	return out
}

func TestProcessMessageStep_SendsAndGoesOn(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const leadConv, buildConv, reviewConv = "mlea-1111-2222-3333-4444", "mbld-1111-2222-3333-4444", "mrev-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "working")
	havePerformerAgent(t, f, "crew", buildConv, "ivan", "implementor", "idle")
	havePerformerAgent(t, f, "crew", reviewConv, "rita", "reviewer", "idle")

	t.Run("to the operator and a role: both get it, with params and outputs filled in", func(t *testing.T) {
		putProcessRuntimeTemplate(t, root, messageTemplate("tell-both", []string{"operator", "role:reviewer"}, false))
		const runID = "run_message_both"
		run := runMessageProcess(t, f, buildConv, runID, "tell-both")
		require.Equal(t, engine.RunCompleted, run.Status, "the message step passes and the run goes on to its end")
		cards := operatorCards(t, runID)
		require.Len(t, cards, 1)
		assert.Equal(t, "Ready: the login fix", cards[0].Subject)
		assert.Contains(t, cards[0].Body, "the login fix is up for review: https://example.test/pr/7")
		assert.Equal(t, "tell", cards[0].ProcessNodeID)
		mail := messageMail(t, reviewConv, runID)
		require.Len(t, mail, 1, "the reviewer gets it in their inbox")
		assert.Contains(t, mail[0].Subject, "Ready: the login fix")
		assert.Contains(t, mail[0].Body, "https://example.test/pr/7")
		assert.Empty(t, messageMail(t, leadConv, runID), "nobody else does")
	})

	t.Run("a role nobody has: the step fails, names who, and the run stops there", func(t *testing.T) {
		putProcessRuntimeTemplate(t, root, messageTemplate("tell-nobody", []string{"operator", "role:designer"}, false))
		const runID = "run_message_nobody"
		run := runMessageProcess(t, f, buildConv, runID, "tell-nobody")
		require.Equal(t, engine.RunFailed, run.Status)
		require.NotNil(t, run.Failure)
		assert.Equal(t, "tell", run.Failure.NodeID)
		assert.Contains(t, run.Failure.Reason, "no member of crew has role designer")
	})

	t.Run("a step that may fail goes on", func(t *testing.T) {
		putProcessRuntimeTemplate(t, root, messageTemplate("tell-maybe", []string{"role:designer"}, true))
		const runID = "run_message_maybe"
		run := runMessageProcess(t, f, buildConv, runID, "tell-maybe")
		require.Equal(t, engine.RunCompleted, run.Status, "mayFail: a failed send goes on along its route")
	})

	t.Run("a recipient its command already reached is skipped (a resumed send)", func(t *testing.T) {
		putProcessRuntimeTemplate(t, root, messageTemplate("tell-resumed", []string{"operator", "role:reviewer"}, false))
		const runID = "run_message_resumed"
		created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
			"id": runID, "templateId": "tell-resumed", "group": "crew", "authorizeProgramProfiles": []string{},
			"params": map[string]string{"what": "the login fix"}})
		require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
		// As if an earlier, interrupted send of this very command had reached
		// the operator before the daemon stopped.
		command := fmt.Sprintf("cmd_%d_%s_%d_%s_%d_%s", len(runID), runID, len("tell"), "tell", 1, "message")
		require.NoError(t, db.RecordProcessMessageDelivery(runID, command, "operator"))
		agentd.WaitForProcessDispatchForTest()
		rec := reportAs(t, f, buildConv, map[string]any{"runId": runID, "nodeId": "build", "attempt": 1,
			"outcome": "succeeded", "outputs": map[string]string{"pr": "https://example.test/pr/8"}})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		agentd.WaitForProcessRunRuntimeForTest()
		executor.WaitTerminalHooks()
		assert.Empty(t, operatorCards(t, runID), "the operator already had it")
		assert.Len(t, messageMail(t, reviewConv, runID), 1, "the reviewer had not")
	})

	t.Run("the lead and the group: each member once", func(t *testing.T) {
		putProcessRuntimeTemplate(t, root, messageTemplate("tell-group", []string{"lead", "group"}, false))
		const runID = "run_message_group"
		run := runMessageProcess(t, f, buildConv, runID, "tell-group")
		require.Equal(t, engine.RunCompleted, run.Status)
		assert.Len(t, messageMail(t, leadConv, runID), 1, "the lead is in the group too, and gets it once")
		assert.Len(t, messageMail(t, buildConv, runID), 1)
		assert.Len(t, messageMail(t, reviewConv, runID), 1)
		assert.Empty(t, operatorCards(t, runID))
	})
}
