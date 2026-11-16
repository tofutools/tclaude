//ci:reads none

package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Built-in ways of working (tk_d02b0f4c): listed from the binary, copied into
// a group on use, and each one run end to end by simulated agents.

// useBuiltin copies built-in id into group as the human and returns the
// copy's template id.
func useBuiltin(t *testing.T, f *testharness.Flow, id, group string) string {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/builtins/"+id+"/use", map[string]any{"group": group})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)
	return created.ID
}

// builtinRun copies built-in id into the group, starts it for task, and
// returns a step function: it checks who performs node and what they were
// told, then reports for them.
func builtinRun(t *testing.T, f *testharness.Flow, g *db.AgentGroup, id, runID, task string, convOf map[string]string) func(node, want string, told []string, extra map[string]any) {
	t.Helper()
	templateID := useBuiltin(t, f, id, g.Name)
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": runID, "templateId": templateID, "group": g.Name, "params": map[string]string{"task": task},
		"authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	return func(node, want string, told []string, extra map[string]any) {
		t.Helper()
		agentd.WaitForProcessDispatchForTest()
		view := showPerformerRun(t, f, runID)
		agentID, attempt, _ := commandFor(t, view, node)
		require.Equal(t, want, agentID, "who performs %s", node)
		inbox := processInbox(t, convOf[agentID])
		require.NotEmpty(t, inbox)
		for _, text := range told {
			assert.Contains(t, inbox[len(inbox)-1].Body, text, "%s's prompt", node)
		}
		reportStep(t, f, convOf[agentID], runID, node, attempt, extra)
	}
}

// Scenario: the built-ins are listed in order with their steps; using one
// makes the group its own copy, saved to the group (not the library), and
// a second use makes another, so a group's edits never touch the built-in.
func TestProcessBuiltins_ListedAndCopiedIntoAGroup(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	f.HaveGroup("crew")

	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/builtins", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listed struct {
		Builtins []struct {
			ID    string   `json:"id"`
			Name  string   `json:"name"`
			Steps []string `json:"steps"`
		} `json:"builtins"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	ids := []string{}
	for _, b := range listed.Builtins {
		ids = append(ids, b.ID)
	}
	assert.Equal(t, []string{"plan-vet-build", "build-review-ship", "bug-fix", "quick-change", "plan-fan-out", "ask-me-first", "check-fix"}, ids)
	assert.Equal(t, "Plan → vet → build → review → merge", listed.Builtins[0].Name)
	assert.Equal(t, []string{"brief", "plan", "vet", "build", "review", "merge"}, listed.Builtins[0].Steps)
	assert.Equal(t, []string{"reproduce", "fix", "verify", "merge"}, listed.Builtins[2].Steps)

	one := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/builtins/bug-fix", nil)
	require.Equal(t, http.StatusOK, one.Code, one.Body.String())
	assert.Contains(t, one.Body.String(), `"reproduce"`)

	first := useBuiltin(t, f, "quick-change", "crew")
	second := useBuiltin(t, f, "quick-change", "crew")
	assert.NotEqual(t, first, second, "each use is its own copy")
	assert.NotEqual(t, "quick-change", first, "the copy has its own id")

	ctx := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/context", nil)
	require.Equal(t, http.StatusOK, ctx.Code, ctx.Body.String())
	var context struct {
		Groups []struct {
			Group     string `json:"group"`
			Processes []struct {
				TemplateID string `json:"templateId"`
				Name       string `json:"name"`
			} `json:"processes"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(ctx.Body.Bytes(), &context))
	var saved []string
	for _, g := range context.Groups {
		if g.Group == "crew" {
			for _, p := range g.Processes {
				saved = append(saved, p.TemplateID)
				assert.Equal(t, "Quick change + one review", p.Name)
			}
		}
	}
	assert.ElementsMatch(t, []string{first, second}, saved, "the copies are the group's saved processes")

	library := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/templates", nil)
	require.Equal(t, http.StatusOK, library.Code)
	assert.NotContains(t, library.Body.String(), first, "a group's copy is not published to the library")

	named := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/builtins/bug-fix/use", map[string]any{"group": "crew", "name": "Crew bugs"})
	require.Equal(t, http.StatusCreated, named.Code, named.Body.String())
	var namedID struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(named.Body.Bytes(), &namedID))
	copied := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/templates/"+namedID.ID, nil)
	require.Equal(t, http.StatusOK, copied.Code, copied.Body.String())
	assert.Contains(t, copied.Body.String(), `"name":"Crew bugs"`, "the copy takes the name it was given")

	missing := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/builtins/nope/use", map[string]any{"group": "crew"})
	assert.Equal(t, http.StatusNotFound, missing.Code)
	noGroup := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/builtins/bug-fix/use", map[string]any{})
	assert.Equal(t, http.StatusBadRequest, noGroup.Code)
	unknownGroup := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/builtins/bug-fix/use", map[string]any{"group": "ghosts"})
	assert.Equal(t, http.StatusNotFound, unknownGroup.Code)
}

// Scenario: Plan → vet → build → review → merge, straight through: the lead
// frames, a planner plans, the vet passes it, a developer builds, two other
// reviewers both approve, and the lead who framed it merges.
func TestProcessBuiltins_PlanVetBuildEndToEnd(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const leadConv, planConv, uxConv = "blead-111-2222-3333-4444", "bplan-111-2222-3333-4444", "bux-1111-2222-3333-4444"
	const devConv, ritaConv, raviConv = "bdev-1111-2222-3333-4444", "brita-111-2222-3333-4444", "bravi-111-2222-3333-4444"
	lena := havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "idle")
	pia := havePerformerAgent(t, f, "crew", planConv, "pia", "planner", "idle")
	uma := havePerformerAgent(t, f, "crew", uxConv, "uma", "ux", "idle")
	dev := havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	rita := havePerformerAgent(t, f, "crew", ritaConv, "rita", "reviewer", "idle")
	ravi := havePerformerAgent(t, f, "crew", raviConv, "ravi", "reviewer", "idle")
	convOf := map[string]string{lena: leadConv, pia: planConv, uma: uxConv, dev: devConv, rita: ritaConv, ravi: raviConv}
	const runID = "run_builtin_plan"
	step := builtinRun(t, f, g, "plan-vet-build", runID, "dark mode", convOf)

	step("brief", lena, []string{"dark mode"}, map[string]any{"note": "Goal: a dark theme."})
	step("plan", pia, []string{"Goal: a dark theme."}, map[string]any{"note": "1. tokens\n2. toggle"})
	step("vet", uma, []string{"1. tokens"}, map[string]any{"verdict": "ok"})
	step("build", dev, []string{"1. tokens"}, map[string]any{"outputs": map[string]any{"pr": "PR #9"}})
	view := showPerformerRun(t, f, runID)
	first, _, _ := commandFor(t, view, "review.vote.1")
	second, _, _ := commandFor(t, view, "review.vote.2")
	assert.ElementsMatch(t, []string{rita, ravi}, []string{first, second})
	step("review.vote.1", first, []string{"PR #9"}, map[string]any{"verdict": "approve"})
	step("review.vote.2", second, []string{"PR #9"}, map[string]any{"verdict": "approve"})
	assert.Equal(t, "same_as", resolutionOf(t, showPerformerRun(t, f, runID), "merge"))
	step("merge", lena, []string{"PR #9"}, nil)
	assert.Equal(t, engine.RunCompleted, showPerformerRun(t, f, runID).Status)
}

// Scenario: Build → review → ship: the reviewer sends it back once, the
// developer does round 2, the reviewer approves, and the lead ships it.
func TestProcessBuiltins_BuildReviewShipEndToEnd(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const leadConv, devConv, revConv = "slead-111-2222-3333-4444", "sdev-1111-2222-3333-4444", "srev-1111-2222-3333-4444"
	lena := havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "idle")
	dev := havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	rita := havePerformerAgent(t, f, "crew", revConv, "rita", "reviewer", "idle")
	convOf := map[string]string{lena: leadConv, dev: devConv, rita: revConv}
	const runID = "run_builtin_ship"
	step := builtinRun(t, f, g, "build-review-ship", runID, "CSV export", convOf)

	step("build", dev, []string{"Build this: CSV export"}, map[string]any{"outputs": map[string]any{"pr": "PR #5"}})
	step("review", rita, []string{"PR #5", "CSV export"}, map[string]any{"verdict": "changes", "note": "quote commas"})
	step("build", dev, []string{"quote commas"}, map[string]any{"outputs": map[string]any{"pr": "PR #5 (quoted)"}})
	step("review", rita, []string{"PR #5 (quoted)"}, map[string]any{"verdict": "approve"})
	step("ship", lena, []string{"The review approved this PR", "PR #5 (quoted)"}, nil)
	assert.Equal(t, engine.RunCompleted, showPerformerRun(t, f, runID).Status)
}

// Scenario: Bug: reproduce → fix → verify. A tester reproduces it, a
// developer fixes it, the same tester finds it still broken, the developer
// fixes it again, the tester verifies it, and the lead merges.
func TestProcessBuiltins_BugFixEndToEnd(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const leadConv, devConv, tessConv, timConv = "glead-111-2222-3333-4444", "gdev-1111-2222-3333-4444", "gtess-111-2222-3333-4444", "gtim-1111-2222-3333-4444"
	lena := havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "idle")
	dev := havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	tess := havePerformerAgent(t, f, "crew", tessConv, "tess", "tester", "idle")
	tim := havePerformerAgent(t, f, "crew", timConv, "tim", "tester", "idle")
	convOf := map[string]string{lena: leadConv, dev: devConv, tess: tessConv, tim: timConv}
	const runID = "run_builtin_bug"
	step := builtinRun(t, f, g, "bug-fix", runID, "login loops on Safari", convOf)

	view := showPerformerRun(t, f, runID)
	tester, _, _ := commandFor(t, view, "reproduce")
	require.Contains(t, []string{tess, tim}, tester)
	step("reproduce", tester, []string{"Reproduce this bug: login loops on Safari"},
		map[string]any{"outputs": map[string]any{"repro": "Safari 17: sign in, reload"}})
	step("fix", dev, []string{"Safari 17: sign in, reload"}, map[string]any{"outputs": map[string]any{"pr": "PR #31"}})
	assert.Equal(t, "same_as", resolutionOf(t, showPerformerRun(t, f, runID), "verify"), "the tester who reproduced it verifies")
	step("verify", tester, []string{"PR #31", "Safari 17: sign in, reload"}, map[string]any{"verdict": "broken", "note": "still loops after reload"})
	step("fix", dev, []string{"still loops after reload"}, map[string]any{"outputs": map[string]any{"pr": "PR #31 v2"}})
	step("verify", tester, []string{"PR #31 v2"}, map[string]any{"verdict": "fixed"})
	step("merge", lena, []string{"The tester verified this fix", "PR #31 v2"}, nil)
	assert.Equal(t, engine.RunCompleted, showPerformerRun(t, f, runID).Status)
}

// Scenario: Quick change + one review: the developer makes it, a reviewer
// who did not make it approves, and the developer who made it merges.
func TestProcessBuiltins_QuickChangeEndToEnd(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const devConv, revConv = "qdev-1111-2222-3333-4444", "qrev-1111-2222-3333-4444"
	dev := havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	rita := havePerformerAgent(t, f, "crew", revConv, "rita", "reviewer", "idle")
	convOf := map[string]string{dev: devConv, rita: revConv}
	const runID = "run_builtin_quick"
	step := builtinRun(t, f, g, "quick-change", runID, "fix the typo in the footer", convOf)

	step("change", dev, []string{"Make this change: fix the typo in the footer"}, map[string]any{"outputs": map[string]any{"pr": "PR #2"}})
	step("review", rita, []string{"PR #2"}, map[string]any{"verdict": "approve"})
	assert.Equal(t, "same_as", resolutionOf(t, showPerformerRun(t, f, runID), "merge"))
	step("merge", dev, []string{"The review approved your PR", "PR #2"}, nil)
	assert.Equal(t, engine.RunCompleted, showPerformerRun(t, f, runID).Status)
}

// builtinOverseer is the run's overseer agent and why it is the overseer.
func builtinOverseer(t *testing.T, f *testharness.Flow, runID string) (agentID, via string) {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view struct {
		Overseer *struct {
			AgentID string `json:"agentId"`
			Via     string `json:"via"`
		} `json:"overseer"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	require.NotNil(t, view.Overseer)
	return view.Overseer.AgentID, view.Overseer.Via
}

// Scenario: Plan → fan out → integrate. The lead splits the task, three
// different developers build a part each at the same time, and once all
// three are in the lead who split it integrates them. The lead oversees it.
func TestProcessBuiltins_PlanFanOutEndToEnd(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const leadConv, d1Conv, d2Conv, d3Conv = "flead-111-2222-3333-4444", "fd1-1111-2222-3333-4444", "fd2-1111-2222-3333-4444", "fd3-1111-2222-3333-4444"
	lena := havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "idle")
	d1 := havePerformerAgent(t, f, "crew", d1Conv, "dana", "dev", "idle")
	d2 := havePerformerAgent(t, f, "crew", d2Conv, "dave", "dev", "idle")
	d3 := havePerformerAgent(t, f, "crew", d3Conv, "dina", "dev", "idle")
	convOf := map[string]string{lena: leadConv, d1: d1Conv, d2: d2Conv, d3: d3Conv}
	const runID = "run_builtin_fanout"
	step := builtinRun(t, f, g, "plan-fan-out", runID, "a settings page", convOf)
	overseer, _ := builtinOverseer(t, f, runID)
	assert.Equal(t, lena, overseer, "the lead oversees it")

	step("plan", lena, []string{"three independent parts", "a settings page"}, map[string]any{"note": "1. layout\n2. storage\n3. API"})
	agentd.WaitForProcessDispatchForTest()
	view := showPerformerRun(t, f, runID)
	parts := []string{}
	for i, node := range []string{"part-1", "part-2", "part-3"} {
		who, _, _ := commandFor(t, view, node)
		parts = append(parts, who)
		step(node, who, []string{"1. layout"}, map[string]any{"outputs": map[string]any{"pr" + string(rune('1'+i)): "PR #" + string(rune('1'+i))}})
	}
	assert.ElementsMatch(t, []string{d1, d2, d3}, parts, "a different developer for each part")
	assert.Equal(t, "same_as", resolutionOf(t, showPerformerRun(t, f, runID), "integrate"))
	step("integrate", lena, []string{"All three parts are in", "PR #1", "PR #2", "PR #3"}, nil)
	assert.Equal(t, engine.RunCompleted, showPerformerRun(t, f, runID).Status)
}

// Scenario: Ask me first. The operator says go, a developer does it, the
// operator sends it back once, then accepts it.
func TestProcessBuiltins_AskMeFirstEndToEnd(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const devConv = "adev-1111-2222-3333-4444"
	dev := havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	convOf := map[string]string{dev: devConv}
	const runID = "run_builtin_ask"
	step := builtinRun(t, f, g, "ask-me-first", runID, "rename the API", convOf)

	require.Equal(t, http.StatusAccepted, decideLoop(t, f, runID, map[string]any{"nodeId": "go", "verdict": "go"}))
	step("work", dev, []string{"Do this: rename the API"}, map[string]any{"outputs": map[string]any{"result": "renamed in PR #4"}})
	require.Equal(t, http.StatusAccepted, decideLoop(t, f, runID, map[string]any{"nodeId": "accept", "verdict": "rework", "evidence": "keep the old name as an alias"}))
	step("work", dev, []string{"keep the old name as an alias"}, map[string]any{"outputs": map[string]any{"result": "alias added"}})
	require.Equal(t, http.StatusAccepted, decideLoop(t, f, runID, map[string]any{"nodeId": "accept", "verdict": "accept"}))
	assert.Equal(t, engine.RunCompleted, showPerformerRun(t, f, runID).Status)
}

// Scenario: Check → fix if red. The tester finds it red, a developer fixes
// it, the same tester finds it still red, the developer fixes it again, and
// the tester finds it green.
func TestProcessBuiltins_CheckFixEndToEnd(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const devConv, tessConv = "cdev-1111-2222-3333-4444", "ctess-111-2222-3333-4444"
	dev := havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	tess := havePerformerAgent(t, f, "crew", tessConv, "tess", "tester", "idle")
	convOf := map[string]string{dev: devConv, tess: tessConv}
	const runID = "run_builtin_check"
	step := builtinRun(t, f, g, "check-fix", runID, "the nightly suite", convOf)

	step("check", tess, []string{"Run the nightly suite"}, map[string]any{"verdict": "red", "note": "TestLogin fails"})
	step("fix", dev, []string{"the nightly suite is red. Run it to see what fails"}, map[string]any{"outputs": map[string]any{"pr": "PR #8"}})
	assert.Equal(t, "same_as", resolutionOf(t, showPerformerRun(t, f, runID), "recheck"))
	step("recheck", tess, []string{"PR #8"}, map[string]any{"verdict": "red", "note": "still flaky"})
	step("fix", dev, []string{"still flaky"}, map[string]any{"outputs": map[string]any{"pr": "PR #8 v2"}})
	step("recheck", tess, []string{"PR #8 v2"}, map[string]any{"verdict": "green"})
	assert.Equal(t, engine.RunCompleted, showPerformerRun(t, f, runID).Status)
}

// Scenario: a review that keeps asking for changes runs out of rounds, and
// the run fails (so a board task it runs for is blocked with the reason).
func TestProcessBuiltins_OutOfRoundsFails(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const devConv, revConv = "odev-1111-2222-3333-4444", "orev-1111-2222-3333-4444"
	dev := havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	rita := havePerformerAgent(t, f, "crew", revConv, "rita", "reviewer", "idle")
	convOf := map[string]string{dev: devConv, rita: revConv}
	const runID = "run_builtin_rounds"
	step := builtinRun(t, f, g, "quick-change", runID, "tweak the logo", convOf)
	for round := 1; round <= 5 && showPerformerRun(t, f, runID).Status == engine.RunRunning; round++ {
		step("change", dev, nil, map[string]any{"outputs": map[string]any{"pr": "PR #3"}})
		step("review", rita, nil, map[string]any{"verdict": "changes", "note": "not yet"})
		agentd.WaitForProcessDispatchForTest()
	}
	assert.Equal(t, engine.RunFailed, showPerformerRun(t, f, runID).Status)
}

// Scenario: in Ask me first, the operator keeps sending the work back; out of
// rework rounds the run fails rather than ending as their own reject.
func TestProcessBuiltins_AskMeFirstOutOfRoundsFails(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	const devConv = "afdev-111-2222-3333-4444"
	dev := havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	convOf := map[string]string{dev: devConv}
	const runID = "run_builtin_ask_rounds"
	step := builtinRun(t, f, g, "ask-me-first", runID, "rename the API", convOf)
	require.Equal(t, http.StatusAccepted, decideLoop(t, f, runID, map[string]any{"nodeId": "go", "verdict": "go"}))
	for round := 1; round <= 6 && showPerformerRun(t, f, runID).Status == engine.RunRunning; round++ {
		step("work", dev, nil, map[string]any{"outputs": map[string]any{"result": "try"}})
		require.Equal(t, http.StatusAccepted, decideLoop(t, f, runID, map[string]any{"nodeId": "accept", "verdict": "rework"}))
	}
	assert.Equal(t, engine.RunFailed, showPerformerRun(t, f, runID).Status)
}
