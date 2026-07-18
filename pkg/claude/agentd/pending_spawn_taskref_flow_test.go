package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario (TCL-568): `agent spawn --task <url>` whose agent materialises
// LATE — the conv-id does not appear within the inline grace, so enrollment
// is back-filled by the pending-spawn sweeper — must still bind the task-
// reference link. Before the fix the pending_spawns row dropped
// TaskURL/TaskLabel, so exactly this path lost the link while the spawn
// reported success (observed live: spawn returned agt_bd0a7472 + success,
// `task show` later said "no task link set").
//
// This pins the whole delayed-materialization contract:
//
//   - The pending row durably carries the requested link (restart-safe —
//     the sweeper path reconstructs from the row alone, never from memory).
//   - The spawn response/CLI reports the link as PENDING, not bound: no
//     claimed linkage before the write actually happened.
//   - One sweep after the conv-id lands, the link is bound to the reserved
//     stable agent identity and rendered on the dashboard snapshot with its
//     derived label — the same production surfaces the inline path feeds.
func TestPendingSpawn_TaskRefSurvivesDelayedMaterialization(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	// Drive the pending path without a real multi-second wait.
	t.Cleanup(agentd.SetAsyncSpawnInlineGraceForTest(50 * time.Millisecond))

	// The gated spawner models a Codex stuck behind a startup gate: session
	// row exists, conv-id absent, so the spawn goes pending.
	gated := &gatedCodexSpawner{
		t:     t,
		w:     f.World,
		inner: f.World.DefaultMocks(t).Spawner,
		sims:  map[string]*testharness.CodexSim{},
	}
	prevSpawn := agentd.Spawn
	agentd.Spawn = gated
	t.Cleanup(func() { agentd.Spawn = prevSpawn })

	g := f.HaveGroup("codex-crew")

	const taskURL = "https://linear.app/acme/issue/TCL-568/spawn-task-race"
	resp, stdout := runSpawnCLI(t, f, &agent.SpawnParams{
		Group:   "codex-crew",
		Name:    "codex-worker",
		Harness: "codex",
		Task:    taskURL,
	})
	require.Empty(t, resp.ConvID, "pending spawn returns an empty conv_id")
	require.NotEmpty(t, resp.AgentID, "pending spawn returns the reserved stable identity")

	// Honesty gate: the response and CLI announce the link as deferred, never
	// as already bound — the write has not happened yet.
	assert.Equal(t, taskURL, resp.TaskRefURL, "response echoes the requested link")
	assert.Equal(t, "pending", resp.TaskRefState, "no bound claim before the write")
	assert.Contains(t, stdout, "Task:    "+taskURL+" (pending", "CLI words the link as deferred")

	// The link rides the durable pending row — the ONLY input the sweeper
	// reconstructs from (a daemon restart must not lose it).
	ps, err := db.GetPendingSpawn(resp.Label)
	require.NoError(t, err)
	require.NotNil(t, ps, "spawn recorded a pending_spawns row")
	assert.Equal(t, taskURL, ps.TaskURL, "pending row persists the task link")

	// Nothing bound yet: the actor row does not carry the link before
	// enrollment.
	ref, err := db.GetAgentTaskRef(resp.AgentID)
	require.NoError(t, err)
	assert.Empty(t, ref.URL, "no task link before the conv-id materialises")

	// The gate clears: Codex takes its first turn, the conv-id lands, one
	// sweep back-fills the enrollment.
	convID := gated.firstTurn(t, resp.Label)
	agentd.RunPendingSpawnSweepForTest()

	gone, err := db.GetPendingSpawn(resp.Label)
	require.NoError(t, err)
	require.Nil(t, gone, "sweeper cleared the pending row")

	boundAgentID, err := db.AgentIDForConv(convID)
	require.NoError(t, err)
	require.Equal(t, resp.AgentID, boundAgentID, "enrollment bound the reserved identity")

	ref, err = db.GetAgentTaskRef(boundAgentID)
	require.NoError(t, err)
	assert.Equal(t, taskURL, ref.URL, "delayed enrollment bound the task link (the TCL-568 loss)")

	// Production read surface: the dashboard snapshot renders the link with
	// its derived Linear label, exactly like an inline spawn's.
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	m := findDashMember(snap, "codex-crew", convID)
	require.NotNil(t, m, "enrolled worker missing from %q members", g.Name)
	assert.Equal(t, taskURL, m.TaskURL)
	assert.Equal(t, "TCL-568", m.TaskLabel, "Linear issue id derived server-side")
}

// Scenario: the inline (launch-enrollment) spawn path reports the task link
// as verifiably bound — the daemon reads it back off the enrolled actor
// before claiming so — and the CLI prints it as a plain fact.
func TestSpawn_TaskRefReportedBoundOnInlinePath(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const taskURL = "https://github.com/tofutools/tclaude/issues/42"
	resp, stdout := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "alpha",
		Name:  "worker",
		Task:  taskURL,
	})
	require.NotEmpty(t, resp.ConvID, "inline spawn resolves its conv-id")
	assert.Equal(t, taskURL, resp.TaskRefURL)
	assert.Equal(t, "bound", resp.TaskRefState, "inline spawn verifies the binding before reporting it")
	assert.Contains(t, stdout, "Task:    "+taskURL+"\n", "CLI prints a verified link without a pending caveat")

	agentID, err := db.AgentIDForConv(resp.ConvID)
	require.NoError(t, err)
	ref, err := db.GetAgentTaskRef(agentID)
	require.NoError(t, err)
	assert.Equal(t, taskURL, ref.URL)
}
