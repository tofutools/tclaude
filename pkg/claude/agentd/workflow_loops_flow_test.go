package agentd_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// JOH-39 — loops / retries / fix-loops. These flow tests drive the real engine
// tick over tool-node templates (deterministic, no agent spawn needed for the
// core mechanics) and assert at the DB surfaces: node Status/Visits, the
// workflow_events audit trail, and the instance status.

// countEvents returns how many of a node's events have the given kind.
func countEvents(t *testing.T, id int64, nodeID, kind string) int {
	t.Helper()
	events, err := db.ListWorkflowEvents(id, nodeID)
	require.NoError(t, err)
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func nodeStatusVisits(t *testing.T, id int64, nodeID string) (string, int64) {
	t.Helper()
	n, err := db.GetWorkflowNode(id, nodeID)
	require.NoError(t, err)
	require.NotNil(t, n, "node %s exists", nodeID)
	return n.Status, n.Visits
}

// Scenario: in-place retry exhaustion. A tool node whose verify always fails and
// declares retries:2 re-runs in place — one attempt per tick (tick-paced) — then
// settles failed after 3 attempts (1 + 2 retries). Visits counts every attempt.
func TestWorkflowEngine_InPlaceRetryExhausts(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	writeToolTemplate(t, root, "retry",
		"name: retry\nentry: a\n",
		"flowchart TD\n a --> done\n",
		map[string]string{
			// executor succeeds; verify always fails → the node fails its own verify.
			"a":    "executor:\n  kind: tool\n  run: echo working\nverify:\n  kind: tool\n  run: exit 1\nretries: 2\n",
			"done": "executor:\n  kind: tool\n  run: echo done\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:retry", "", nil)

	// Each tick runs exactly one attempt (tick-pacing). Attempts 1 + 2 re-arm.
	agentd.RunWorkflowEngineTickForTest()
	st, v := nodeStatusVisits(t, id, "a")
	assert.Equal(t, "ready", st, "after attempt 1 the node is re-armed for retry")
	assert.Equal(t, int64(1), v, "one execution so far")

	agentd.RunWorkflowEngineTickForTest() // attempt 2
	agentd.RunWorkflowEngineTickForTest() // attempt 3 → budget spent → failed

	st, v = nodeStatusVisits(t, id, "a")
	assert.Equal(t, "failed", st, "node fails after retries are exhausted")
	assert.Equal(t, int64(3), v, "1 initial + 2 retries = 3 executions")
	assert.Equal(t, 2, countEvents(t, id, "a", db.WorkflowEventNodeRetry), "two in-place retries recorded")
	d := wfGet(t, mux, id)
	assert.Equal(t, "failed", d.Instance.Status, "the instance halts on the exhausted node (no |fail| edge)")
}

// Scenario: in-place retry succeeds. A node whose verify fails once then passes
// (a marker file flips it) settles done on the 2nd attempt — the fix-loop's
// inner loop. The downstream node then runs and the instance completes.
func TestWorkflowEngine_InPlaceRetrySucceeds(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "flip")
	// verify: fail (and create the marker) the first time; pass once it exists.
	verify := fmt.Sprintf("[ -f %s ] && exit 0 || { touch %s; exit 1; }", marker, marker)
	writeToolTemplate(t, root, "retryok",
		"name: retryok\nentry: a\n",
		"flowchart TD\n a --> done\n",
		map[string]string{
			"a":    "executor:\n  kind: tool\n  run: echo working\nverify:\n  kind: tool\n  run: \"" + verify + "\"\nretries: 3\n",
			"done": "executor:\n  kind: tool\n  run: echo done\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:retryok", "", nil)

	agentd.RunWorkflowEngineTickForTest() // attempt 1 fails → retry
	agentd.RunWorkflowEngineTickForTest() // attempt 2 passes → done → readies `done`
	agentd.RunWorkflowEngineTickForTest() // `done` runs → instance completes

	st, v := nodeStatusVisits(t, id, "a")
	assert.Equal(t, "done", st, "node settles done once verify passes")
	assert.Equal(t, int64(2), v, "passed on the 2nd attempt")
	assert.Equal(t, 1, countEvents(t, id, "a", db.WorkflowEventNodeRetry), "one retry before success")
	d := wfGet(t, mux, id)
	assert.Equal(t, "completed", d.Instance.Status)
}

// Scenario: an edge-driven fix-loop converges. test -->|fail| impl loops back;
// test passes on the 2nd visit (marker), takes |pass| to done. The loop body
// (impl, test) re-runs once; Visits reflects two iterations; the instance
// completes.
func TestWorkflowEngine_FixLoopConverges(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "passed")
	verify := fmt.Sprintf("[ -f %s ] && exit 0 || { touch %s; exit 1; }", marker, marker)
	writeToolTemplate(t, root, "fixloop",
		"name: fixloop\nentry: impl\n",
		"flowchart TD\n impl --> test\n test -->|fail| impl\n test -->|pass| done\n",
		map[string]string{
			"impl": "executor:\n  kind: tool\n  run: echo implementing\n",
			"test": "executor:\n  kind: tool\n  run: echo testing\nverify:\n  kind: tool\n  run: \"" +
				verify + "\"\non_fail: continue\n",
			"done": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:fixloop", "", nil)

	// Drive several ticks; the chain + one loop-back + the break path drain.
	for range 6 {
		agentd.RunWorkflowEngineTickForTest()
	}

	d := wfGet(t, mux, id)
	assert.Equal(t, "completed", d.Instance.Status, "the fix-loop converges and the instance completes")
	_, implV := nodeStatusVisits(t, id, "impl")
	stTest, testV := nodeStatusVisits(t, id, "test")
	assert.Equal(t, int64(2), implV, "impl re-ran once (2 iterations)")
	assert.Equal(t, int64(2), testV, "test re-ran once (2 iterations)")
	assert.Equal(t, "done", stTest, "test settled done (pass) on the 2nd iteration")
	assert.GreaterOrEqual(t, countEvents(t, id, "impl", db.WorkflowEventNodeReentry), 1,
		"impl was re-armed by a loop-back")
}

// Scenario: MaxVisits breaks a runaway loop. test always fails and loops back to
// impl; with the engine default cap lowered to 3, the loop runs 3 times then the
// node is force-failed ("max_visits exceeded") and the instance halts — it never
// spins forever.
func TestWorkflowEngine_MaxVisitsBreaksRunawayLoop(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))
	t.Cleanup(agentd.SetWorkflowMaxVisitsForTest(3)) // small cap for a deterministic break

	root := t.TempDir()
	writeToolTemplate(t, root, "runaway",
		"name: runaway\nentry: impl\n",
		"flowchart TD\n impl --> test\n test -->|fail| impl\n test -->|pass| done\n",
		map[string]string{
			"impl": "executor:\n  kind: tool\n  run: echo implementing\n",
			"test": "executor:\n  kind: tool\n  run: echo testing\nverify:\n  kind: tool\n  run: exit 1\non_fail: continue\n",
			"done": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:runaway", "", nil)

	// Plenty of ticks; the cap must stop it well before these run out.
	for range 20 {
		agentd.RunWorkflowEngineTickForTest()
	}

	d := wfGet(t, mux, id)
	assert.Equal(t, "failed", d.Instance.Status, "max_visits halts the runaway loop")
	_, implV := nodeStatusVisits(t, id, "impl")
	assert.Equal(t, int64(3), implV, "impl ran exactly cap (3) times, never more")
	// The max_visits failure is recorded as a node_failed event mentioning the cap.
	events, err := db.ListWorkflowEvents(id, "impl")
	require.NoError(t, err)
	var sawCap bool
	for _, e := range events {
		if e.Kind == db.WorkflowEventNodeFailed && strings.Contains(e.Message, "max_visits") {
			sawCap = true
		}
	}
	assert.True(t, sawCap, "a max_visits-exceeded failure event is recorded")
}

// Scenario: an ai node reporting `failed` with retry budget is re-armed (not
// settled) so the engine re-spawns it next tick — the ai half of in-place retry.
// Drives the node-PATCH path as the worker (its assignee) on a bound, engine-on
// instance.
func TestWorkflowEngine_AINodeRetryReArmsOnFail(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	const conv = "wkr-1111-2222-3333-4444-555566667777"
	f.HaveConvWithTitle(conv, "worker")

	root := t.TempDir()
	writeToolTemplate(t, root, "airetry",
		"name: airetry\nentry: work\n",
		"flowchart TD\n work --> done\n",
		map[string]string{
			"work": "executor:\n  kind: ai\n  agent: worker\n  prompt: do it\nretries: 1\n",
			"done": "executor:\n  kind: tool\n  run: echo done\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreateInGroup(t, mux, "project:airetry", "", nil, "squad")

	// Model the spawned-and-running state: work is running, owned by the worker.
	bindRunning(t, id, "work", conv)

	// The node reports failed via the node-PATCH path; with retry budget on a
	// bound, engine-on instance the engine re-arms it instead of settling failed.
	wfPatch(t, mux, id, "work", map[string]any{"status": "failed"})

	st, _ := nodeStatusVisits(t, id, "work")
	assert.Equal(t, "ready", st, "an ai node with retry budget is re-armed, not failed")
	n, _ := db.GetWorkflowNode(id, "work")
	assert.Empty(t, n.Assignee, "the assignee is cleared so the engine re-spawns a fresh agent")
	assert.Equal(t, 1, countEvents(t, id, "work", db.WorkflowEventNodeRetry), "a retry was recorded")

	// Budget spent: a SECOND failure (the agent re-spawned, ran, failed again)
	// settles the node failed — the interception falls through to a real settle.
	bindRunning(t, id, "work", conv)
	wfPatch(t, mux, id, "work", map[string]any{"status": "failed"})
	st2, _ := nodeStatusVisits(t, id, "work")
	assert.Equal(t, "failed", st2, "once retries:1 is spent, the next failure settles the node failed")
}

// Scenario: the JOH-39 ↔ JOH-40 seam. When a back-edge re-arms a loop body that
// contains a bound ai node, that node's assignee is CLEARED — so the engine
// re-spawns it under a fresh conv, which re-fires its JOH-40 handoff with the new
// iteration's input. Here the loop's back-edge source is a tool node (so the
// loop-back is deterministic without spawning), and the instance is unbound (so
// spawnReadyAINodes won't immediately re-spawn the re-armed worker before we
// assert). We assert the re-armed worker is ready with a cleared assignee.
func TestWorkflowEngine_LoopBackClearsAssigneeForReSpawn(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	writeToolTemplate(t, root, "looprespawn",
		"name: looprespawn\nentry: worker\n",
		"flowchart TD\n worker --> check\n check -->|fail| worker\n check -->|pass| done\n",
		map[string]string{
			"worker": "executor:\n  kind: ai\n  agent: worker\n  prompt: do it\n",
			"check":  "executor:\n  kind: tool\n  run: echo checking\nverify:\n  kind: tool\n  run: exit 1\non_fail: continue\n",
			"done":   "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:looprespawn", "", nil) // unbound (no group)

	// Model worker having run once and bound an agent; check is ready to run.
	const conv = "agt-1111-2222-3333-4444-555566667777"
	settleDoneWithOutput(t, id, "worker", "iteration-1 output", "pass")
	ready := db.WorkflowNodeStatusReady
	_, err := db.UpdateWorkflowNode(id, "check", db.WorkflowNodePatch{Status: &ready})
	require.NoError(t, err)
	asg := conv
	_, err = db.UpdateWorkflowNode(id, "worker", db.WorkflowNodePatch{Assignee: &asg})
	require.NoError(t, err)

	// One tick: check runs (tool) → verify fails → |fail| loop-back re-arms worker.
	agentd.RunWorkflowEngineTickForTest()

	w, err := db.GetWorkflowNode(id, "worker")
	require.NoError(t, err)
	assert.Equal(t, "ready", w.Status, "worker is re-armed by the loop-back")
	assert.Empty(t, w.Assignee, "loop-back clears the assignee so a fresh agent (new conv) is spawned → fresh handoff")
	assert.GreaterOrEqual(t, countEvents(t, id, "worker", db.WorkflowEventNodeReentry), 1,
		"a loop re-entry was recorded on the worker node")
}
