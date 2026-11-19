package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// What a process run waits on a person for reaches the operator as a
// Messages card: put up when the run starts waiting, answered from the card
// or anywhere else, and closed when it is settled or withdrawn.

type waitingCardRow struct {
	ID            int64    `json:"id"`
	FromTitle     string   `json:"from_title"`
	Subject       string   `json:"subject"`
	Body          string   `json:"body"`
	Options       []string `json:"options"`
	Decision      string   `json:"decision"`
	DismissedBy   string   `json:"dismissed_by"`
	DismissReason string   `json:"dismiss_reason"`
	ProcessRun    string   `json:"process_run"`
	ProcessCard   *struct {
		Outputs []string `json:"outputs"`
		Passing []string `json:"passing"`
	} `json:"process_card"`
}

// waitingCards reads the operator's Messages as the dashboard does, after
// the run's card reconciles have finished.
func waitingCards(t *testing.T, mux http.Handler, runID string) []waitingCardRow {
	t.Helper()
	executor.WaitTerminalHooks()
	page := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/mailbox?id=human", nil))
	require.Equal(t, http.StatusOK, page.Code, page.Body.String())
	var p struct {
		Messages []waitingCardRow `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(page.Body.Bytes(), &p))
	var out []waitingCardRow
	for _, m := range p.Messages {
		if m.ProcessRun == runID {
			out = append(out, m)
		}
	}
	return out
}

// Scenario: a person's step puts up a card with its ask and its choices; Done
// on the card finishes the step, and the card shows the pick.
func TestProcessWaitingCard_PersonStepAnsweredFromTheCard(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("stall")
	const worker = "pwca-1111-2222-3333-4444"
	havePerformerAgent(t, f, "stall", worker, "alice", "dev", "idle")
	mux := dashMessageMux(t)
	person := model.Performer{Kind: model.PerformerHuman, Ask: "Pack the order for {{ params.customer }}."}
	putProcessRuntimeTemplate(t, root, personStepTemplate("card-person", person, nil))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_card_person", "templateId": "card-person", "authorizeProgramProfiles": []string{}, "group": g.Name,
		"params": map[string]string{"customer": "Ada"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	cards := waitingCards(t, mux, "run_card_person")
	require.Len(t, cards, 1, "the step waiting on the operator puts up one card")
	card := cards[0]
	assert.Equal(t, "Waiting on you: pack · card-person", card.Subject)
	assert.NotContains(t, card.Body, "run_card_person", "the card spells out no run id")
	assert.Contains(t, card.Body, "Pack the order for Ada.")
	assert.Equal(t, []string{"Done", "Couldn't do it"}, card.Options)
	assert.Empty(t, card.Decision)

	reply := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/human-messages/reply",
		map[string]any{"id": card.ID, "choice": 1}))
	require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
	view := showPersonStepRun(t, f, "run_card_person")
	assert.Empty(t, view.AwaitingDecisions, "the card's Done finished the step")
	require.Len(t, view.Commands, 1)
	assert.Equal(t, "ship", view.Commands[0].NodeID, "the run went on to the next step")

	cards = waitingCards(t, mux, "run_card_person")
	require.Len(t, cards, 1, "a settled step puts up nothing more")
	assert.Equal(t, "Done", cards[0].Decision)
}

// Scenario: a decision's card closes with the verdict when it is decided in
// the run view, and a step's card is withdrawn when the run is canceled.
func TestProcessWaitingCard_ClosedWhenSettledElsewhereOrWithdrawn(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("stall")
	mux := dashMessageMux(t)
	person := model.Performer{Kind: model.PerformerHuman, Ask: "Check the shipment."}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "card-decide", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "choose"}},
			"choose": {Type: model.NodeTypeDecision, Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Ship today?"},
				Next: model.Next{"yes": "check", "no": "end"}},
			"check": {Type: model.NodeTypeTask, Performer: &person, Captures: []string{"weight"}, Next: model.Next{"next": "end"}},
			"end":   {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
	putProcessRuntimeTemplate(t, root, tmpl)
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_card_decide", "templateId": "card-decide", "authorizeProgramProfiles": []string{}, "group": g.Name,
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	cards := waitingCards(t, mux, "run_card_decide")
	require.Len(t, cards, 1)
	assert.Contains(t, cards[0].Subject, "Your decision: choose")
	assert.Contains(t, cards[0].Body, "Ship today?")
	assert.ElementsMatch(t, []string{"yes", "no"}, cards[0].Options)

	require.Less(t, decidePersonStep(t, f, "run_card_decide", map[string]any{"nodeId": "choose", "verdict": "yes"}), 300)
	cards = waitingCards(t, mux, "run_card_decide")
	require.Len(t, cards, 2, "the decision's card closed and the next step put up its own")
	var decision, step waitingCardRow
	for _, c := range cards {
		if strings.HasPrefix(c.Subject, "Your decision:") {
			decision = c
		} else {
			step = c
		}
	}
	assert.Equal(t, "yes", decision.Decision, "decided in the run view, the card shows the verdict")
	assert.Contains(t, step.Subject, "Waiting on you: check")
	require.NotNil(t, step.ProcessCard, "a waiting card says what the dashboard needs of it")
	assert.Equal(t, []string{"weight"}, step.ProcessCard.Outputs, "the card sends Done to the run, where weight is filled in")
	assert.Equal(t, []string{"Done"}, step.ProcessCard.Passing)
	assert.NotContains(t, step.Subject, "run_card_decide", "the card spells out no run id")

	// Done from the card needs the output it asks for.
	doneAt := 0
	for i, option := range step.Options {
		if option == "Done" {
			doneAt = i + 1
		}
	}
	reply := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/human-messages/reply",
		map[string]any{"id": step.ID, "choice": doneAt}))
	assert.Equal(t, http.StatusConflict, reply.Code, reply.Body.String())
	assert.Contains(t, reply.Body.String(), "weight")

	rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_card_decide/cancel", map[string]any{})
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.Equal(t, engine.RunCanceled, showPersonStepRun(t, f, "run_card_decide").Status)
	for _, c := range waitingCards(t, mux, "run_card_decide") {
		if c.ID == step.ID {
			assert.Empty(t, c.Decision)
			assert.Equal(t, "process", c.DismissedBy)
			assert.Equal(t, "the run was canceled", c.DismissReason)
		}
	}
}
