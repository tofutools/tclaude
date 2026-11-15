package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tk_e46af423: the primitives a "way of working" needs. A step can be
// performed by the group's lead; it can be bound to the agent that performed
// another step (sameAs) or kept from every agent that performed others
// (differentFrom); and a step can declare outputs its performer reports,
// which later steps' prompts read as {{ name }}.

func agentStep(prompt string, edit func(*model.Performer)) *model.Performer {
	performer := &model.Performer{Kind: model.PerformerAgent, Prompt: prompt}
	if edit != nil {
		edit(performer)
	}
	return performer
}

// chainTemplate is start → steps[0] → … → end, each step an agent task.
func chainTemplate(id string, steps []string, nodes map[string]model.Node) *model.Template {
	tmpl := &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "start",
		Nodes: map[string]model.Node{"start": {Type: model.NodeTypeStart, Next: model.Next{"next": steps[0]}},
			"end": {Type: model.NodeTypeEnd, Result: "success"}}}
	for i, step := range steps {
		node := nodes[step]
		node.Type = model.NodeTypeTask
		next := "end"
		if i+1 < len(steps) {
			next = steps[i+1]
		}
		node.Next = model.Next{"next": next}
		tmpl.Nodes[step] = node
	}
	return tmpl
}

// commandFor is the run's outstanding command for node, or fails.
func commandFor(t *testing.T, view performerRunView, node string) (agentID string, attempt int, state string) {
	t.Helper()
	for _, command := range view.Commands {
		if command.NodeID == node {
			require.NotNil(t, command.Performer, "%+v", view)
			return command.Performer.AgentID, command.Attempt, command.State
		}
	}
	t.Fatalf("no command for %s: %+v", node, view.Commands)
	return "", 0, ""
}

func resolutionOf(t *testing.T, view performerRunView, node string) string {
	t.Helper()
	for _, command := range view.Commands {
		if command.NodeID == node && command.Performer != nil {
			return command.Performer.Resolution
		}
	}
	t.Fatalf("no command for %s", node)
	return ""
}

func reportStep(t *testing.T, f *testharness.Flow, conv, runID, node string, attempt int, extra map[string]any) {
	t.Helper()
	body := map[string]any{"runId": runID, "nodeId": node, "attempt": attempt, "outcome": "succeeded", "note": "done"}
	for k, v := range extra {
		body[k] = v
	}
	rec := reportAs(t, f, conv, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Scenario: "the group lead" is the member with the role lead; with none, a
// member that owns the group; with neither, the step waits and says why.
func TestProcessLeadSelector_RoleLeadThenOwnerThenWaits(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	havePerformerAgent(t, f, "crew", "ldev-1111-2222-3333-4444", "alice", "dev", "idle")
	const ownerConv = "lown-1111-2222-3333-4444"
	owner := havePerformerAgent(t, f, "crew", ownerConv, "olga", "dev", "working")
	merge := chainTemplate("lead-merge", []string{"merge"}, map[string]model.Node{
		"merge": {Performer: agentStep("Check the PR and merge it.", func(p *model.Performer) {
			p.Lead = true
			p.Fallback = &model.PerformerFallback{Action: model.FallbackWait}
		})},
	})

	waiting := startPerformerRun(t, f, root, "run_lead_none", merge, groupScope(g))
	_, _, state := commandFor(t, waiting, "merge")
	assert.Equal(t, db.ProcessDispatchNeedsPerformer, state)
	assert.Contains(t, waiting.Commands[0].Performer.WaitReason, "the group has no lead")

	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, ""))
	byOwner := startPerformerRun(t, f, root, "run_lead_owner", merge, groupScope(g))
	agentID, _, _ := commandFor(t, byOwner, "merge")
	assert.Equal(t, owner, agentID, "with no lead role, the member that owns the group leads it")
	assert.Equal(t, "lead", resolutionOf(t, byOwner, "merge"))

	lena := havePerformerAgent(t, f, "crew", "lead-1111-2222-3333-4444", "lena", "lead", "working")
	byRole := startPerformerRun(t, f, root, "run_lead_role", merge, groupScope(g))
	agentID, _, _ = commandFor(t, byRole, "merge")
	assert.Equal(t, lena, agentID, "the member with the role lead comes first, busy or not")
}

// Scenario: a reviewer must differ from the implementor, and a fix-up goes
// back to the implementor (sameAs) even though "anyone" would prefer the idle
// reviewer. With only one dev, the independent review falls back to a
// spawn-free wait that says why.
func TestProcessPerformerConstraints_SameAsAndDifferentFrom(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const aliceConv, bobConv = "cdev-1111-2222-3333-4444", "cdvb-1111-2222-3333-4444"
	alice := havePerformerAgent(t, f, "crew", aliceConv, "alice", "dev", "idle")
	tmpl := chainTemplate("constraints", []string{"implement", "review", "fixup"}, map[string]model.Node{
		"implement": {Performer: agentStep("Implement it.", func(p *model.Performer) { p.Role = "dev" })},
		"review": {Performer: agentStep("Review it independently.", func(p *model.Performer) {
			p.Role = "dev"
			p.DifferentFrom = []string{"implement"}
			p.Fallback = &model.PerformerFallback{Action: model.FallbackWait}
		})},
		"fixup": {Performer: agentStep("Apply the review.", func(p *model.Performer) {
			p.Anyone = true
			p.SameAs = "implement"
		})},
	})

	const runID = "run_constraints"
	view := startPerformerRun(t, f, root, runID, tmpl, groupScope(g))
	agentID, attempt, _ := commandFor(t, view, "implement")
	require.Equal(t, alice, agentID)
	reportStep(t, f, aliceConv, runID, "implement", attempt, nil)

	view = showPerformerRun(t, f, runID)
	_, _, state := commandFor(t, view, "review")
	assert.Equal(t, db.ProcessDispatchNeedsPerformer, state, "alice implemented it, so she cannot review it")
	assert.Contains(t, view.Commands[0].Performer.WaitReason, "without having performed implement")

	bob := havePerformerAgent(t, f, "crew", bobConv, "bob", "dev", "idle")
	setConvStatus(t, aliceConv, "working")
	agentd.SweepProcessAgentDispatchesForTest()
	view = showPerformerRun(t, f, runID)
	agentID, attempt, _ = commandFor(t, view, "review")
	require.Equal(t, bob, agentID, "a new dev who did not implement it takes the review")
	reportStep(t, f, bobConv, runID, "review", attempt, nil)

	view = showPerformerRun(t, f, runID)
	agentID, attempt, _ = commandFor(t, view, "fixup")
	assert.Equal(t, alice, agentID, "the fix-up goes back to the implementor, busy as she is, not to idle bob")
	assert.Equal(t, "same_as", resolutionOf(t, view, "fixup"))
	reportStep(t, f, aliceConv, runID, "fixup", attempt, nil)
	assert.Equal(t, engine.RunCompleted, showPerformerRun(t, f, runID).Status)
}

// Scenario: two reviewers alongside each other, the first one declared
// different from the second. They are dispatched together, the first one
// first; both would take the one idle reviewer, but differentFrom holds both
// ways, so the second goes to the busy one.
func TestProcessPerformerConstraints_ParallelReviewersAreDistinct(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	rita := havePerformerAgent(t, f, "crew", "prva-1111-2222-3333-4444", "rita", "reviewer", "idle")
	ravi := havePerformerAgent(t, f, "crew", "prvb-1111-2222-3333-4444", "ravi", "reviewer", "working")
	reviewer := func(prompt string, edit func(*model.Performer)) model.Node {
		return model.Node{Type: model.NodeTypeTask, Next: model.Next{"next": "join"},
			Performer: agentStep(prompt, func(p *model.Performer) {
				p.Role = "reviewer"
				if edit != nil {
					edit(p)
				}
			})}
	}
	tmpl := &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: "two-reviewers", Start: "fork",
		Nodes: map[string]model.Node{
			"fork":     {Type: model.NodeTypeParallel, Next: model.Next{"a": "review-a", "b": "review-b"}},
			"review-a": reviewer("Review it.", func(p *model.Performer) { p.DifferentFrom = []string{"review-b"} }),
			"review-b": reviewer("Review it too.", nil),
			"join":     {Type: model.NodeTypeEnd, Join: model.JoinAll, Result: "success"},
		}}
	view := startPerformerRun(t, f, root, "run_distinct", tmpl, groupScope(g))
	a, _, _ := commandFor(t, view, "review-a")
	b, _, _ := commandFor(t, view, "review-b")
	assert.Equal(t, rita, a, "the idle reviewer takes the first review")
	assert.Equal(t, ravi, b, "the second review may not go to the first one's reviewer, though it names no constraint")
}

// Scenario: a plan step's output reaches the build step's prompt, and the
// build step's PR reaches the check step with the plan. A success that leaves
// an output out is refused at its own step; a name the step does not declare
// is refused too. With one output, the note is it.
func TestProcessStepOutputs_PassOnToLaterPrompts(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const devConv = "odev-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", devConv, "alice", "dev", "idle")
	dev := func(prompt string) *model.Performer {
		return agentStep(prompt, func(p *model.Performer) { p.Role = "dev" })
	}
	tmpl := chainTemplate("outputs", []string{"plan", "build", "check"}, map[string]model.Node{
		"plan":  {Performer: dev("Plan the change."), Captures: []string{"plan"}},
		"build": {Performer: dev("Build this plan:\n{{ plan }}"), Captures: []string{"pr", "branch"}},
		"check": {Performer: dev("Check {{pr}} on {{ branch }} against the plan:\n{{plan}}")},
	})

	const runID = "run_outputs"
	view := startPerformerRun(t, f, root, runID, tmpl, groupScope(g))
	_, attempt, _ := commandFor(t, view, "plan")
	inbox := processInbox(t, devConv)
	require.Len(t, inbox, 1)
	assert.Contains(t, inbox[0].Body, "--output plan=")
	assert.Contains(t, inbox[0].Body, "This step's output, plan, goes to later steps")

	missing := reportAs(t, f, devConv, map[string]any{"runId": runID, "nodeId": "plan", "attempt": attempt, "outcome": "succeeded"})
	assert.Equal(t, http.StatusUnprocessableEntity, missing.Code)
	assert.Contains(t, missing.Body.String(), `this step must report its output \"plan\"`)
	unknown := reportAs(t, f, devConv, map[string]any{"runId": runID, "nodeId": "plan", "attempt": attempt,
		"outcome": "succeeded", "note": "x", "outputs": map[string]any{"pr": "#1"}})
	assert.Equal(t, http.StatusUnprocessableEntity, unknown.Code)
	assert.Contains(t, unknown.Body.String(), `\"pr\" is not one`)
	reportStep(t, f, devConv, runID, "plan", attempt, map[string]any{"note": "1. add a filter field\n2. save it per user"})

	view = showPerformerRun(t, f, runID)
	_, attempt, _ = commandFor(t, view, "build")
	inbox = processInbox(t, devConv)
	require.Len(t, inbox, 2)
	assert.Contains(t, inbox[1].Body, "Build this plan:\n1. add a filter field\n2. save it per user")
	assert.Contains(t, inbox[1].Body, `--output pr="<value>" --output branch="<value>"`)
	onlyOne := reportAs(t, f, devConv, map[string]any{"runId": runID, "nodeId": "build", "attempt": attempt,
		"outcome": "succeeded", "note": "PR is up", "outputs": map[string]any{"pr": "PR #51"}})
	assert.Equal(t, http.StatusUnprocessableEntity, onlyOne.Code, "with two outputs the note stands in for neither")
	assert.Contains(t, onlyOne.Body.String(), `\"branch\"`)
	reportStep(t, f, devConv, runID, "build", attempt, map[string]any{"outputs": map[string]any{"pr": "PR #51", "branch": "saved-filters"}})

	view = showPerformerRun(t, f, runID)
	_, attempt, _ = commandFor(t, view, "check")
	inbox = processInbox(t, devConv)
	require.Len(t, inbox, 3)
	assert.Contains(t, inbox[2].Body, "Check PR #51 on saved-filters against the plan:\n1. add a filter field")
	reportStep(t, f, devConv, runID, "check", attempt, nil)

	view = showPerformerRun(t, f, runID)
	assert.Equal(t, engine.RunCompleted, view.Status)
	outputs := map[string]map[string]string{}
	for _, performer := range view.Performers {
		if len(performer.Outputs) > 0 {
			outputs[performer.NodeID] = performer.Outputs
		}
	}
	assert.Equal(t, map[string]map[string]string{
		"plan":  {"plan": "1. add a filter field\n2. save it per user"},
		"build": {"pr": "PR #51", "branch": "saved-filters"},
	}, outputs, "the run view shows what each step reported")
}

// Scenario: the new fields validate: lead is a selector like the others,
// sameAs and differentFrom name other agent steps and do not contradict,
// an assignee leaves no room for a constraint, outputs are declared once and
// only plain agent tasks can run one, and an output nobody declares is a
// warning.
func TestProcessPrimitives_Validation(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	type finding struct{ Code, Severity string }
	validate := func(tmpl *model.Template) []finding {
		t.Helper()
		rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/validate", map[string]any{"template": tmpl})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			Diagnostics []finding `json:"diagnostics"`
		}
		testharness.DecodeJSON(t, rec, &body)
		return body.Diagnostics
	}
	codes := func(tmpl *model.Template) []string {
		var out []string
		for _, d := range validate(tmpl) {
			out = append(out, d.Code)
		}
		return out
	}
	two := func(first, second *model.Performer, captures ...[]string) *model.Template {
		nodes := map[string]model.Node{"one": {Performer: first}, "two": {Performer: second}}
		if len(captures) > 0 {
			one := nodes["one"]
			one.Captures = captures[0]
			nodes["one"] = one
		}
		if len(captures) > 1 {
			two := nodes["two"]
			two.Captures = captures[1]
			nodes["two"] = two
		}
		return chainTemplate("primitives", []string{"one", "two"}, nodes)
	}
	plain := func() *model.Performer { return agentStep("Do it.", func(p *model.Performer) { p.Role = "dev" }) }
	with := func(edit func(*model.Performer)) *model.Performer {
		return agentStep("Do it.", edit)
	}

	assert.Empty(t, codes(two(plain(), with(func(p *model.Performer) {
		p.Lead, p.SameAs = true, "one"
	}))))
	assert.Contains(t, codes(two(plain(), with(func(p *model.Performer) { p.Lead, p.Role = true, "dev" }))), "conflicting_performer_selector")
	assert.Contains(t, codes(two(plain(), with(func(p *model.Performer) { p.SameAs = "nope" }))), "invalid_performer_constraint")
	assert.Contains(t, codes(two(plain(), with(func(p *model.Performer) { p.DifferentFrom = []string{"two"} }))), "invalid_performer_constraint")
	assert.Contains(t, codes(two(plain(), with(func(p *model.Performer) {
		p.SameAs, p.DifferentFrom = "one", []string{"one"}
	}))), "conflicting_performer_constraint")
	assert.Contains(t, codes(two(plain(), with(func(p *model.Performer) {
		p.Assignee, p.DifferentFrom = "alice", []string{"one"}
	}))), "conflicting_performer_constraint")
	assert.Contains(t, codes(two(plain(), &model.Performer{Kind: model.PerformerProgram, Run: "true", SameAs: "one"})),
		"kind_scoped_field", "a program step has no performer constraints")
	assert.Contains(t, codes(two(plain(), plain(), []string{"pr"}, []string{"pr"})), "duplicate_capture")
	assert.Contains(t, validate(two(plain(), with(func(p *model.Performer) { p.Prompt = "Check {{ pr }}." }))),
		finding{Code: "unknown_capture_ref", Severity: "warning"})
	assert.NotContains(t, codes(two(plain(), with(func(p *model.Performer) { p.Prompt = "Check {{ pr }}." }), []string{"pr"})),
		"unknown_capture_ref")
}
