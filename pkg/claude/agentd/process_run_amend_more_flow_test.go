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
