package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The run graph (process context) follows the run's own copy once it is
// amended, and another run of the same template keeps the template's graph.
func TestProcessAmend_RunGraphFollowsTheAmendedCopy(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const leadConv, devConv = "sal1-1111-2222-3333-4444", "sad1-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "working")
	havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	dev := func(prompt string) model.Node {
		return model.Node{Performer: agentStep(prompt, func(p *model.Performer) { p.Profile = "dev" })}
	}
	tmpl := chainTemplate("amendme", []string{"work", "ship"}, map[string]model.Node{
		"work": dev("Build it."), "ship": dev("Ship it."),
	})
	putProcessRuntimeTemplate(t, root, tmpl)
	for _, id := range []string{"run_am1", "run_am2"} {
		created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
			"id": id, "templateId": "amendme", "group": "crew", "authorizeProgramProfiles": []string{},
		})
		require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	}
	agentd.WaitForProcessDispatchForTest()
	type ctxResp struct {
		Runs map[string]struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"runs"`
	}
	nodes := func(run string) []string {
		var c ctxResp
		testharness.DecodeJSON(t, processRuntimeRequest(t, f, http.MethodGet, "/v1/process/context", nil), &c)
		var out []string
		for _, n := range c.Runs[run].Nodes {
			out = append(out, n.ID)
		}
		return out
	}
	amended := cloneProcessTemplate(tmpl)
	work := amended.Nodes["work"]
	work.Next = model.Next{model.DefaultOutcome: "security"}
	amended.Nodes["work"] = work
	security := dev("Check it.")
	security.Type = model.NodeTypeTask
	security.Next = model.Next{model.DefaultOutcome: "ship"}
	amended.Nodes["security"] = security
	applied := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_am1/amendments", amendBody(t, amended, ""))
	require.Equal(t, http.StatusAccepted, applied.Code, applied.Body.String())
	got := nodes("run_am1")
	require.Contains(t, got, "security", "the amended run's graph shows the inserted step")
	require.NotContains(t, nodes("run_am2"), "security", "another run of the template is untouched")
}

// process.templates.manage applies an amendment at once, the overseer's own
// included.
func TestProcessAmend_TemplatesManageAppliesDirectly(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const leadConv, devConv, tplConv = "sal1-1111-2222-3333-4444", "sad1-1111-2222-3333-4444", "sat1-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "working")
	havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	havePerformerAgent(t, f, "crew", tplConv, "tina", "scribe", "idle")
	require.NoError(t, db.GrantAgentPermission(tplConv, agentd.PermProcessTemplatesManage, "human"))
	dev := func(prompt string) model.Node {
		return model.Node{Performer: agentStep(prompt, func(p *model.Performer) { p.Profile = "dev" })}
	}
	tmpl := chainTemplate("amendme", []string{"work", "ship"}, map[string]model.Node{
		"work": dev("Build it."), "ship": dev("Ship it."),
	})
	putProcessRuntimeTemplate(t, root, tmpl)
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_am1", "templateId": "amendme", "group": "crew", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessDispatchForTest()
	amended := cloneProcessTemplate(tmpl)
	ship := amended.Nodes["ship"]
	ship.Performer = agentStep("Ship it carefully.", func(p *model.Performer) { p.Profile = "dev" })
	amended.Nodes["ship"] = ship
	res := agentReq(t, f, tplConv, http.MethodPost, "/v1/process/runs/run_am1/amendments", amendBody(t, amended, ""))
	require.Equal(t, http.StatusAccepted, res.Code, res.Body.String())
	require.NoError(t, db.GrantAgentPermission(leadConv, agentd.PermProcessTemplatesManage, "human"))
	again := cloneProcessTemplate(amended)
	ship = again.Nodes["ship"]
	ship.Performer = agentStep("Ship it very carefully.", func(p *model.Performer) { p.Profile = "dev" })
	again.Nodes["ship"] = ship
	res2 := agentReq(t, f, leadConv, http.MethodPost, "/v1/process/runs/run_am1/amendments", amendBody(t, again, ""))
	require.Equal(t, http.StatusAccepted, res2.Code, res2.Body.String())
}

// An applied amendment is saved back as its template's next version, once,
// by someone with process.templates.manage; runs started after follow it.
func TestProcessAmend_SaveBackToTheTemplate(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const leadConv, devConv = "sbl1-1111-2222-3333-4444", "sbd1-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "working")
	havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	dev := func(prompt string) model.Node {
		return model.Node{Performer: agentStep(prompt, func(p *model.Performer) { p.Profile = "dev" })}
	}
	tmpl := chainTemplate("amendme", []string{"work", "ship"}, map[string]model.Node{
		"work": dev("Build it."), "ship": dev("Ship it."),
	})
	putProcessRuntimeTemplate(t, root, tmpl)
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_sb1", "templateId": "amendme", "group": "crew", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessDispatchForTest()
	amended := cloneProcessTemplate(tmpl)
	ship := amended.Nodes["ship"]
	ship.Performer = agentStep("Ship it behind a flag.", func(p *model.Performer) { p.Profile = "dev" })
	amended.Nodes["ship"] = ship
	applied := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_sb1/amendments", amendBody(t, amended, "flags first"))
	require.Equal(t, http.StatusAccepted, applied.Code, applied.Body.String())
	var view struct {
		Amendments []struct {
			ID       int64 `json:"id"`
			Saveable bool  `json:"saveable"`
		} `json:"amendments"`
	}
	testharness.DecodeJSON(t, processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_sb1", nil), &view)
	require.Len(t, view.Amendments, 1)
	require.True(t, view.Amendments[0].Saveable)
	path := "/v1/process/runs/run_sb1/amendments/" + jsonInt(view.Amendments[0].ID) + "/save-back"

	denied := agentReq(t, f, leadConv, http.MethodPost, path, map[string]any{})
	require.Equal(t, http.StatusForbidden, denied.Code, "the overseer alone does not edit the template")
	saved := processRuntimeRequest(t, f, http.MethodPost, path, map[string]any{})
	require.Equal(t, http.StatusOK, saved.Code, saved.Body.String())
	again := processRuntimeRequest(t, f, http.MethodPost, path, map[string]any{})
	require.Equal(t, http.StatusConflict, again.Code, "saved once")

	// The next run starts from the saved version.
	next := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_sb2", "templateId": "amendme", "group": "crew", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, next.Code, next.Body.String())
	var printed struct {
		YAML string `json:"yaml"`
	}
	testharness.DecodeJSON(t, processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_sb2/template", nil), &printed)
	require.Contains(t, printed.YAML, "Ship it behind a flag.")
	view.Amendments = nil
	testharness.DecodeJSON(t, processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_sb1", nil), &view)
	require.False(t, view.Amendments[0].Saveable)

	// A run of an older version cannot overwrite the template's newer edits.
	amended2 := cloneProcessTemplate(tmpl)
	ship = amended2.Nodes["ship"]
	ship.Performer = agentStep("Ship it on Friday.", func(p *model.Performer) { p.Profile = "dev" })
	amended2.Nodes["ship"] = ship
	created3 := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_sb3", "templateId": "amendme", "group": "crew", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created3.Code, created3.Body.String())
	putProcessRuntimeTemplate(t, root, amended2) // someone edits the template meanwhile
	amended3 := cloneProcessTemplate(amended)
	ship = amended3.Nodes["ship"]
	ship.Performer = agentStep("Ship it slowly.", func(p *model.Performer) { p.Profile = "dev" })
	amended3.Nodes["ship"] = ship
	applied3 := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_sb3/amendments", amendBody(t, amended3, ""))
	require.Equal(t, http.StatusAccepted, applied3.Code, applied3.Body.String())
	view.Amendments = nil
	testharness.DecodeJSON(t, processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_sb3", nil), &view)
	moved := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_sb3/amendments/"+jsonInt(view.Amendments[0].ID)+"/save-back", map[string]any{})
	require.Equal(t, http.StatusConflict, moved.Code, moved.Body.String())
	require.Contains(t, moved.Body.String(), "changed since this run started")
}

// The graph editor reads the run's copy as its edit model, with the steps the
// run reached named, and sends its edit back as an amendment.
func TestProcessAmend_GraphEditorModelRoundTrips(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const leadConv, devConv = "sge1-1111-2222-3333-4444", "sgd1-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "working")
	havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	dev := func(prompt string) model.Node {
		return model.Node{Performer: agentStep(prompt, func(p *model.Performer) { p.Profile = "dev" })}
	}
	tmpl := chainTemplate("amendme", []string{"work", "ship"}, map[string]model.Node{
		"work": dev("Build it."), "ship": dev("Ship it."),
	})
	putProcessRuntimeTemplate(t, root, tmpl)
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_ge1", "templateId": "amendme", "group": "crew", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessDispatchForTest()
	var got struct {
		EditView struct {
			Template map[string]any   `json:"template"`
			Edges    []map[string]any `json:"edges"`
			Layout   any              `json:"layout"`
		} `json:"editView"`
		Reached    []string `json:"reached"`
		Reroutable []string `json:"reroutable"`
	}
	testharness.DecodeJSON(t, processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_ge1/template", nil), &got)
	require.Contains(t, got.Reached, "work")
	require.Contains(t, got.Reroutable, "work", "work is out: where it leads is not settled yet")
	require.NotContains(t, got.Reached, "ship")

	// Unchanged, it changes nothing.
	same := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_ge1/amendments", map[string]any{"edit": got.EditView, "dryRun": true})
	require.Equal(t, http.StatusUnprocessableEntity, same.Code, same.Body.String())
	require.Contains(t, same.Body.String(), "changes nothing")

	// Insert a step after work, as the editor would: a node and the edges.
	nodes := got.EditView.Template["nodes"].(map[string]any)
	nodes["security"] = map[string]any{"type": "task", "name": "Security review",
		"performer": map[string]any{"kind": "agent", "profile": "dev", "prompt": "Check it."}}
	for _, e := range got.EditView.Edges {
		if e["from"] == "work" {
			e["to"] = "security"
		}
	}
	got.EditView.Edges = append(got.EditView.Edges, map[string]any{"from": "security", "outcome": "default", "to": "ship"})
	checked := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_ge1/amendments", map[string]any{"edit": got.EditView, "dryRun": true})
	require.Equal(t, http.StatusOK, checked.Code, checked.Body.String())
	require.Contains(t, checked.Body.String(), `"added":["security"]`)
	require.Contains(t, checked.Body.String(), `"rerouted":["work"]`)
	applied := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_ge1/amendments", map[string]any{"edit": got.EditView})
	require.Equal(t, http.StatusAccepted, applied.Code, applied.Body.String())
}
