package agentd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// writeToolTemplate lays down a template under <root>/<name> with the given
// flow.mmd and node files (map of node-id → yaml body). It resolves as
// "project:<name>". A tiny helper so each engine scenario declares just its
// graph + node defs.
func writeToolTemplate(t *testing.T, root, name, workflowYAML, mermaid string, nodes map[string]string) {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nodes"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflow.yaml"), []byte(workflowYAML), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "flow.mmd"), []byte(mermaid), 0o644))
	for id, body := range nodes {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "nodes", id+".yaml"), []byte(body), 0o644))
	}
}

// Scenario: a pure tool-node workflow runs to completion with NO human clicks —
// the engine picks up each ready node, runs its command, captures output,
// verifies, settles, and advances, all from the background tick.
func TestWorkflowEngine_ToolChainAutoCompletes(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	writeToolTemplate(t, root, "toolchain",
		"name: toolchain\nentry: a\n",
		"flowchart TD\n a --> b\n b --> c\n",
		map[string]string{
			"a": "executor:\n  kind: tool\n  run: echo step-a\n",
			"b": "executor:\n  kind: tool\n  run: echo step-b\n",
			"c": "executor:\n  kind: tool\n  run: echo step-c\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:toolchain", "", nil)

	// One tick drains the whole chain (each tool node completes instantly).
	agentd.RunWorkflowEngineTickForTest()

	d := wfGet(t, mux, id)
	assert.Equal(t, "completed", d.Instance.Status, "instance should auto-complete")
	for _, n := range d.Nodes {
		assert.Equal(t, "done", n.Status, "node %s should be done", n.NodeID)
	}
}

// Scenario: an enum tool-verify node selects the outgoing branch from the
// command's produced value — the engine reads the last output line as the enum
// outcome and follows the matching edge, skipping the sibling.
func TestWorkflowEngine_EnumToolVerifySelectsBranch(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	writeToolTemplate(t, root, "enumtool",
		"name: enumtool\nentry: pick\n",
		"flowchart TD\n"+
			" pick -->|left| a\n"+
			" pick -->|right| b\n"+
			" a --> done\n"+
			" b --> done\n",
		map[string]string{
			// pick runs a command whose final line is the enum verdict "left".
			"pick": "executor:\n  kind: tool\n  run: echo left\nverify:\n  kind: enum\n  values: [left, right]\n",
			"a":    "executor:\n  kind: tool\n  run: echo took-a\n",
			"b":    "executor:\n  kind: tool\n  run: echo took-b\n",
			"done": "executor:\n  kind: tool\n  run: echo done\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:enumtool", "", nil)

	agentd.RunWorkflowEngineTickForTest()

	d := wfGet(t, mux, id)
	st := map[string]string{}
	outcome := map[string]string{}
	for _, n := range d.Nodes {
		st[n.NodeID] = n.Status
		outcome[n.NodeID] = n.Outcome
	}
	assert.Equal(t, "done", st["pick"], "pick done")
	assert.Equal(t, "left", outcome["pick"], "pick outcome = left (the produced enum value)")
	assert.Equal(t, "done", st["a"], "branch a taken + completed")
	assert.Equal(t, "skipped", st["b"], "branch b not taken → skipped")
	assert.Equal(t, "completed", d.Instance.Status)
}

// Scenario: a failing tool node halts the instance (default on_fail: stop).
func TestWorkflowEngine_FailingToolHaltsInstance(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	writeToolTemplate(t, root, "failchain",
		"name: failchain\nentry: a\n",
		"flowchart TD\n a --> b\n",
		map[string]string{
			"a": "executor:\n  kind: tool\n  run: \"exit 3\"\n",
			"b": "executor:\n  kind: tool\n  run: echo never\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:failchain", "", nil)

	agentd.RunWorkflowEngineTickForTest()

	d := wfGet(t, mux, id)
	st := map[string]string{}
	for _, n := range d.Nodes {
		st[n.NodeID] = n.Status
	}
	assert.Equal(t, "failed", st["a"], "failing node a")
	// a failed with no |fail| edge, so Advance leaves b unreachable → skipped
	// (same as the manual PATCH path, which also Advances on a fail-settle).
	assert.Equal(t, "skipped", st["b"], "downstream b unreachable after the halt → skipped")
	assert.Equal(t, "failed", d.Instance.Status, "instance halts failed")
}

// Scenario: a tool node failing with on_fail: continue + a |fail| edge follows
// the fail branch instead of halting — the engine routes the failure.
func TestWorkflowEngine_OnFailContinueFollowsFailEdge(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	writeToolTemplate(t, root, "failedge",
		"name: failedge\nentry: a\n",
		"flowchart TD\n a --> ok\n a -->|fail| recover\n",
		map[string]string{
			"a":       "executor:\n  kind: tool\n  run: \"exit 1\"\non_fail: continue\n",
			"ok":      "executor:\n  kind: tool\n  run: echo ok\n",
			"recover": "executor:\n  kind: tool\n  run: echo recovered\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:failedge", "", nil)

	agentd.RunWorkflowEngineTickForTest()

	d := wfGet(t, mux, id)
	st := map[string]string{}
	for _, n := range d.Nodes {
		st[n.NodeID] = n.Status
	}
	assert.Equal(t, "failed", st["a"], "a failed")
	assert.Equal(t, "done", st["recover"], "fail edge → recover ran")
	assert.Equal(t, "skipped", st["ok"], "success edge ok skipped")
	assert.Equal(t, "completed", d.Instance.Status, "instance completes via the fail path")
}

// Scenario: type-preserving interpolation — a captured value flows into a
// downstream command via {{capture}} and {{node.output}}.
func TestWorkflowEngine_CaptureInterpolatesDownstream(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	writeToolTemplate(t, root, "capflow",
		"name: capflow\nentry: produce\n",
		"flowchart TD\n produce --> consume\n consume --> gate\n",
		map[string]string{
			"produce": "executor:\n  kind: tool\n  run: echo hello-world\ncapture: greeting\n",
			// consume echoes the captured greeting; verify it equals what produce captured.
			"consume": "executor:\n  kind: tool\n  run: \"echo {{greeting}}\"\ncapture: echoed\n",
			// gate passes only if the captured value threaded through correctly.
			"gate": "executor:\n  kind: tool\n  run: \"[ '{{produce.output}}' = 'hello-world' ]\"\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:capflow", "", nil)

	agentd.RunWorkflowEngineTickForTest()

	d := wfGet(t, mux, id)
	st := map[string]string{}
	for _, n := range d.Nodes {
		st[n.NodeID] = n.Status
	}
	assert.Equal(t, "done", st["produce"])
	assert.Equal(t, "done", st["consume"])
	assert.Equal(t, "done", st["gate"], "gate proves {{produce.output}} interpolated to the captured value")
	assert.Equal(t, "completed", d.Instance.Status)
}

// Scenario (M1): a node that emits more than the 64KiB capture cap has its
// output truncated in storage, but the captured value stays USABLE downstream —
// the tail (where a command's verdict lives) survives, so {{node.output}} is not
// silently empty. A gate node greps the tail marker to prove it threaded through.
func TestWorkflowEngine_LargeOutputTruncatedButUsable(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	// produce prints ~200KB of filler then a final sentinel line; the tail
	// (incl. the sentinel) must survive the 64KiB cap.
	writeToolTemplate(t, root, "bigout",
		"name: bigout\nentry: produce\n",
		"flowchart TD\n produce --> gate\n",
		map[string]string{
			"produce": "executor:\n  kind: tool\n  run: |\n" +
				"    for i in $(seq 1 4000); do echo 'filler-line-padding-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'; done\n" +
				"    echo SENTINEL-TAIL\n" +
				"capture: blob\n",
			// gate passes only if the captured blob still contains the tail sentinel.
			"gate": "executor:\n  kind: tool\n  run: \"printf '%s' '{{produce.output}}' | grep -q SENTINEL-TAIL\"\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:bigout", "", nil)

	agentd.RunWorkflowEngineTickForTest()

	d := wfGet(t, mux, id)
	st := map[string]string{}
	var produceOut string
	for _, n := range d.Nodes {
		st[n.NodeID] = n.Status
		if n.NodeID == "produce" {
			produceOut = n.Output
		}
	}
	assert.Equal(t, "done", st["produce"])
	assert.Equal(t, "done", st["gate"], "captured tail must survive truncation and interpolate downstream")
	assert.Equal(t, "completed", d.Instance.Status)
	// Stored output is capped (well under the raw ~200KB) but non-empty.
	assert.NotEmpty(t, produceOut, "captured output must not be empty after truncation")
	assert.LessOrEqual(t, len(produceOut), 70*1024, "stored output should be capped near 64KiB")
	assert.Contains(t, produceOut, "SENTINEL-TAIL", "the tail (verdict) must be kept")
	assert.Contains(t, produceOut, "truncated", "a truncation marker should be present")
}

// Scenario: the opt-in gate. With the engine disabled (production default), a
// tick is a no-op — a ready tool node is NOT auto-run.
func TestWorkflowEngine_DisabledIsNoOp(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	// Engine left at its production default (disabled) — no enable call.

	root := t.TempDir()
	writeToolTemplate(t, root, "gated",
		"name: gated\nentry: a\n",
		"flowchart TD\n a --> b\n",
		map[string]string{
			"a": "executor:\n  kind: tool\n  run: echo a\n",
			"b": "executor:\n  kind: tool\n  run: echo b\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:gated", "", nil)

	agentd.RunWorkflowEngineTickForTest() // no-op: engine disabled

	d := wfGet(t, mux, id)
	st := map[string]string{}
	for _, n := range d.Nodes {
		st[n.NodeID] = n.Status
	}
	assert.Equal(t, "ready", st["a"], "disabled engine must not run the ready node")
	assert.Equal(t, "running", d.Instance.Status, "instance untouched, still running")
}

// Scenario: crash recovery. A tool/program node left `running` by a dead daemon
// is reaped back to `ready` at engine startup, so the next tick re-runs it and
// the instance can complete — instead of hanging forever on a corpse node.
func TestWorkflowEngine_ReapsOrphanedRunningNode(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	writeToolTemplate(t, root, "reap",
		"name: reap\nentry: a\n",
		"flowchart TD\n a --> b\n",
		map[string]string{
			"a": "executor:\n  kind: tool\n  run: echo a\n",
			"b": "executor:\n  kind: tool\n  run: echo b\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:reap", "", nil)

	// Simulate a daemon that died mid-command: node a stuck `running` AND
	// carrying the engine-owner sentinel (which claimNextNode stamps).
	running := db.WorkflowNodeStatusRunning
	owner := agentd.WorkflowEngineAssigneeForTest()
	_, err := db.UpdateWorkflowNode(id, "a", db.WorkflowNodePatch{Status: &running, Assignee: &owner})
	require.NoError(t, err)

	// Reap (startup recovery) then tick.
	agentd.ReapOrphanedEngineNodesForTest()
	agentd.RunWorkflowEngineTickForTest()

	d := wfGet(t, mux, id)
	assert.Equal(t, "completed", d.Instance.Status, "instance completes after the orphan is reaped + re-run")
	for _, n := range d.Nodes {
		assert.Equal(t, "done", n.Status, "node %s done", n.NodeID)
	}
}

// Scenario: the reaper must NOT touch a tool node a HUMAN manually drove to
// running (no engine sentinel) — only engine corpses are reaped. Pins the
// cold-review finding that status alone can't distinguish the two.
func TestWorkflowEngine_ReaperLeavesHumanDrivenRunningNode(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	writeToolTemplate(t, root, "humandrive",
		"name: humandrive\nentry: a\n",
		"flowchart TD\n a --> b\n",
		map[string]string{
			"a": "executor:\n  kind: tool\n  run: echo a\n",
			"b": "executor:\n  kind: tool\n  run: echo b\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:humandrive", "", nil)

	// A human manually drove node a to running (no engine sentinel assignee).
	running := db.WorkflowNodeStatusRunning
	_, err := db.UpdateWorkflowNode(id, "a", db.WorkflowNodePatch{Status: &running})
	require.NoError(t, err)

	agentd.ReapOrphanedEngineNodesForTest()

	got, err := db.GetWorkflowNode(id, "a")
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status, "human-driven running node must survive the reaper")
}

// Scenario: the engine never auto-runs a human node — even on a BOUND instance
// (where it WOULD auto-spawn an ai node). A human entry node stays ready after a
// tick; the instance stays running, parked for the dashboard approve/drive path.
func TestWorkflowEngine_LeavesHumanNodeAlone(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	writeToolTemplate(t, root, "humanfirst",
		"name: humanfirst\nentry: review\n",
		"flowchart TD\n review --> ship\n",
		map[string]string{
			"review": "executor:\n  kind: human\n  instructions: look at it\n",
			"ship":   "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreateInGroup(t, mux, "project:humanfirst", "", nil, "squad")

	agentd.RunWorkflowEngineTickForTest()

	d := wfGet(t, mux, id)
	st := map[string]string{}
	for _, n := range d.Nodes {
		st[n.NodeID] = n.Status
	}
	assert.Equal(t, "ready", st["review"], "human node left ready (not auto-run) even on a bound instance")
	assert.Equal(t, "pending", st["ship"], "downstream waits on the human node")
	assert.Equal(t, "running", d.Instance.Status, "instance still running, parked on the human node")
	assert.Empty(t, f.ListGroupMembers("squad"), "engine must not spawn anything for a human node")
}

// writeAINode is the node-yaml for an `ai` executor with the given agent role +
// task prompt — the engine spawns an agent into the instance's bound group and
// hands it this prompt.
func writeAINode(agent, prompt string) string {
	return "executor:\n  kind: ai\n  agent: " + agent + "\n  prompt: " + prompt + "\n"
}

// runningAINode fetches a node and asserts it's running, assigned to a real
// conv-id (not the engine sentinel), and returns that conv-id.
func assertRunningAIAssignee(t *testing.T, id int64, nodeID string) string {
	t.Helper()
	n, err := db.GetWorkflowNode(id, nodeID)
	require.NoError(t, err)
	require.NotNil(t, n, "node %s exists", nodeID)
	assert.Equal(t, "running", n.Status, "ai node %s running", nodeID)
	assert.NotEmpty(t, n.Assignee, "ai node %s has an assignee", nodeID)
	assert.NotEqual(t, agentd.WorkflowEngineAssigneeForTest(), n.Assignee,
		"the sentinel must be swapped for the spawned conv-id after settle")
	return n.Assignee
}

// Scenario: the engine's AI path — a ready ai node on a BOUND-group instance is
// auto-spawned an agent (via the shared executeSpawn core), the node goes
// running with the spawned conv-id as its assignee, and the new agent joins the
// bound group. The downstream node waits (the agent settles it later out-of-band
// via the node-PATCH), and the instance stays running.
func TestWorkflowEngine_AINodeAutoSpawnsIntoBoundGroup(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	writeToolTemplate(t, root, "aifirst",
		"name: aifirst\nentry: plan\n",
		"flowchart TD\n plan --> ship\n",
		map[string]string{
			"plan": writeAINode("planner", "make a plan"),
			"ship": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreateInGroup(t, mux, "project:aifirst", "", nil, "squad")

	agentd.RunWorkflowEngineTickForTest()

	conv := assertRunningAIAssignee(t, id, "plan")

	d := wfGet(t, mux, id)
	st := map[string]string{}
	for _, n := range d.Nodes {
		st[n.NodeID] = n.Status
	}
	assert.Equal(t, "pending", st["ship"], "downstream waits while the agent works async")
	assert.Equal(t, "running", d.Instance.Status, "instance keeps running while the agent works")

	// The spawned agent is a member of the bound group.
	var joined bool
	for _, m := range f.ListGroupMembers("squad") {
		if m.ConvID == conv {
			joined = true
		}
	}
	assert.True(t, joined, "spawned agent %q should have joined squad", conv)
}

// Scenario: the engine never force-spawns an ai node without a group — an ai
// node on an UNBOUND instance is left ready for the dashboard start path.
func TestWorkflowEngine_AINodeUnboundLeftReady(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))

	root := t.TempDir()
	writeToolTemplate(t, root, "aiunbound",
		"name: aiunbound\nentry: plan\n",
		"flowchart TD\n plan --> ship\n",
		map[string]string{
			"plan": writeAINode("planner", "make a plan"),
			"ship": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:aiunbound", "", nil) // no group

	agentd.RunWorkflowEngineTickForTest()

	d := wfGet(t, mux, id)
	st := map[string]string{}
	for _, n := range d.Nodes {
		st[n.NodeID] = n.Status
	}
	assert.Equal(t, "ready", st["plan"], "unbound ai node stays ready (no group to spawn into)")
	assert.Equal(t, "pending", st["ship"], "downstream still pending")
	assert.Equal(t, "running", d.Instance.Status)
}

// Scenario: the opt-in gate covers the AI path too — with the engine disabled, a
// ready ai node on a bound group is NOT auto-spawned.
func TestWorkflowEngine_AIDisabledLeavesNodeReady(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	// Engine left at its production default (disabled) — no enable call.

	root := t.TempDir()
	writeToolTemplate(t, root, "aigated",
		"name: aigated\nentry: plan\n",
		"flowchart TD\n plan --> ship\n",
		map[string]string{
			"plan": writeAINode("planner", "make a plan"),
			"ship": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreateInGroup(t, mux, "project:aigated", "", nil, "squad")

	agentd.RunWorkflowEngineTickForTest() // no-op: engine disabled

	d := wfGet(t, mux, id)
	st := map[string]string{}
	for _, n := range d.Nodes {
		st[n.NodeID] = n.Status
	}
	assert.Equal(t, "ready", st["plan"], "disabled engine must not auto-spawn the ai node")
	assert.Empty(t, f.ListGroupMembers("squad"), "nothing spawned while the engine is off")
}

// Scenario: the per-instance parallelism cap. With two parallel ready ai nodes
// and a per-instance cap of 1, one tick spawns exactly one; the other stays
// ready. Raising the cap to 2 and ticking again spawns the second — proving the
// cap, not chance, gated the first pass.
func TestWorkflowEngine_AIPerInstanceCap(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))
	t.Cleanup(agentd.SetWorkflowAICapsForTest(1, 8)) // per-instance 1, global 8

	root := t.TempDir()
	writeToolTemplate(t, root, "aifanout",
		"name: aifanout\nentry: [a, b]\n",
		"flowchart TD\n a --> done\n b --> done\n",
		map[string]string{
			"a":    writeAINode("worker", "branch a"),
			"b":    writeAINode("worker", "branch b"),
			"done": "executor:\n  kind: tool\n  run: echo done\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreateInGroup(t, mux, "project:aifanout", "", nil, "squad")

	// Cap = 1: one tick spawns exactly one of {a, b}; the other stays ready.
	agentd.RunWorkflowEngineTickForTest()
	running, ready := countAIStatuses(t, id, "a", "b")
	assert.Equal(t, 1, running, "per-instance cap 1 → exactly one ai node running")
	assert.Equal(t, 1, ready, "the capped-out ai node stays ready")
	assert.Len(t, f.ListGroupMembers("squad"), 1, "only one agent spawned under the cap")

	// A second tick at cap 1 still spawns nothing more (one already running).
	agentd.RunWorkflowEngineTickForTest()
	running, ready = countAIStatuses(t, id, "a", "b")
	assert.Equal(t, 1, running, "cap still holds across ticks")
	assert.Equal(t, 1, ready)

	// Raise the cap to 2 and tick: the second ai node now spawns.
	agentd.SetWorkflowAICapsForTest(2, 8)
	agentd.RunWorkflowEngineTickForTest()
	running, _ = countAIStatuses(t, id, "a", "b")
	assert.Equal(t, 2, running, "raising the cap lets the second ai node spawn")
	assert.Len(t, f.ListGroupMembers("squad"), 2, "second agent spawned after the cap was raised")
}

// Scenario: the GLOBAL parallelism cap gates across instances. Two bound
// instances each with a ready ai entry node, a global cap of 1: a single tick
// spawns the ai node of exactly one instance; the other instance's node is held
// ready by the global cap even though its own per-instance budget is free.
func TestWorkflowEngine_AIGlobalCap(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	t.Cleanup(agentd.SetWorkflowEngineEnabledForTest(true))
	t.Cleanup(agentd.SetWorkflowAICapsForTest(1, 1)) // per-instance 1, global 1

	root := t.TempDir()
	writeToolTemplate(t, root, "aionly",
		"name: aionly\nentry: plan\n",
		"flowchart TD\n plan --> done\n",
		map[string]string{
			"plan": writeAINode("worker", "do it"),
			"done": "executor:\n  kind: tool\n  run: echo done\n",
		})
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id1 := wfCreateInGroup(t, mux, "project:aionly", "one", nil, "squad")
	id2 := wfCreateInGroup(t, mux, "project:aionly", "two", nil, "squad")

	agentd.RunWorkflowEngineTickForTest()

	r1, _ := countAIStatuses(t, id1, "plan")
	r2, _ := countAIStatuses(t, id2, "plan")
	assert.Equal(t, 1, r1+r2, "global cap 1 → exactly one ai node running across both instances")
	assert.Len(t, f.ListGroupMembers("squad"), 1, "only one agent spawned globally under the cap")
}

// countAIStatuses tallies how many of the given nodes are running vs ready.
func countAIStatuses(t *testing.T, id int64, nodeIDs ...string) (running, ready int) {
	t.Helper()
	for _, nid := range nodeIDs {
		n, err := db.GetWorkflowNode(id, nid)
		require.NoError(t, err)
		require.NotNil(t, n)
		switch n.Status {
		case "running":
			running++
		case "ready":
			ready++
		}
	}
	return running, ready
}
