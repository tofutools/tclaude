package agentd_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The in-context read model (tcl-f2cpfb): GET /v1/process/context projects
// scoped runs per group and per agent as bounded mini-graphs. These tests
// read it the way the dashboard overlays do.

type processContextWire struct {
	Runs   map[string]processProjectionWire `json:"runs"`
	Groups []struct {
		GroupID       int64    `json:"groupId"`
		Group         string   `json:"group"`
		Active        []string `json:"active"`
		NeedsYou      []string `json:"needsYou"`
		Done          []string `json:"done"`
		Planned       []any    `json:"planned"`
		ActiveCount   int      `json:"activeCount"`
		NeedsYouCount int      `json:"needsYouCount"`
	} `json:"groups"`
	Agents []struct {
		AgentID string `json:"agentId"`
		Runs    []struct {
			RunID  string `json:"runId"`
			NodeID string `json:"nodeId"`
			Index  int    `json:"index"`
			Total  int    `json:"total"`
			State  string `json:"state"`
		} `json:"runs"`
	} `json:"agents"`
	Orphaned []string `json:"orphaned"`
}

type processProjectionWire struct {
	RunID    string              `json:"runId"`
	Name     string              `json:"name"`
	Status   string              `json:"status"`
	Action   string              `json:"action"`
	NeedsYou bool                `json:"needsYou"`
	Scope    processRunScopeWire `json:"scope"`
	Nodes    []struct {
		ID        string `json:"id"`
		Label     string `json:"label"`
		Kind      string `json:"kind"`
		State     string `json:"state"`
		Index     int    `json:"index"`
		Performer struct {
			Kind string `json:"kind"`
			Ref  string `json:"ref"`
		} `json:"performer"`
	} `json:"nodes"`
	Edges []struct {
		From  string `json:"from"`
		To    string `json:"to"`
		Label string `json:"label"`
		State string `json:"state"`
		Back  bool   `json:"back"`
	} `json:"edges"`
	Current []string `json:"current"`
}

func readProcessContext(t *testing.T, f *testharness.Flow, convID string) processContextWire {
	t.Helper()
	var rec = processRuntimeRequest(t, f, http.MethodGet, "/v1/process/context", nil)
	if convID != "" {
		rec = agentReq(t, f, convID, http.MethodGet, "/v1/process/context", nil)
	}
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view processContextWire
	testharness.DecodeJSON(t, rec, &view)
	for _, g := range view.Groups {
		for _, id := range append(append(append([]string{}, g.Active...), g.NeedsYou...), g.Done...) {
			require.Contains(t, view.Runs, id, "every referenced run resolves in the runs map")
		}
	}
	return view
}

func startScopedRun(t *testing.T, f *testharness.Flow, id, templateID string, scope map[string]any, profiles []string) {
	t.Helper()
	body := map[string]any{"id": id, "templateId": templateID, "authorizeProgramProfiles": profiles}
	for k, v := range scope {
		body[k] = v
	}
	rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// Scenario: a group holds one run executing a program and one waiting on a
// human; an agent holds a run bound to it. The group lists them as active and
// needs-you, each with its current node, performer and edges; the agent entry
// places the agent at its step; unscoped runs stay out of context.
func TestProcessContextProjectsGroupAndAgentRuns(t *testing.T) {
	f := scopedRunFlow(t)
	gate := newProcessProgramGate(t)
	t.Cleanup(gate.releaseAll)
	g := f.HaveGroup("ctx-team")
	const workerConv = "pcxw-1111-2222-3333-4444"
	f.HaveMember("ctx-team", workerConv)
	workerID := agentIDOf(t, workerConv)

	startScopedRun(t, f, "run_ctx_prog", "prog", map[string]any{"group": "ctx-team"}, []string{"safe"})
	gate.awaitEntered(t, 1)
	startScopedRun(t, f, "run_ctx_gate", "gate", map[string]any{"group": "ctx-team"}, []string{})
	startScopedRun(t, f, "run_ctx_agent", "gate", map[string]any{"agent": workerID}, []string{})
	startScopedRun(t, f, "run_ctx_none", "gate", nil, []string{})

	view := readProcessContext(t, f, "")
	require.Len(t, view.Groups, 1)
	group := view.Groups[0]
	assert.Equal(t, g.ID, group.GroupID)
	assert.Equal(t, "ctx-team", group.Group)
	assert.Equal(t, []string{"run_ctx_prog"}, group.Active)
	assert.Equal(t, []string{"run_ctx_gate"}, group.NeedsYou)
	assert.Equal(t, 1, group.ActiveCount)
	assert.Equal(t, 1, group.NeedsYouCount)
	assert.Empty(t, group.Planned)
	assert.NotContains(t, view.Runs, "run_ctx_none", "unscoped runs stay out of context")

	prog := view.Runs["run_ctx_prog"]
	assert.False(t, prog.NeedsYou)
	assert.Equal(t, []string{"task-01"}, prog.Current)
	task := prog.Nodes[1]
	assert.Equal(t, "task-01", task.ID)
	assert.Equal(t, "task", task.Kind)
	assert.Equal(t, "running", task.State)
	assert.Equal(t, 1, task.Index)
	assert.Equal(t, "program", task.Performer.Kind)
	assert.Equal(t, "safe", task.Performer.Ref)
	assert.Equal(t, "start", prog.Nodes[0].Kind)
	require.NotEmpty(t, prog.Edges)
	assert.Equal(t, "start", prog.Edges[0].From)
	assert.Equal(t, "arrived", prog.Edges[0].State)

	waiting := view.Runs["run_ctx_gate"]
	assert.True(t, waiting.NeedsYou)
	assert.Equal(t, "awaiting_decision", waiting.Action)
	assert.Equal(t, []string{"choose"}, waiting.Current)
	assert.Equal(t, "awaiting", waiting.Nodes[1].State)
	assert.Equal(t, "human", waiting.Nodes[1].Performer.Kind)
	assert.Equal(t, "gate", waiting.Name, "an unnamed template falls back to its id")
	assert.Equal(t, "choose", waiting.Nodes[1].Label, "an unnamed node falls back to its id")
	assert.Equal(t, 1, waiting.Nodes[1].Index)
	assert.Equal(t, "done", waiting.Nodes[0].State)
	assert.Equal(t, "end", waiting.Nodes[2].Kind)
	assert.Equal(t, processRunScopeWire{Kind: "group", GroupID: g.ID, Group: "ctx-team",
		Owner: "human:operator", State: "bound"}, waiting.Scope)
	require.Len(t, waiting.Edges, 3)
	assert.Equal(t, "approve", waiting.Edges[1].Label)
	assert.Equal(t, "end", waiting.Edges[1].To)
	assert.Equal(t, "unresolved", waiting.Edges[1].State)
	assert.False(t, waiting.Edges[1].Back)

	require.Len(t, view.Agents, 1)
	assert.Equal(t, workerID, view.Agents[0].AgentID)
	require.Len(t, view.Agents[0].Runs, 1)
	step := view.Agents[0].Runs[0]
	assert.Equal(t, "run_ctx_agent", step.RunID)
	assert.Equal(t, "choose", step.NodeID)
	assert.Equal(t, 1, step.Index)
	assert.Equal(t, 1, step.Total)
	assert.Equal(t, "awaiting", step.State)
	assert.Contains(t, view.Runs, "run_ctx_agent")
}

// Scenario: an agent without process.runs.read sees only its own groups' runs
// and the runs bound to itself; a group owner (who holds the read) sees all.
func TestProcessContextVisibility(t *testing.T) {
	f := scopedRunFlow(t)
	f.HaveGroup("ctx-mine")
	other := f.HaveGroup("ctx-theirs")
	const memberConv = "pcxm-1111-2222-3333-4444"
	const strangerConv = "pcxs-1111-2222-3333-4444"
	const ownerConv = "pcxo-1111-2222-3333-4444"
	f.HaveMember("ctx-mine", memberConv)
	f.HaveMember("ctx-theirs", strangerConv)
	f.HaveMember("ctx-theirs", ownerConv)
	require.NoError(t, db.AddAgentGroupOwner(other.ID, ownerConv, "test"))

	startScopedRun(t, f, "run_vis_mine", "gate", map[string]any{"group": "ctx-mine"}, []string{})
	startScopedRun(t, f, "run_vis_theirs", "gate", map[string]any{"group": "ctx-theirs"}, []string{})
	startScopedRun(t, f, "run_vis_self", "gate", map[string]any{"agent": agentIDOf(t, memberConv)}, []string{})
	startScopedRun(t, f, "run_vis_stranger", "gate", map[string]any{"agent": agentIDOf(t, strangerConv)}, []string{})

	member := readProcessContext(t, f, memberConv)
	assert.ElementsMatch(t, []string{"run_vis_mine", "run_vis_self"}, mapKeysOf(member.Runs))
	require.Len(t, member.Groups, 1)
	assert.Equal(t, "ctx-mine", member.Groups[0].Group)

	owner := readProcessContext(t, f, ownerConv)
	assert.ElementsMatch(t, []string{"run_vis_mine", "run_vis_theirs", "run_vis_self", "run_vis_stranger"},
		mapKeysOf(owner.Runs), "process.runs.read (owner-implied) sees every scoped run")
}

// Scenario: a group with more needs-you runs than the per-group bound lists
// the bound and reports the true count; an orphaned run moves to the
// top-level orphaned list.
func TestProcessContextBoundsAndOrphans(t *testing.T) {
	f := scopedRunFlow(t)
	f.HaveGroup("ctx-busy")
	f.HaveGroup("ctx-gone")
	for i := range 10 {
		startScopedRun(t, f, fmt.Sprintf("run_busy_%02d", i), "gate", map[string]any{"group": "ctx-busy"}, []string{})
	}
	startScopedRun(t, f, "run_gone", "gate", map[string]any{"group": "ctx-gone"}, []string{})
	require.NoError(t, db.DeleteAgentGroup("ctx-gone"))

	view := readProcessContext(t, f, "")
	require.Len(t, view.Groups, 1)
	assert.Len(t, view.Groups[0].NeedsYou, 8, "each bucket is bounded")
	assert.Equal(t, 10, view.Groups[0].NeedsYouCount, "the count stays true")
	assert.Equal(t, []string{"run_gone"}, view.Orphaned)
	assert.Equal(t, "orphaned", view.Runs["run_gone"].Action)
	assert.True(t, view.Runs["run_gone"].NeedsYou)
	assert.Empty(t, view.Groups[0].Done)

	decide := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_busy_00/decide",
		map[string]any{"nodeId": "choose", "verdict": "approve"})
	require.Equal(t, http.StatusAccepted, decide.Code, decide.Body.String())
	view = readProcessContext(t, f, "")
	assert.Equal(t, []string{"run_busy_00"}, view.Groups[0].Done, "a finished run moves to done")
	assert.Equal(t, "completed", view.Runs["run_busy_00"].Status)
	assert.Equal(t, 9, view.Groups[0].NeedsYouCount)
}

// Scenario: the read model is part of the processes feature; with the flag
// off the route is the same stable 404 as every other process route.
func TestProcessContextFeatureGated(t *testing.T) {
	f := scopedRunFlow(t)
	require.NoError(t, config.Save(&config.Config{}))
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/context", nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "processes_disabled")
}

func mapKeysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

// Scenario: a decision whose approve branch skips ahead to a node the reject
// branch's rework also reaches. The converging rework → merge edge is a
// forward edge, not a loop, and step indices follow execution order.
func TestProcessContextConvergingEdgesAreForward(t *testing.T) {
	f, root := processRuntimeFlow(t)
	task := func(next model.Next) model.Node {
		return model.Node{Type: model.NodeTypeTask, Next: next,
			Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: "true"}}
	}
	putProcessRuntimeTemplate(t, root, &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "converge", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "review"}},
			"review": {Type: model.NodeTypeDecision, Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Ok?"},
				Next: model.Next{"approve": "merge", "reject": "rework"}},
			"rework": task(model.Next{"next": "merge"}),
			"merge":  task(model.Next{"next": "end"}),
			"end":    {Type: model.NodeTypeEnd, Result: "success"},
		},
	})
	f.HaveGroup("ctx-converge")
	// The entry decision parks the run on a human, so no program executes.
	startScopedRun(t, f, "run_converge", "converge", map[string]any{"group": "ctx-converge"}, []string{"safe"})

	run := readProcessContext(t, f, "").Runs["run_converge"]
	back := map[string]bool{}
	for _, edge := range run.Edges {
		back[edge.From+">"+edge.To] = edge.Back
	}
	assert.False(t, back["rework>merge"], "a converging forward edge is not a loop")
	assert.False(t, back["review>merge"])
	index := map[string]int{}
	for _, node := range run.Nodes {
		index[node.ID] = node.Index
	}
	assert.Equal(t, map[string]int{"start": 0, "review": 1, "rework": 2, "merge": 3, "end": 0}, index)
}

// Scenario: the browser reads the context through the dashboard's own mux,
// as the human operator, and sees every scoped run.
func TestProcessContextServedOnDashboardMux(t *testing.T) {
	f := scopedRunFlow(t)
	f.HaveGroup("ctx-dash")
	startScopedRun(t, f, "run_dash", "gate", map[string]any{"group": "ctx-dash"}, []string{})

	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
		dashReq(t, http.MethodGet, "/v1/process/context", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view processContextWire
	testharness.DecodeJSON(t, rec, &view)
	require.Len(t, view.Groups, 1)
	assert.Equal(t, []string{"run_dash"}, view.Groups[0].NeedsYou)
}
