package agentd_test

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// rowWritingFailingSpawner reproduces the exact failure shape of the
// "command too long" incident: the forked `tclaude session new` wrapper
// writes its session row (SaveSessionStateForLaunch runs before the tmux
// launch) and THEN dies — tmux refused the command. No pane, no hook, no
// conversation ever exists. The row write mimics a wrapper OLDER than the
// launch-row rollback (or one killed before its own cleanup), so this also
// pins the daemon-side backstop.
type rowWritingFailingSpawner struct{}

func (rowWritingFailingSpawner) SpawnNew(args clcommon.SpawnArgs) error {
	if err := db.SaveSession(&db.SessionRow{
		ID:     args.Label,
		ConvID: args.SessionID,
		Cwd:    args.Cwd,
		Status: "idle",
	}); err != nil {
		return err
	}
	return fmt.Errorf("spawn session wrapper failed: exit status 1: command too long")
}

func (rowWritingFailingSpawner) SpawnResume(clcommon.SpawnArgs) error {
	return fmt.Errorf("resume is not part of this scenario")
}

// Scenario: a launch-enrollment (Claude Code) spawn whose fork fails after
// enrollment ran and after the wrapper wrote its session row — the
// "command too long" incident. The incident left a ghost agent in the
// dashboard's virtual "Ungrouped" group (the enrollment-minted actor row
// survived the membership rollback) and a zombie session row, both of which
// the operator had to clear by hand.
//
// Real surfaces asserted: the spawn CLI fails; /api/snapshot shows no group
// member, no ungrouped ghost, and no leftover conversation; the sessions
// table is empty again.
func TestSpawnLaunchFailure_LeavesNoDanglingRows(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("alpha")
	bridgeAgentClientToMux(t, f.Mux)

	prevSpawn := agentd.Spawn
	agentd.Spawn = rowWritingFailingSpawner{}
	t.Cleanup(func() { agentd.Spawn = prevSpawn })

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(
		&agent.SpawnParams{Group: "alpha", Name: "doomed", InitialMessage: "do the thing"},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	require.NotEqual(t, 0, rc, "spawn must fail when the wrapper dies; stderr=%s", stderr.String())
	require.Nil(t, resp, "a failed spawn returns no response")

	// The zombie session row the wrapper wrote must be gone (daemon backstop).
	rows, err := db.ListSessions()
	require.NoError(t, err, "ListSessions")
	assert.Empty(t, rows, "failed spawn must not leave session rows")

	// The dashboard must be clean: no member in alpha, no ungrouped ghost
	// actor, no leftover conversation row.
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	for _, g := range snap.Groups {
		assert.Emptyf(t, g.Members, "group %q must have no members after the failed spawn", g.Name)
	}
	assert.Empty(t, snap.Ungrouped, "failed spawn must not strand a ghost in Ungrouped")
	assert.Empty(t, snap.Agents, "failed spawn must not strand an actor in the roster")
	assert.Empty(t, snap.Conversations, "failed spawn must not strand a conversation row")

	// And the recovery path stays open: the same spawn succeeds once the
	// launch works again (the rollback must not have poisoned the group).
	agentd.Spawn = f.World.DefaultMocks(t).Spawner
	resp2, rc2 := agent.RunSpawn(
		&agent.SpawnParams{Group: "alpha", Name: "phoenix", InitialMessage: "do the thing"},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	require.Equalf(t, 0, rc2, "retry spawn rc, stderr=%s", stderr.String())
	require.NotNil(t, resp2, "retry spawn response")
	require.NotEmpty(t, resp2.ConvID, "retry spawn conv id")
}

// asyncDyingSpawner models the PROOFLESS launch path: SpawnNew returns nil
// (the wrapper started — fire-and-forget), and the wrapper dies a moment
// later, reported the way liveSpawnNew's reaper goroutine reports it. The
// wrapper died before writing its session row (e.g. its launch script could
// not be written), so the daemon's poll finds nothing.
type asyncDyingSpawner struct{}

func (asyncDyingSpawner) SpawnNew(args clcommon.SpawnArgs) error {
	go func() {
		time.Sleep(100 * time.Millisecond)
		agentd.SignalSpawnWrapperFailureForTest(args.Label,
			fmt.Errorf("spawn session wrapper failed: exit status 1: cannot write launch script"))
	}()
	return nil
}

func (asyncDyingSpawner) SpawnResume(clcommon.SpawnArgs) error {
	return fmt.Errorf("resume is not part of this scenario")
}

// Scenario: a proofless (human-initiated) launch-enrollment spawn whose
// wrapper dies AFTER the fork. liveSpawnNew is fire-and-forget on this path,
// so before the wrapper-failure signal the daemon polled to timeout and
// returned the preset conv-id as a success — reporting a spawn that never
// existed and stranding the pre-fork enrollment as a ghost (cr-1363 cold
// review finding). The spawn must now fail and unwind completely.
func TestSpawnAsyncWrapperDeath_FailsAndUnwinds(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("alpha")
	bridgeAgentClientToMux(t, f.Mux)

	prevSpawn := agentd.Spawn
	agentd.Spawn = asyncDyingSpawner{}
	t.Cleanup(func() { agentd.Spawn = prevSpawn })

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(
		&agent.SpawnParams{Group: "alpha", Name: "doomed-async", InitialMessage: "do the thing"},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	require.NotEqual(t, 0, rc, "spawn must fail when the wrapper dies post-fork; stderr=%s", stderr.String())
	require.Nil(t, resp, "a failed spawn returns no response")
	assert.Contains(t, stderr.String(), "cannot write launch script",
		"the wrapper's failure must surface to the caller, not a poll timeout")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	for _, g := range snap.Groups {
		assert.Emptyf(t, g.Members, "group %q must have no members after the failed spawn", g.Name)
	}
	assert.Empty(t, snap.Ungrouped, "wrapper death must not strand a ghost in Ungrouped")
	assert.Empty(t, snap.Agents, "wrapper death must not strand an actor in the roster")
}
