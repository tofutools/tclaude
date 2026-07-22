package agentd_test

// Flow test for the node edit dialogs (TCL-298). The dialog UI is JS (its
// pure edit path — ProcessEditModel.updateNode + the process-node-form
// helpers — is unit-tested in jstest/process-node-form.test.mjs); this test
// exercises the exact wire flow those edits produce: GET the edit model,
// apply the dialog's mutations (performer kind switch with kind-scoped
// pruning, retry policy, plan/review stages, captures), POST, and assert
// through the templates REST surface that the new version's canonical YAML
// carries every change.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestProcessNodeDialogEditPathSaveRoundTrip(t *testing.T) {
	f, root := processAuthoringFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	seed := &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "dialog-edits",
		Name:       "Dialog edits",
		Start:      "work",
		Nodes: map[string]model.Node{
			"work": {
				Type: model.NodeTypeTask, Name: "Work",
				Performer: &model.Performer{Kind: model.PerformerAgent, Profile: "dev", Prompt: "Do the thing"},
				Retry:     &model.RetryPolicy{MaxAttempts: 3, OnFail: "feedback-same-session"},
				Next:      model.Next{"pass": "done"},
			},
			"done": {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
	_, err = fs.PutTemplate(t.Context(), seed)
	require.NoError(t, err)

	getRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/dialog-edits", nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var edit processEditResponse
	testharness.DecodeJSON(t, getRec, &edit)

	// The dialog's edits, expressed on the wire model exactly as the
	// updateNode gate leaves the node:
	work := edit.Template.Nodes["work"]
	// Work performer kind agent → program: kind-scoped fields are pruned
	// (prompt gone), common fields (profile) survive, program fields land.
	work.Performer = &model.Performer{
		Kind: model.PerformerProgram, Profile: "dev",
		Run: "go", Args: []string{"test", "./..."},
		Contact: &model.ContactSchedule{Cadence: "10m", Budget: 3, EscalationTarget: "human:operator"},
	}
	// Retry policy: max attempts + on_fail flipped to fresh-attempt.
	work.Retry = &model.RetryPolicy{MaxAttempts: 5, OnFail: "fresh-attempt"}
	// Plan stage enabled with a human approval gate; review gate with the
	// human-scoped fields (choices/assignee).
	work.Plan = &model.Step{
		ID: "plan", Approval: "human",
		Performer: model.Performer{Kind: model.PerformerAgent, Profile: "dev", Prompt: "Plan the thing", Model: "opus", Effort: "high"},
	}
	work.Review = &model.Step{
		ID: "sign-off",
		Performer: model.Performer{
			Kind: model.PerformerHuman, Profile: "operator", Ask: "Ship it?",
			Choices: []string{"ship", "hold"}, ChoiceOutcomes: map[string]string{"ship": "pass", "hold": "fail"}, Assignee: "johan",
		},
	}
	work.Captures = []string{"diff", "test-report"}
	edit.Template.Nodes["work"] = work

	saveRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/dialog-edits", edit)
	require.Equal(t, http.StatusCreated, saveRec.Code, saveRec.Body.String())
	// The new fields are legal on their kinds: the advisory diagnostics must
	// not flag kind scoping for this save.
	assert.NotContains(t, saveRec.Body.String(), "kind_scoped_field")
	assert.NotContains(t, saveRec.Body.String(), "captures_on_non_task_node")

	// Assert through the REST read path: the reloaded head's edit model and
	// canonical YAML both carry the dialog's changes.
	reload := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/dialog-edits", nil)
	require.Equal(t, http.StatusOK, reload.Code, reload.Body.String())
	var next processEditResponse
	testharness.DecodeJSON(t, reload, &next)

	saved := next.Template.Nodes["work"]
	require.NotNil(t, saved.Performer)
	assert.Equal(t, model.PerformerProgram, saved.Performer.Kind)
	assert.Equal(t, "dev", saved.Performer.Profile)
	assert.Empty(t, saved.Performer.Prompt, "agent-scoped prompt must not survive the kind switch")
	assert.Equal(t, []string{"test", "./..."}, saved.Performer.Args)
	assert.Equal(t, &model.RetryPolicy{MaxAttempts: 5, OnFail: "fresh-attempt"}, saved.Retry)
	assert.Equal(t, []string{"diff", "test-report"}, saved.Captures)
	require.NotNil(t, saved.Plan)
	assert.Equal(t, "human", saved.Plan.Approval)
	assert.Equal(t, "opus", saved.Plan.Performer.Model)
	require.NotNil(t, saved.Review)
	assert.Equal(t, []string{"ship", "hold"}, saved.Review.Performer.Choices)
	assert.Equal(t, "johan", saved.Review.Performer.Assignee)

	for _, needle := range []string{
		"kind: program", "run: go", "maxAttempts: 5", "onFail: fresh-attempt",
		"captures:", "- diff", "- test-report",
		"approval: human", "model: opus", "effort: high",
		"choices:", "- ship", "- hold", "choiceOutcomes:", "ship: pass", "hold: fail", "assignee: johan",
		"cadence: 10m", "escalationTarget: human:operator",
	} {
		assert.Contains(t, next.Source, needle, "canonical YAML must carry the dialog edit %q", needle)
	}
	assert.NotEqual(t, edit.SemanticHash, next.SemanticHash, "stage edits are semantic changes")
}
