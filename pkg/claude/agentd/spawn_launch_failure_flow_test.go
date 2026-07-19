package agentd_test

import (
	"bytes"
	"fmt"
	"testing"

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
