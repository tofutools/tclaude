package agentd_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Scenario (TCL-613): an agent runs a shell command in the BACKGROUND
// (`Bash` with run_in_background) and its own turn then ends. Claude Code
// fires a PostToolUse when the command LAUNCHES and no hook whatsoever
// when it EXITS — so before this feature the agent settled to plain `idle`
// while it was actually waiting on that command.
//
// These tests pin the two halves of the fix on the real dashboard read
// path: the hook-fed ledger that makes the "⚙+N" badge appear, and the
// process-liveness reconcile that makes it honest enough to be worth
// showing (which is what the earlier "no background-shell count" decision
// concluded was impossible).

// bgLaunchHook builds the PostToolUse payload for a backgrounded Bash, in
// the shape verified against a live Claude Code session.
func bgLaunchHook(conv, cwd, command, taskID string) session.HookCallbackInput {
	toolInput, _ := json.Marshal(map[string]any{
		"command": command, "description": "bg", "run_in_background": true,
	})
	toolResponse, _ := json.Marshal(map[string]any{
		"stdout": "", "stderr": "", "interrupted": false, "backgroundTaskId": taskID,
	})
	return session.HookCallbackInput{
		HookEventName: "PostToolUse",
		ToolName:      "Bash",
		ConvID:        conv,
		Cwd:           cwd,
		ToolInput:     toolInput,
		ToolResponse:  toolResponse,
	}
}

func TestDashboardSnapshot_BgShellCountSurvivesMainAgentStop(t *testing.T) {
	const conv = "bgsh-1111-2222-3333-4444"
	const label = "spwn-bgsh"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBgShellReconcileCacheForTest)

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-bgsh", f.TestCwd("bgsh"))
	f.HaveMember("squad", conv)
	cwd := f.TestCwd("bgsh")

	apply := func(in session.HookCallbackInput) {
		t.Helper()
		agentd.ResetBgShellReconcileCacheForTest()
		require.NoError(t, session.ApplyHook(in, label), "ApplyHook(%s)", in.HookEventName)
	}
	member := func() *dashMember {
		t.Helper()
		m := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
		require.NotNil(t, m, "agent %s missing from group squad members", conv)
		return m
	}

	// 1) A background shell launches.
	apply(bgLaunchHook(conv, cwd, "npm run dev --port 4321", "task-1"))
	assert.Equal(t, 1, member().State.BgShellCount, "bg_shell_count after the backgrounded Bash")

	// 2) The crux: the agent's turn ends while the command runs on. The
	//    badge must survive, and the agent must not render as plain idle.
	apply(session.HookCallbackInput{HookEventName: "Stop", ConvID: conv, Cwd: cwd})
	got := member()
	assert.Equal(t, 1, got.State.BgShellCount, "bg_shell_count must survive the agent's Stop")
	assert.Equal(t, session.StatusMainAgentIdle, got.State.Status,
		"an agent waiting on a background command is not idle")
	assert.Equal(t, "1 background shell running", got.State.StatusDetail)

	// 3) A second one; the badge counts both.
	apply(bgLaunchHook(conv, cwd, "pytest -x tests/integration", "task-2"))
	assert.Equal(t, 2, member().State.BgShellCount)

	// 4) TaskStop kills one by id — the single exit signal the hook
	//    stream does carry.
	apply(session.HookCallbackInput{
		HookEventName: "PostToolUse", ToolName: "TaskStop", ConvID: conv, Cwd: cwd,
		ToolInput: json.RawMessage(`{"task_id":"task-2"}`),
	})
	assert.Equal(t, 1, member().State.BgShellCount, "TaskStop removes the task it named")

	// 5) The last one is killed too: the agent finally settles to idle.
	apply(session.HookCallbackInput{
		HookEventName: "PostToolUse", ToolName: "TaskStop", ConvID: conv, Cwd: cwd,
		ToolInput: json.RawMessage(`{"task_id":"task-1"}`),
	})
	apply(session.HookCallbackInput{HookEventName: "Stop", ConvID: conv, Cwd: cwd})
	got = member()
	assert.Zero(t, got.State.BgShellCount)
	assert.Equal(t, session.StatusIdle, got.State.Status, "nothing left running: plain idle")
}

// The half the earlier investigation thought impossible: a background
// shell that simply FINISHES fires no hook at all, so the badge can only
// clear if the daemon notices the process is gone. This drives the real
// reconcile against real processes — the test's own children stand in for
// the agent's.
func TestDashboardSnapshot_BgShellReconcileRetiresAFinishedCommand(t *testing.T) {
	const conv = "bgrc-1111-2222-3333-4444"
	const label = "spwn-bgrc"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBgShellReconcileCacheForTest)

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-bgrc", f.TestCwd("bgrc"))
	f.HaveMember("squad", conv)
	cwd := f.TestCwd("bgrc")

	// The reconcile enumerates the processes below the session row's
	// recorded pid. Point it at this test process so its own children play
	// the part of the agent's background shells. Re-stamped before every
	// read rather than once up front: ApplyHook re-derives the row's pid
	// from its own ancestry (FindClaudePID), which in a test binary
	// resolves to whatever happens to be running the suite.
	usePID := func() {
		t.Helper()
		row, err := db.LoadSession(label)
		require.NoError(t, err)
		row.PID = os.Getpid()
		require.NoError(t, db.SaveSession(row))
	}

	// Two distinct long-running "background commands", plus one that will
	// have already finished by the time the dashboard looks.
	marker := fmt.Sprintf("tcl613-flow-%d", os.Getpid())
	alive := "sleep 120 #" + marker + "-alive"
	finished := "sleep 120 #" + marker + "-finished"
	cmd := exec.Command("sh", "-c", alive)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	apply := func(in session.HookCallbackInput) {
		t.Helper()
		agentd.ResetBgShellReconcileCacheForTest()
		require.NoError(t, session.ApplyHook(in, label), "ApplyHook(%s)", in.HookEventName)
	}
	member := func() *dashMember {
		t.Helper()
		usePID()
		agentd.ResetBgShellReconcileCacheForTest()
		m := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
		require.NotNil(t, m, "agent %s missing from group squad members", conv)
		return m
	}

	apply(bgLaunchHook(conv, cwd, alive, "task-alive"))
	apply(bgLaunchHook(conv, cwd, finished, "task-finished"))
	apply(session.HookCallbackInput{HookEventName: "Stop", ConvID: conv, Cwd: cwd})

	// The ledger holds both (no exit hook ever fires), but only one
	// process exists — so the badge must read 1, not 2. Poll briefly: the
	// child is forked asynchronously.
	var got *dashMember
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got = member(); got.State.BgShellCount == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Equal(t, 1, got.State.BgShellCount,
		"the ledger holds 2 but only 1 process is alive — the ghost must not be badged")
	assert.Equal(t, session.StatusMainAgentIdle, got.State.Status,
		"the surviving command still keeps the agent off plain idle")

	// The retirement is persisted, not just filtered at read time, so the
	// ghost is gone for good rather than re-derived every poll.
	row, err := db.LoadSession(label)
	require.NoError(t, err)
	stored := db.ParseBgShellSet(row.BgShellsJSON)
	assert.Len(t, stored, 1, "the dead entry was removed from the stored ledger")
	_, ghostKept := stored["task-finished"]
	assert.False(t, ghostKept)

	// Now the survivor exits too. With no hook to announce it, the
	// reconcile is the ONLY thing that can clear the badge.
	require.NoError(t, cmd.Process.Kill())
	_, _ = cmd.Process.Wait()
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got = member(); got.State.BgShellCount == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	assert.Zero(t, got.State.BgShellCount,
		"a finished background command clears the badge with no exit hook involved")
	assert.Equal(t, session.StatusIdle, got.State.Status,
		"and the agent settles to idle once nothing is left running")
}

// Background shells are children of the harness process, so an OFFLINE
// agent has none regardless of what its stale row says.
func TestDashboardSnapshot_BgShellCountZeroForOfflineAgent(t *testing.T) {
	const conv = "bgof-1111-2222-3333-4444"
	const label = "spwn-bgof"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBgShellReconcileCacheForTest)

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-bgof", f.TestCwd("bgof"))
	f.HaveMember("squad", conv)

	require.NoError(t, session.ApplyHook(
		bgLaunchHook(conv, f.TestCwd("bgof"), "npm run dev", "task-1"), label))
	require.Equal(t, 1, findDashMember(
		fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv).State.BgShellCount)

	// The pane dies (kill -9: no SessionEnd, no TaskStop, nothing).
	f.MarkOffline("tmux-bgof")
	agentd.ResetBgShellReconcileCacheForTest()

	member := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
	require.NotNil(t, member)
	assert.Zero(t, member.State.BgShellCount,
		"a dead harness process took its background shells with it")
	assert.Equal(t, session.StatusExited, member.State.Status)
}

// Codex has no background-shell mechanism. Its PostToolUse hooks flow
// through the same callback, so without the capability gate a Codex agent
// would grow a badge nothing could ever retire.
func TestDashboardSnapshot_BgShellCountNotShownForCodex(t *testing.T) {
	const conv = "bgcx-1111-2222-3333-4444"
	const label = "spwn-bgcx"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBgShellReconcileCacheForTest)

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveCodexSession(conv, label, "tmux-bgcx", f.TestCwd("bgcx"))
	f.HaveMember("squad", conv)

	require.NoError(t, session.ApplyHook(
		bgLaunchHook(conv, f.TestCwd("bgcx"), "npm run dev", "task-1"), label))

	member := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
	require.NotNil(t, member)
	assert.Zero(t, member.State.BgShellCount, "Codex is unaffected by the background-shell badge")
}
