package agentd_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type amendmentWire struct {
	ID        int64  `json:"id"`
	State     string `json:"state"`
	MessageID int64  `json:"messageId"`
	Summary   struct {
		Added    []string `json:"added"`
		Changed  []string `json:"changed"`
		Rerouted []string `json:"rerouted"`
	} `json:"summary"`
}

func amendBody(t *testing.T, tmpl *model.Template, note string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(tmpl)
	require.NoError(t, err)
	return map[string]any{"template": json.RawMessage(raw), "note": note}
}

// The overseer proposes inserting a step after the one being worked on; the
// operator's card says what it changes and applying it reroutes the run.
// One proposed before the run moved on is refused as stale; the operator
// amends directly.
func TestProcessAmend_OverseerProposesOperatorApprovesInTheCard(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const leadConv, devConv = "sal1-1111-2222-3333-4444", "sad1-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "working")
	havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	dev := func(prompt string) model.Node {
		return model.Node{Performer: agentStep(prompt, func(p *model.Performer) { p.Profile = "dev" })}
	}
	tmpl := chainTemplate("amendme", []string{"work", "ship"}, map[string]model.Node{
		"work": dev("Build it."),
		"ship": dev("Ship it."),
	})
	putProcessRuntimeTemplate(t, root, tmpl)
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_am1", "templateId": "amendme", "group": "crew", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessDispatchForTest()

	// The run's copy as printed for editing reads back as itself: sent back
	// unchanged, it changes nothing (and no field differs only by being empty).
	var printed struct {
		YAML string `json:"yaml"`
	}
	got := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_am1/template", nil)
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	testharness.DecodeJSON(t, got, &printed)
	same := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_am1/amendments", map[string]any{"yaml": printed.YAML, "dryRun": true})
	assert.Equal(t, http.StatusUnprocessableEntity, same.Code, same.Body.String())
	assert.Contains(t, same.Body.String(), "changes nothing")

	// The run's template, with a security step between work and ship.
	amended := cloneProcessTemplate(tmpl)
	work := amended.Nodes["work"]
	work.Next = model.Next{model.DefaultOutcome: "security"}
	amended.Nodes["work"] = work
	security := dev("Check it for secrets.")
	security.Next = model.Next{model.DefaultOutcome: "ship"}
	security.Type = model.NodeTypeTask
	amended.Nodes["security"] = security

	startedEdit := cloneProcessTemplate(amended)
	busy := startedEdit.Nodes["work"]
	busy.Performer = agentStep("Build something else.", func(p *model.Performer) { p.Profile = "dev" })
	startedEdit.Nodes["work"] = busy
	refused := agentReq(t, f, leadConv, http.MethodPost, "/v1/process/runs/run_am1/amendments", amendBody(t, startedEdit, "rename"))
	assert.Equal(t, http.StatusUnprocessableEntity, refused.Code, refused.Body.String())
	assert.Contains(t, refused.Body.String(), "step work has started")
	stranger := agentReq(t, f, devConv, http.MethodPost, "/v1/process/runs/run_am1/amendments", amendBody(t, amended, "mine"))
	assert.Equal(t, http.StatusForbidden, stranger.Code, "only the overseer proposes without manage")

	proposed := agentReq(t, f, leadConv, http.MethodPost, "/v1/process/runs/run_am1/amendments", amendBody(t, amended, "Needs a secrets scan."))
	require.Equal(t, http.StatusCreated, proposed.Code, proposed.Body.String())
	var first struct {
		Amendment amendmentWire `json:"amendment"`
	}
	testharness.DecodeJSON(t, proposed, &first)
	assert.Equal(t, "proposed", first.Amendment.State)
	require.NotZero(t, first.Amendment.MessageID)
	assert.Equal(t, []string{"security"}, first.Amendment.Summary.Added)
	assert.Equal(t, []string{"work"}, first.Amendment.Summary.Rerouted)
	card, err := db.GetHumanMessage(first.Amendment.MessageID)
	require.NoError(t, err)
	assert.Contains(t, card.Body, "Steps added: security")
	assert.Contains(t, card.Body, "Steps rerouted: work")
	assert.Contains(t, card.Body, "Needs a secrets scan.")

	// A second proposal is formed now, then the run moves on.
	second := agentReq(t, f, leadConv, http.MethodPost, "/v1/process/runs/run_am1/amendments", amendBody(t, amended, "Same, again."))
	require.Equal(t, http.StatusCreated, second.Code, second.Body.String())
	var stale struct {
		Amendment amendmentWire `json:"amendment"`
	}
	testharness.DecodeJSON(t, second, &stale)

	var before struct {
		Amendments []struct {
			ID       int64 `json:"id"`
			Outdated bool  `json:"outdated"`
		} `json:"amendments"`
	}
	testharness.DecodeJSON(t, processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_am1", nil), &before)
	for _, a := range before.Amendments {
		assert.False(t, a.Outdated, "a proposal at the run's current state is not outdated")
	}
	pickOnCard(t, first.Amendment.MessageID, "Apply amendment", "", http.StatusOK)
	agentd.WaitForProcessDispatchForTest()
	pickOnCard(t, stale.Amendment.MessageID, "Apply amendment", "", http.StatusConflict)

	reportStep(t, f, devConv, "run_am1", "work", 1, nil)
	agentd.WaitForProcessDispatchForTest()
	assert.Contains(t, lastInboxSubject(t, devConv), "security (attempt 1)", "the run goes through the inserted step")
	var after struct {
		Amended    bool            `json:"amended"`
		Amendments []amendmentWire `json:"amendments"`
	}
	shown := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_am1", nil)
	testharness.DecodeJSON(t, shown, &after)
	assert.True(t, after.Amended)
	states := map[int64]string{}
	for _, a := range after.Amendments {
		states[a.ID] = a.State
	}
	assert.Equal(t, map[int64]string{first.Amendment.ID: "applied", stale.Amendment.ID: "stale"}, states)
	var mailed bool
	for _, m := range processInbox(t, leadConv) {
		mailed = mailed || strings.Contains(m.Subject, "amendment #"+jsonInt(first.Amendment.ID)+" to run_am1 applied")
	}
	assert.True(t, mailed, "the proposer hears it was applied")

	// The operator amends directly: ship's prompt changes.
	direct := cloneProcessTemplate(amended)
	ship := direct.Nodes["ship"]
	ship.Performer = agentStep("Ship it behind a flag.", func(p *model.Performer) { p.Profile = "dev" })
	direct.Nodes["ship"] = ship
	applied := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_am1/amendments", amendBody(t, direct, ""))
	require.Equal(t, http.StatusAccepted, applied.Code, applied.Body.String())
	reportStep(t, f, devConv, "run_am1", "security", 1, nil)
	agentd.WaitForProcessDispatchForTest()
	inbox := processInbox(t, devConv)
	idx := slices.IndexFunc(inbox, func(m *db.AgentMessage) bool { return strings.Contains(m.Subject, "ship (attempt 1)") })
	require.GreaterOrEqual(t, idx, 0)
	assert.Contains(t, inbox[idx].Body, "Ship it behind a flag.")
}

func cloneProcessTemplate(tmpl *model.Template) *model.Template {
	out := *tmpl
	out.Nodes = make(map[string]model.Node, len(tmpl.Nodes))
	for id, node := range tmpl.Nodes {
		out.Nodes[id] = node
	}
	return &out
}
