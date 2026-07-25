package agentd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// processGateTemplate is the compound runtime fixture with an authored rework
// budget on the parent. Its stages keep the distinct profiles the compound
// authorization gate already covers.
func processGateTemplate(id string, maxAttempts int) *model.Template {
	tmpl := processCompoundTemplate(id)
	build := tmpl.Nodes["build"]
	build.Retry = &model.RetryPolicy{MaxAttempts: maxAttempts}
	tmpl.Nodes["build"] = build
	return tmpl
}

// processGateEventPage reads a rework loop's WHOLE public stream by following
// the endpoint's own cursor. The shared single-page helper is sized for shorter
// runs, and a loop that reworked and then parked twice records more rows than
// one page holds.
func processGateEventPage(t *testing.T, f *testharness.Flow, runID string) processRuntimeEventPage {
	t.Helper()
	var all processRuntimeEventPage
	for after := int64(0); ; {
		path := fmt.Sprintf("/v1/process/runs/%s/events?limit=16&after=%d", runID, after)
		rec := processRuntimeRequest(t, f, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var page processRuntimeEventPage
		testharness.DecodeJSON(t, rec, &page)
		all.Events = append(all.Events, page.Events...)
		if page.Next == 0 || len(page.Events) == 0 {
			return all
		}
		after = page.Next
	}
}

func createProcessGateRun(t *testing.T, f *testharness.Flow, templateID string) processRuntimeRunView {
	t.Helper()
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId":               templateID,
		"authorizeProgramProfiles": []string{"checker", "doer", "planner", "reviewer"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	require.NotEmpty(t, run.ID)
	return run
}

// TestProcessRuntimeCompoundGateReworksTheWorkAndCompletes is the end-to-end
// rework loop through the production mux: a check rejects the work, the daemon
// re-runs the do stage and every gate on the compound's own budget, and the run
// completes. The public evidence tells that story in causal order.
func TestProcessRuntimeCompoundGateReworksTheWorkAndCompletes(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processGateTemplate("gate-rework", 2))

	log := &processAttemptLog{}
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		command := dispatch.Command()
		log.record(ctx, command)
		// The check rejects the first pass only, so the second one gets through.
		if command.NodeID == "build.test.unit" && command.Attempt == 1 {
			return processObservation(command, engine.ProgramFailed, 1), nil
		}
		return processObservation(command, engine.ProgramSucceeded, 0), nil
	}))

	run := createProcessGateRun(t, f, "gate-rework")
	agentd.WaitForProcessRunRuntimeForTest()

	dispatched, _ := log.snapshot()
	order := make([]string, 0, len(dispatched))
	for _, command := range dispatched {
		order = append(order, command.NodeID)
	}
	// The plan stage ran once; the work and both gates ran again after the
	// rejection, and the review only ever saw the accepted work.
	assert.Equal(t, []string{
		"build.plan", "build.do", "build.test.unit",
		"build.do", "build.test.unit", "build.review",
	}, order)

	completed := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, completed.Status)
	assert.Equal(t, "terminal", completed.Action)
	assert.Empty(t, completed.Blocked)
	for _, nodeID := range []string{
		"start", "build", "build.plan", "build.do", "build.test.unit", "build.review", "build.done", "end",
	} {
		assert.Equal(t, engine.NodeDone, completed.Checkpoint.Nodes[nodeID], nodeID)
	}
	// The budget lives on the work and nothing else earned a ceiling entry.
	assert.Equal(t, 2, completed.Checkpoint.Attempts["build.do"])
	assert.Equal(t, 2, completed.Checkpoint.Attempts["build.test.unit"])
	assert.Equal(t, 1, completed.Checkpoint.Attempts["build.plan"])
	assert.Empty(t, completed.Checkpoint.AttemptCeilings)

	// The public stream carries exactly one compact reset row, between the
	// verdict that caused it and the work command it made plannable.
	page := processGateEventPage(t, f, run.ID)
	reset := -1
	for index, event := range page.Events {
		if event.Kind == "stage_reset" {
			require.Equal(t, -1, reset, "the loop recorded more than one reset")
			reset = index
		}
	}
	require.GreaterOrEqual(t, reset, 1)
	assert.Equal(t, "program_observed", page.Events[reset-1].Kind)
	assert.Equal(t, "build.test.unit", page.Events[reset-1].NodeID)
	require.Less(t, reset+1, len(page.Events))
	assert.Equal(t, "program_prepared", page.Events[reset+1].Kind)
	assert.Equal(t, "build.do", page.Events[reset+1].NodeID)
	assert.Equal(t, "engine:program-executor", page.Events[reset].Actor)
	assert.Equal(t, "build", page.Events[reset].NodeID)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(page.Events[reset].Payload, &payload))
	assert.Equal(t, map[string]any{
		"parentNodeId": "build", "gateNodeId": "build.test.unit",
		"workNodeId": "build.do", "nextWorkAttempt": float64(2),
	}, payload)
}

// TestProcessRuntimeExhaustedCompoundGateParksAndIsResolvedByRetryThenSkip is
// the operator loop for a gate: the compound's budget runs out at the gate, the
// run parks instead of failing, show names the exact resolution identity, retry
// buys another pass at the WORK, and skip finally carries the run through.
func TestProcessRuntimeExhaustedCompoundGateParksAndIsResolvedByRetryThenSkip(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processGateTemplate("gate-parking", 1))

	log := &processAttemptLog{}
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		command := dispatch.Command()
		log.record(ctx, command)
		if command.NodeID == "build.test.unit" {
			return processObservation(command, engine.ProgramFailed, 1), nil
		}
		return processObservation(command, engine.ProgramSucceeded, 0), nil
	}))

	run := createProcessGateRun(t, f, "gate-parking")
	agentd.WaitForProcessRunRuntimeForTest()

	blocked := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunRunning, blocked.Status, "an exhausted gate must not doom the run")
	assert.Equal(t, "blocked", blocked.Action)
	require.Len(t, blocked.Blocked, 1)
	assert.Equal(t, "build.test.unit", blocked.Blocked[0].NodeID, "the gate parks at its own node")
	assert.Equal(t, 1, blocked.Blocked[0].Attempt)
	assert.NotEmpty(t, blocked.Blocked[0].Reason)
	assert.False(t, blocked.NeedsReconcile)
	assert.Empty(t, blocked.Commands)
	// The parent is still running: only its done stage ever completes it.
	assert.Equal(t, engine.NodeRunning, blocked.Checkpoint.Nodes["build"])
	assert.Equal(t, engine.NodeDone, blocked.Checkpoint.Nodes["build.do"])
	parkedVersion := blocked.StateVersion

	// A parked compound is parked work: the sweep leaves it alone.
	for range 3 {
		agentd.RunProcessRunSweepForTest()
		agentd.WaitForProcessRunRuntimeForTest()
	}
	assert.Equal(t, parkedVersion, showProcessRun(t, f, run.ID).StateVersion, "the sweep churned a parked compound")

	// Retry opens one fresh window on the WORK and the loop resumes there.
	resolved := resolveBlocked(t, f, run.ID, map[string]any{
		"nodeId": "build.test.unit", "attempt": 1, "action": "retry", "note": "checker host was down",
	})
	assert.True(t, resolved.Started, "the retry made the work dispatchable again")
	agentd.WaitForProcessRunRuntimeForTest()

	afterRetry, _ := log.snapshot()
	order := make([]string, 0, len(afterRetry))
	for _, command := range afterRetry {
		order = append(order, command.NodeID)
	}
	assert.Equal(t, []string{
		"build.plan", "build.do", "build.test.unit", "build.do", "build.test.unit",
	}, order, "the operator retry re-ran the work, not just the gate")

	reblocked := showProcessRun(t, f, run.ID)
	require.Len(t, reblocked.Blocked, 1)
	assert.Equal(t, 2, reblocked.Blocked[0].Attempt, "the gate re-parked at its new exact attempt")
	assert.Equal(t, map[string]int{"build.do": 2}, reblocked.Checkpoint.AttemptCeilings,
		"the raise landed on the work, and no gate earned a ceiling of its own")

	// Skip passes the gate and the compound advances to its next stage.
	skipped := resolveBlocked(t, f, run.ID, map[string]any{
		"nodeId": "build.test.unit", "attempt": 2, "action": "skip",
	})
	assert.True(t, skipped.Started, "skipping the gate readied the review stage")
	agentd.WaitForProcessRunRuntimeForTest()

	completed := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, completed.Status)
	assert.Empty(t, completed.Blocked)
	assert.Equal(t, engine.NodeDone, completed.Checkpoint.Nodes["build.test.unit"])
	assert.Equal(t, engine.NodeDone, completed.Checkpoint.Nodes["build"])
	final, _ := log.snapshot()
	assert.Len(t, final, 6, "the skip passed the gate rather than running it again")

	// Both resolutions are audited, and each rework recorded exactly one reset.
	page := processGateEventPage(t, f, run.ID)
	kinds := map[string]int{}
	for _, event := range page.Events {
		kinds[event.Kind]++
		if event.Kind == "blocked_resolved" {
			assert.Equal(t, "human:operator", event.Actor)
		}
		if event.Kind == "stage_reset" {
			assert.Equal(t, "engine:program-executor", event.Actor)
		}
	}
	assert.Equal(t, 2, kinds["node_blocked"], "kinds = %v", kinds)
	assert.Equal(t, 2, kinds["blocked_resolved"], "kinds = %v", kinds)
	assert.Equal(t, 1, kinds["stage_reset"], "only the retry resets; a skip passes the gate")
}
