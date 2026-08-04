package agentd_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// exitedPID returns the pid of a process that has already exited, so
// IsProcessAlive reports false for it.
func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, cmd.Run())
	return cmd.Process.Pid
}

// A managed OpenCode server is launched with cwd set to the launch dir BEFORE
// the pane fork, so it holds that directory while no session row claims it yet.
// The runtime row already records the cwd, so cleanup must read it (TCL-1027).
func TestCleanup_Agents_KeepsWorktreeHeldByLiveOpenCodeRuntime(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const leaving = "0c1e0000-1111-2222-3333-444444444444"
	const serving = "0c1e0000-5555-6666-7777-888888888888"
	repo, _ := initRepoOnMain(t)
	shared, err := worktree.AddWorktreeIn(repo, "opencode-held", "main", "")
	require.NoError(t, err)

	// The agent being retired, recorded at the shared root and offline.
	f.HaveConvWithTitle(leaving, "leaving-opencode-root")
	f.HaveAliveSession(leaving, "spwn-ocl", "tmux-ocl", shared)
	f.HaveEnrolledAgent(leaving)
	f.MarkOffline("tmux-ocl")

	// A managed server mid-launch in the same root: process up, no session row
	// yet. Its PID is this test process, which is certainly alive.
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "spwn-opencode-held",
		ConvID:    serving,
		ServerURL: "http://127.0.0.1:43211",
		Password:  "private",
		PID:       os.Getpid(),
		Cwd:       shared,
	}))

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+leaving+`"],"delete":true,"delete_worktrees":true}`)

	require.Len(t, resp.Outcomes, 1)
	assert.Equal(t, 1, resp.Deleted, "the agent itself is still retired")
	assert.Contains(t, resp.Outcomes[0].Detail, "OpenCode server",
		"the note must name the real holder, not a peer agent that does not exist")
	assert.DirExists(t, shared,
		"a live managed OpenCode server's cwd must never be removed")
}

// The server is daemon-owned, not the agent's pane, so retiring or deleting the
// agent that OWNS it does not stop it. Its own retirement must therefore not be
// able to exclude the claim — otherwise the removal runs while the server is
// still sitting in the directory.
func TestCleanup_Agents_KeepsWorktreeHeldByItsOwnLiveOpenCodeRuntime(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const owner = "0c1e5e1f-1111-2222-3333-444444444444"
	repo, _ := initRepoOnMain(t)
	own, err := worktree.AddWorktreeIn(repo, "opencode-self", "main", "")
	require.NoError(t, err)

	f.HaveConvWithTitle(owner, "owns-its-server")
	f.HaveAliveSession(owner, "spwn-ocself", "tmux-ocself", own)
	f.HaveEnrolledAgent(owner)
	f.MarkOffline("tmux-ocself")

	// The retiring agent's OWN server, still alive.
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "spwn-opencode-self",
		ConvID:    owner,
		ServerURL: "http://127.0.0.1:43213",
		Password:  "private",
		PID:       os.Getpid(),
		Cwd:       own,
	}))

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+owner+`"],"delete":true,"delete_worktrees":true}`)

	require.Len(t, resp.Outcomes, 1)
	assert.Contains(t, resp.Outcomes[0].Detail, "OpenCode server")
	assert.DirExists(t, own,
		"an agent's own live server must still block removal of the dir it is in")
}

// A runtime row outlives a crashed daemon. Presence alone must not pin the
// worktree forever — liveness is the runtime manager's own PID check.
func TestCleanup_Agents_RemovesWorktreeWhenOpenCodeRuntimeIsDead(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const leaving = "dead0000-1111-2222-3333-444444444444"
	const serving = "dead0000-5555-6666-7777-888888888888"
	repo, _ := initRepoOnMain(t)
	stale, err := worktree.AddWorktreeIn(repo, "opencode-stale", "main", "")
	require.NoError(t, err)

	f.HaveConvWithTitle(leaving, "leaving-stale-root")
	f.HaveAliveSession(leaving, "spwn-ocs", "tmux-ocs", stale)
	f.HaveEnrolledAgent(leaving)
	f.MarkOffline("tmux-ocs")

	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "spwn-opencode-stale",
		ConvID:    serving,
		ServerURL: "http://127.0.0.1:43212",
		Password:  "private",
		PID:       exitedPID(t),
		Cwd:       stale,
	}))

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+leaving+`"],"delete":true,"delete_worktrees":true}`)

	require.Len(t, resp.Outcomes, 1)
	assert.Equal(t, 1, resp.Deleted)
	assert.NoDirExists(t, stale,
		"a stale runtime row must not pin the worktree after its server died")
}
