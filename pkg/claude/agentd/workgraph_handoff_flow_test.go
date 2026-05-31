package agentd_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// JOH-40 — handoffs as inbox messages. When a node settles and the graph
// advances, the engine delivers the completing node's captured output to each
// bound successor agent as a normal inbox message (+ a tmux nudge), so the
// workgraph's data-flow is visible agent-to-agent traffic over tclaude's inbox.
// These scenarios exercise the reconciliation pass (deliverReadyHandoffs) at the
// real surfaces: the agent_messages rows the recipient's `inbox` reads, the
// workgraph_events audit marker, and the TmuxSim nudge.

// handoffMsgs returns the handoff messages (subject contains "handoff") in a
// conv's inbox, most-recent first per ListAgentMessagesForConv.
func handoffMsgs(t *testing.T, conv string) []*db.AgentMessage {
	t.Helper()
	all, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	var out []*db.AgentMessage
	for _, m := range all {
		if strings.Contains(m.Subject, "handoff") {
			out = append(out, m)
		}
	}
	return out
}

// bindRunning marks a node running and assigned to conv — the state the engine
// leaves an ai node in once its agent is spawned (settleAISpawn swaps the
// sentinel for the real conv-id). A bound, running successor is the handoff
// recipient.
func bindRunning(t *testing.T, id int64, nodeID, conv string) {
	t.Helper()
	running := db.WorkgraphNodeStatusRunning
	asg := conv
	_, err := db.UpdateWorkgraphNode(id, nodeID, db.WorkgraphNodePatch{Status: &running, Assignee: &asg})
	require.NoError(t, err)
}

// settleDoneWithOutput marks a node done with a captured output + outcome — the
// state a predecessor lands in after its executor + verify settle.
func settleDoneWithOutput(t *testing.T, id int64, nodeID, output, outcome string) {
	t.Helper()
	done := db.WorkgraphNodeStatusDone
	_, err := db.UpdateWorkgraphNode(id, nodeID,
		db.WorkgraphNodePatch{Status: &done, Output: &output, Outcome: &outcome})
	require.NoError(t, err)
}

// Scenario: a node completes and the engine has spawned its successor's agent;
// one tick delivers the predecessor's captured output to that agent's inbox and
// nudges its pane.
func TestWorkgraphEngine_HandoffDeliveredToBoundSuccessor(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	g := f.HaveGroup("squad")
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	const convB = "bbbb1111-2222-3333-4444-555566667777"
	f.HaveAliveSession(convB, "succ-worker", "tmux-succ", t.TempDir())
	f.HaveMember("squad", convB)

	root := t.TempDir()
	writeToolTemplate(t, root, "handoff",
		"name: handoff\nentry: a\n",
		"flowchart TD\n a --> b\n",
		map[string]string{
			"a": writeAINode("alpha", "do a"),
			"b": writeAINode("beta", "do b"),
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreateInGroup(t, mux, "project:handoff", "", nil, "squad")

	// Model the post-advance state directly: a is done with output; b's agent is
	// already spawned (running + bound), so b is a live handoff recipient.
	const artifact = "ARTIFACT: the-plan-produced-by-node-a"
	settleDoneWithOutput(t, id, "a", artifact, "pass")
	bindRunning(t, id, "b", convB)

	agentd.RunWorkgraphEngineTickForTest()

	msgs := handoffMsgs(t, convB)
	require.Len(t, msgs, 1, "successor's agent should receive exactly one handoff message")
	h := msgs[0]
	assert.Equal(t, agentd.WorkgraphEngineAssigneeForTest(), h.FromConv, "handoff is from the workgraph-engine sentinel")
	assert.Equal(t, g.ID, h.GroupID, "handoff routed through the instance's group")
	assert.Contains(t, h.Body, artifact, "handoff carries the predecessor's captured output")
	assert.Contains(t, h.Body, "(a)", "handoff names the predecessor node id")
	assert.Contains(t, h.Body, "(b)", "handoff orients the recipient to its own node id")

	// The nudge fired to b's pane (same out-of-sandbox tmux path as agent-coord).
	f.AssertSentContains("tmux-succ:0.0", "new agent message", 2*time.Second)

	// A "handoff" audit marker landed on the successor node.
	events, err := db.ListWorkgraphEvents(id, "b")
	require.NoError(t, err)
	var marked bool
	for _, e := range events {
		if e.Kind == db.WorkgraphEventHandoff {
			marked = true
		}
	}
	assert.True(t, marked, "a handoff audit event should be recorded on the successor node")
}

// Scenario: a join successor fed by two settled predecessors receives one
// handoff per predecessor — N upstream branches → N independent messages.
func TestWorkgraphEngine_HandoffJoinDeliversPerPredecessor(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	const convJ = "jjjj1111-2222-3333-4444-555566667777"
	f.HaveAliveSession(convJ, "join-worker", "tmux-join", t.TempDir())
	f.HaveMember("squad", convJ)

	root := t.TempDir()
	writeToolTemplate(t, root, "joinflow",
		"name: joinflow\n",
		"flowchart TD\n a --> j\n c --> j\n",
		map[string]string{
			"a": writeAINode("alpha", "do a"),
			"c": writeAINode("gamma", "do c"),
			"j": writeAINode("joiner", "merge a and c"),
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreateInGroup(t, mux, "project:joinflow", "", nil, "squad")

	const outA = "RESULT-FROM-A-branch"
	const outC = "RESULT-FROM-C-branch"
	settleDoneWithOutput(t, id, "a", outA, "pass")
	settleDoneWithOutput(t, id, "c", outC, "pass")
	bindRunning(t, id, "j", convJ)

	agentd.RunWorkgraphEngineTickForTest()

	msgs := handoffMsgs(t, convJ)
	require.Len(t, msgs, 2, "join successor should receive one handoff per settled predecessor")
	bodies := msgs[0].Body + "\n" + msgs[1].Body
	assert.Contains(t, bodies, outA, "join handoff should carry predecessor a's output")
	assert.Contains(t, bodies, outC, "join handoff should carry predecessor c's output")
}

// Scenario: idempotency / re-derivation + restart. The pass re-derives from
// SQLite every tick; repeated ticks (and, by extension, a daemon restart that
// re-runs the sweep) must never double-deliver — the persisted handoff marker
// suppresses a resend.
func TestWorkgraphEngine_HandoffIdempotentAcrossTicks(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	const convB = "bbbb2222-3333-4444-5555-666677778888"
	f.HaveAliveSession(convB, "succ-worker", "tmux-idem", t.TempDir())
	f.HaveMember("squad", convB)

	root := t.TempDir()
	writeToolTemplate(t, root, "idem",
		"name: idem\nentry: a\n",
		"flowchart TD\n a --> b\n",
		map[string]string{
			"a": writeAINode("alpha", "do a"),
			"b": writeAINode("beta", "do b"),
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreateInGroup(t, mux, "project:idem", "", nil, "squad")

	settleDoneWithOutput(t, id, "a", "the-output", "pass")
	bindRunning(t, id, "b", convB)

	// Three ticks (stand-in for re-derivation + a restart re-running the sweep).
	agentd.RunWorkgraphEngineTickForTest()
	agentd.RunWorkgraphEngineTickForTest()
	agentd.RunWorkgraphEngineTickForTest()

	assert.Len(t, handoffMsgs(t, convB), 1, "repeated ticks must not re-deliver the same handoff")
}

// Scenario: deferred binding. A successor that is ready but NOT yet bound to an
// agent gets no handoff; once its agent is spawned (running + bound) the next
// tick delivers it. This is the "degrade gracefully when no agent is bound yet"
// contract — the handoff arrives when the successor is assigned.
func TestWorkgraphEngine_HandoffDeferredUntilSuccessorBound(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	const convB = "bbbb3333-4444-5555-6666-777788889999"
	f.HaveAliveSession(convB, "succ-worker", "tmux-defer", t.TempDir())
	f.HaveMember("squad", convB)

	root := t.TempDir()
	writeToolTemplate(t, root, "defer",
		"name: defer\nentry: a\n",
		"flowchart TD\n a --> b\n",
		map[string]string{
			"a": writeAINode("alpha", "do a"),
			"b": writeAINode("beta", "do b"),
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreateInGroup(t, mux, "project:defer", "", nil, "squad")

	// a done; b ready but unbound (no agent spawned yet).
	settleDoneWithOutput(t, id, "a", "deferred-output", "pass")
	ready := db.WorkgraphNodeStatusReady
	_, err := db.UpdateWorkgraphNode(id, "b", db.WorkgraphNodePatch{Status: &ready})
	require.NoError(t, err)

	agentd.RunWorkgraphEngineTickForTest()
	assert.Empty(t, handoffMsgs(t, convB), "no handoff while the successor has no bound agent")

	// Now the engine spawns b's agent — bind it and tick again.
	bindRunning(t, id, "b", convB)
	agentd.RunWorkgraphEngineTickForTest()
	assert.Len(t, handoffMsgs(t, convB), 1, "handoff delivered once the successor is bound")
}

// Scenario: the engine opt-in gate also governs handoffs — with the engine OFF,
// no handoff is emitted even with a done predecessor and a bound successor.
// Manual dashboard driving is the human's domain; the dashboard shows the output
// there, so there is no inbox handoff. Guards the gating against silent regress.
func TestWorkgraphEngine_HandoffSuppressedWhenEngineOff(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	// Engine deliberately NOT enabled.

	const convB = "bbbb4444-5555-6666-7777-88889999aaaa"
	f.HaveAliveSession(convB, "succ-worker", "tmux-off", t.TempDir())
	f.HaveMember("squad", convB)

	root := t.TempDir()
	writeToolTemplate(t, root, "engoff",
		"name: engoff\nentry: a\n",
		"flowchart TD\n a --> b\n",
		map[string]string{
			"a": writeAINode("alpha", "do a"),
			"b": writeAINode("beta", "do b"),
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreateInGroup(t, mux, "project:engoff", "", nil, "squad")

	settleDoneWithOutput(t, id, "a", "out", "pass")
	bindRunning(t, id, "b", convB)

	agentd.RunWorkgraphEngineTickForTest()
	assert.Empty(t, handoffMsgs(t, convB), "no handoff should be emitted while the engine is off")
}

// Scenario: a handoff fires only along a TAKEN edge. Node `a` settles with
// outcome `pass`, but its only edge into the join `j` is the `|fail|` branch —
// not taken. `j` becomes ready via `c` (a JoinAny node) and binds. The pass must
// deliver a handoff from `c` (whose unlabeled edge WAS taken) but NOT from `a`,
// even though `a` is `done` — data only flows where the graph routed it.
func TestWorkgraphEngine_HandoffOnlyAlongTakenEdge(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	const convJ = "jjjj9999-8888-7777-6666-555544443333"
	f.HaveAliveSession(convJ, "join-worker", "tmux-taken", t.TempDir())
	f.HaveMember("squad", convJ)

	root := t.TempDir()
	// a reaches j only via |fail| (live only because a sets on_fail: continue);
	// a's success path is a --> aok. c reaches j via the unlabeled (pass) edge.
	writeToolTemplate(t, root, "taken",
		"name: taken\n",
		"flowchart TD\n a -->|fail| j\n a --> aok\n c --> j\n",
		map[string]string{
			"a":   "executor:\n  kind: ai\n  agent: alpha\n  prompt: do a\non_fail: continue\n",
			"aok": writeAINode("aokr", "a-success path"),
			"c":   writeAINode("gamma", "do c"),
			"j":   "label: Join\nexecutor:\n  kind: ai\n  agent: joiner\n  prompt: merge\njoin: any\n",
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreateInGroup(t, mux, "project:taken", "", nil, "squad")

	// a is done with outcome pass (its |fail| edge into j was NOT taken).
	settleDoneWithOutput(t, id, "a", "OUTPUT-FROM-A-not-taken", "pass")
	// c is done with outcome pass (its unlabeled edge into j WAS taken).
	settleDoneWithOutput(t, id, "c", "OUTPUT-FROM-C-taken", "pass")
	bindRunning(t, id, "j", convJ)

	agentd.RunWorkgraphEngineTickForTest()

	msgs := handoffMsgs(t, convJ)
	require.Len(t, msgs, 1, "only the taken-edge predecessor (c) hands off")
	assert.Contains(t, msgs[0].Body, "OUTPUT-FROM-C-taken", "handoff carries c's output")
	assert.Contains(t, msgs[0].Body, "(c)", "handoff is from c")
	assert.NotContains(t, msgs[0].Body, "OUTPUT-FROM-A-not-taken", "a's not-taken-branch output must not leak")
}

// Scenario: a skipped (not-taken-branch) predecessor produces no handoff. A
// decision node routes to one branch; the other branch's node is skipped. The
// downstream join binds and must receive only the live branch's handoff — the
// skipped predecessor hands off nothing.
func TestWorkgraphEngine_HandoffSkippedPredecessorSilent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	const convE = "eeee1111-2222-3333-4444-555566667777"
	f.HaveAliveSession(convE, "end-worker", "tmux-skip", t.TempDir())
	f.HaveMember("squad", convE)

	root := t.TempDir()
	writeToolTemplate(t, root, "skipflow",
		"name: skipflow\n",
		"flowchart TD\n live --> end\n dead --> end\n",
		map[string]string{
			"live": writeAINode("liver", "do live"),
			"dead": writeAINode("deader", "skipped branch"),
			"end":  "executor:\n  kind: ai\n  agent: ender\n  prompt: finish\njoin: any\n",
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreateInGroup(t, mux, "project:skipflow", "", nil, "squad")

	settleDoneWithOutput(t, id, "live", "LIVE-OUTPUT", "pass")
	skipped := db.WorkgraphNodeStatusSkipped
	_, err := db.UpdateWorkgraphNode(id, "dead", db.WorkgraphNodePatch{Status: &skipped})
	require.NoError(t, err)
	bindRunning(t, id, "end", convE)

	agentd.RunWorkgraphEngineTickForTest()

	msgs := handoffMsgs(t, convE)
	require.Len(t, msgs, 1, "only the live predecessor hands off; the skipped one is silent")
	assert.Contains(t, msgs[0].Body, "LIVE-OUTPUT", "handoff carries the live branch's output")
	assert.Contains(t, msgs[0].Body, "(live)", "handoff is from the live branch")
}
