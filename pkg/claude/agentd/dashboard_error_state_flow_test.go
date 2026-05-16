package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: an agent's turn ends in an API/auth/billing error. Claude
// Code's StopFailure hook flips its session row to Status="error" with
// the error_type in status_detail — but the CC process is still alive.
// The dashboard snapshot must surface all three cases honestly:
//
//   - errored + alive  → online=true,  state.status="error" (flows through)
//   - healthy + alive  → online=true,  state.status unchanged (control)
//   - errored + dead   → online=false, state.status="exited" (the
//     liveness override wins; a stale "error" must not masquerade as a
//     live state for a process that is gone)
//
// Pins the "errored agent stays frozen on its last successful status"
// bug, and its inverse — that stateForConv does not clobber a live
// agent's "error" status to "exited".
func TestDashboardSnapshot_ErroredAgentSurfacesErrorState(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const erroredConv = "errd-aaaa-bbbb-cccc-dddddddddddd"
	const healthyConv = "okok-aaaa-bbbb-cccc-dddddddddddd"
	const deadConv = "dead-aaaa-bbbb-cccc-dddddddddddd"
	f.HaveConvWithTitle(erroredConv, "errored-worker")
	f.HaveConvWithTitle(healthyConv, "healthy-worker")
	f.HaveConvWithTitle(deadConv, "crashed-worker")
	f.HaveAliveSession(erroredConv, "spwn-errd", "tmux-errd", "/tmp/errd")
	f.HaveAliveSession(healthyConv, "spwn-okok", "tmux-okok", "/tmp/okok")
	f.HaveAliveSession(deadConv, "spwn-dead", "tmux-dead", "/tmp/dead")

	// All three join a group so they surface in the snapshot.
	f.HaveGroup("crew")
	f.HaveMember("crew", erroredConv)
	f.HaveMember("crew", healthyConv)
	f.HaveMember("crew", deadConv)

	// The errored agent: the row the StopFailure hook callback leaves
	// behind — Status="error", error_type in status_detail.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-errd", TmuxSession: "tmux-errd", ConvID: erroredConv,
		Cwd: "/tmp/errd", Status: "error", StatusDetail: "rate_limit",
		LastHook: time.Now(),
	}), "freeze errored session row")
	// The control: a normal working agent.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-okok", TmuxSession: "tmux-okok", ConvID: healthyConv,
		Cwd: "/tmp/okok", Status: "working", LastHook: time.Now(),
	}), "freeze healthy session row")
	// The crashed agent: errored, then its tmux session died.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-dead", TmuxSession: "tmux-dead", ConvID: deadConv,
		Cwd: "/tmp/dead", Status: "error", StatusDetail: "server_error",
		LastHook: time.Now(),
	}), "freeze crashed session row")
	f.MarkOffline("tmux-dead")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	memberOf := func(conv string) *dashMember {
		for _, g := range snap.Groups {
			for i := range g.Members {
				if g.Members[i].ConvID == conv {
					return &g.Members[i]
				}
			}
		}
		return nil
	}

	// Errored + alive: online, and the hook status flows through as "error".
	errd := memberOf(erroredConv)
	require.NotNil(t, errd, "errored conv should be a member of crew")
	assert.True(t, errd.Online,
		"an errored agent's CC process is still alive — it must report online")
	assert.Equal(t, "error", errd.State.Status,
		"a live errored agent must report status=error, not its prior status")
	assert.Equal(t, "rate_limit", errd.State.StatusDetail,
		"error_type must survive into the snapshot's status_detail")

	// Healthy + alive: the control keeps its non-error status untouched.
	ok := memberOf(healthyConv)
	require.NotNil(t, ok, "healthy conv should be a member of crew")
	assert.True(t, ok.Online, "live tmux session → online")
	assert.Equal(t, "working", ok.State.Status,
		"a healthy agent must not be mislabelled as errored")

	// Errored + dead: the liveness override wins — "exited", not a
	// stale "error" that would imply a still-running process.
	dead := memberOf(deadConv)
	require.NotNil(t, dead, "crashed conv should be a member of crew")
	assert.False(t, dead.Online, "dead tmux session → offline")
	assert.Equal(t, "exited", dead.State.Status,
		"a dead agent reports exited even if its last hook status was error")
	assert.NotEqual(t, "error", dead.State.Status,
		"the stale error status must not leak as a live state for a dead agent")
}
