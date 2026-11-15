package agentd_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
)

// engineConcerns are a run view's concerns the engine raised.
func engineConcerns(v oversightRunView) []oversightConcernWire {
	var out []oversightConcernWire
	for _, c := range v.Concerns {
		if c.Source == db.ProcessConcernSourceEngine {
			out = append(out, c)
		}
	}
	return out
}

// Scenario: a template names its overseer by role and gives its steps 10
// minutes of patience. Its one step blocks (retries spent). Before the
// patience runs out nothing is raised; after, the engine raises one concern
// to the template's overseer, and only one however often the sweep runs.
// The operator skips the step; the concern settles itself as cleared.
func TestProcessStuck_BlockedStepRaisesOnceToTheTemplatesOverseerAndClears(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	advance := moveOversightClock(t)
	const leadConv, qaConv = "stl1-1111-2222-3333-4444", "stq1-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "idle")
	qa := havePerformerAgent(t, f, "crew", qaConv, "quinn", "qa", "idle")
	tmpl := processBlockingTemplate("st-parking", 1)
	tmpl.Overseer = "role:qa"
	tmpl.Patience = "10m"
	putProcessRuntimeTemplate(t, root, tmpl)
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(_ context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		return processObservation(dispatch.Command(), engine.ProgramFailed, 1), nil
	}))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_st1", "templateId": "st-parking", "group": "crew", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	run := showOversightRun(t, f, "run_st1")
	require.Len(t, run.Blocked, 1)
	require.NotNil(t, run.Overseer)
	assert.Equal(t, qa, run.Overseer.AgentID)
	assert.Equal(t, "template", run.Overseer.Via, "the template's role:qa wins over the group's lead")

	advance(5 * time.Minute)
	agentd.SweepProcessStuckSignalsForTest()
	assert.Empty(t, engineConcerns(showOversightRun(t, f, "run_st1")), "within its patience a blocked step raises nothing")

	advance(6 * time.Minute)
	agentd.SweepProcessStuckSignalsForTest()
	agentd.SweepProcessStuckSignalsForTest()
	raised := engineConcerns(showOversightRun(t, f, "run_st1"))
	require.Len(t, raised, 1, "one concern per stuck spell")
	c := raised[0]
	assert.Equal(t, "open", c.State)
	assert.Equal(t, run.Blocked[0].NodeID, c.NodeID)
	assert.Equal(t, db.ProcessConcernLevelOverseer, c.Level)
	assert.Equal(t, "agent:"+qa, c.Holder)
	assert.Contains(t, c.Reason, "has been blocked for")
	require.Len(t, oversightInbox(t, qaConv), 1, "the template's overseer is told")

	// The overseer answers "carry on": the step is still blocked, but the
	// same spell is not raised again.
	answerConcernAs(t, f, qaConv, "run_st1", c.ID, "continue", "known flake", http.StatusOK)
	advance(time.Hour)
	agentd.SweepProcessStuckSignalsForTest()
	again := engineConcerns(showOversightRun(t, f, "run_st1"))
	require.Len(t, again, 1, "an answered spell is not raised again")
	assert.Equal(t, "resolved", again[0].State)
	assert.Equal(t, "continue", again[0].Resolution)

	resolveBlocked(t, f, "run_st1", map[string]any{
		"nodeId": run.Blocked[0].NodeID, "attempt": run.Blocked[0].Attempt, "action": "skip", "note": "flaky",
	})
	agentd.SweepProcessStuckSignalsForTest()
	assert.Len(t, engineConcerns(showOversightRun(t, f, "run_st1")), 1, "a step that moved on raises nothing")
}

// Scenario: an agent step with its own patience is handed out and not
// reported. Past its patience the engine raises no_report to the group's
// lead. The performer then reports: the spell is over, and the concern
// settles itself as cleared while the run goes on.
func TestProcessStuck_UnreportedStepRaisesAndClearsWhenReported(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	advance := moveOversightClock(t)
	const leadConv, devConv = "stl2-1111-2222-3333-4444", "std2-1111-2222-3333-4444"
	lead := havePerformerAgent(t, f, "crew", leadConv, "lena", "lead", "working")
	havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	tmpl := epicTemplate()
	work := tmpl.Nodes["work"]
	work.Patience = "20m"
	tmpl.Nodes["work"] = work
	putProcessRuntimeTemplate(t, root, tmpl)
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_st2", "templateId": "epic", "group": "crew", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	showOversightRun(t, f, "run_st2")

	advance(25 * time.Minute)
	agentd.SweepProcessStuckSignalsForTest()
	raised := engineConcerns(showOversightRun(t, f, "run_st2"))
	require.Len(t, raised, 1)
	assert.Equal(t, "work", raised[0].NodeID)
	assert.Equal(t, "agent:"+lead, raised[0].Holder)
	assert.Contains(t, raised[0].Reason, "has not been reported")

	reportStep(t, f, devConv, "run_st2", "work", 1, nil)
	agentd.SweepProcessStuckSignalsForTest()
	view := showOversightRun(t, f, "run_st2")
	settled := view.concern(t, raised[0].ID)
	assert.Equal(t, "resolved", settled.State)
	assert.Equal(t, "cleared", settled.Resolution, "the step was reported, so the concern settles itself")
	assert.NotEqual(t, engine.RunCompleted, view.Status, "the run goes on")
}
