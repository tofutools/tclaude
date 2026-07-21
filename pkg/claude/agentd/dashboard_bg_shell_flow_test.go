package agentd_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
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

// bgTestCommand builds a stand-in background command that is uniquely
// identifiable in the process table.
//
// The shape matters. A shell handed one SIMPLE command exec()s straight
// into it, replacing its own argv — so a marker parked in a trailing
// COMMENT (`sleep 120 #marker`) vanishes from the process table entirely,
// and appending `; true` to that does not help because the comment eats it.
// macOS's /bin/sh exec-optimizes and Linux's happened not to, which is
// exactly the kind of accident a test must not depend on.
//
// Leading with a `:` no-op that CARRIES the marker as a real argument makes
// the script genuinely compound, so the wrapper process survives with the
// whole command inside its argv on both platforms. That is also what the
// real thing looks like: Claude Code's wrapper sources a shell snapshot and
// `eval`s the command, so it is never a single simple command either.
func bgTestCommand(marker string) string {
	return ": " + marker + "; sleep 120"
}

// startWrapperShell launches bgTestCommand under a stand-in for the wrapper
// shell, and returns a stop func that takes the whole subtree down.
//
// Its own process group, killed as a group: killing only the wrapper would
// orphan the `sleep` it started, leaving a stray process behind and making
// "the command finished" a half-truth.
func startWrapperShell(t *testing.T, command string) (stop func()) {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	var once sync.Once
	stop = func() {
		once.Do(func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		})
	}
	t.Cleanup(stop)
	return stop
}

// descendantDump renders what the reconcile can actually see below this test
// process, for use in failure messages. A count mismatch here is otherwise
// almost impossible to diagnose from CI on a platform you cannot reproduce.
func descendantDump() string {
	cmdlines, ok := session.DescendantCommandLines(os.Getpid())
	if !ok {
		return "descendants: <process table unreadable>"
	}
	return fmt.Sprintf("descendants (%d):\n  %s", len(cmdlines), strings.Join(cmdlines, "\n  "))
}

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
	alive := bgTestCommand(marker + "-alive")
	finished := bgTestCommand(marker + "-finished")
	stopAlive := startWrapperShell(t, alive)

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
		"the ledger holds 2 but only 1 process is alive — the ghost must not be badged\n%s", descendantDump())
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
	stopAlive()
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

// The dashboard polls every agent on every tick. A running background
// shell must therefore reach a STEADY STATE: repeated reads must not keep
// rewriting the ledger row, or every poll would also miss the read-through
// cache and re-walk the host's whole process table.
func TestDashboardSnapshot_BgShellReconcileIsStableAcrossPolls(t *testing.T) {
	const conv = "bgst-1111-2222-3333-4444"
	const label = "spwn-bgst"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBgShellReconcileCacheForTest)

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-bgst", f.TestCwd("bgst"))
	f.HaveMember("squad", conv)

	marker := fmt.Sprintf("tcl613-stable-%d", os.Getpid())
	command := bgTestCommand(marker)
	startWrapperShell(t, command)

	require.NoError(t, session.ApplyHook(
		bgLaunchHook(conv, f.TestCwd("bgst"), command, "task-1"), label))

	read := func() (int, string) {
		t.Helper()
		row, err := db.LoadSession(label)
		require.NoError(t, err)
		row.PID = os.Getpid()
		require.NoError(t, db.SaveSession(row))
		agentd.ResetBgShellReconcileCacheForTest()
		m := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
		require.NotNil(t, m)
		after, err := db.LoadSession(label)
		require.NoError(t, err)
		return m.State.BgShellCount, after.BgShellsJSON
	}

	// Let the child appear, then take the settled ledger as the baseline.
	var count int
	var settled string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if count, settled = read(); count == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Equal(t, 1, count, "the live background shell is badged\n%s", descendantDump())

	for i := range 3 {
		count, again := read()
		assert.Equal(t, 1, count, "poll %d still badges the live shell", i)
		assert.Equal(t, settled, again,
			"poll %d rewrote the ledger — a freshly-stamped entry must not be re-stamped every tick", i)
	}
}

// A PARTIAL reconcile — two shells down to one — crosses no zero/nonzero
// boundary, so the stored status_detail (written by the last hook, off the
// counts as they were then) keeps claiming "2 background shells running"
// unless the read path re-renders it. The pill must not contradict the ⚙+1
// badge sitting next to it.
func TestDashboardSnapshot_BgShellDetailTracksAPartialReconcile(t *testing.T) {
	const conv = "bgpd-1111-2222-3333-4444"
	const label = "spwn-bgpd"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBgShellReconcileCacheForTest)

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-bgpd", f.TestCwd("bgpd"))
	f.HaveMember("squad", conv)
	cwd := f.TestCwd("bgpd")

	marker := fmt.Sprintf("tcl613-partial-%d", os.Getpid())
	alive := bgTestCommand(marker + "-alive")
	finished := bgTestCommand(marker + "-finished")
	startWrapperShell(t, alive)

	require.NoError(t, session.ApplyHook(bgLaunchHook(conv, cwd, alive, "task-alive"), label))
	require.NoError(t, session.ApplyHook(bgLaunchHook(conv, cwd, finished, "task-finished"), label))
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "Stop", ConvID: conv, Cwd: cwd,
	}, label))

	// The hook-written row claims two, because at Stop that is what the
	// ledger held and no exit hook ever contradicts it.
	row, err := db.LoadSession(label)
	require.NoError(t, err)
	require.Equal(t, "2 background shells running", row.StatusDetail,
		"precondition: the stored detail is the stale one")

	// Only one of the two processes exists, so the reconcile retires the
	// other — a partial change that moves no boundary.
	var member *dashMember
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		row, err := db.LoadSession(label)
		require.NoError(t, err)
		row.PID = os.Getpid()
		require.NoError(t, db.SaveSession(row))
		agentd.ResetBgShellReconcileCacheForTest()
		member = findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
		require.NotNil(t, member)
		if member.State.BgShellCount == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Equal(t, 1, member.State.BgShellCount,
		"one of the two commands is gone\n%s", descendantDump())
	assert.Equal(t, session.StatusMainAgentIdle, member.State.Status,
		"the surviving command keeps the agent busy")
	assert.Equal(t, "1 background shell running", member.State.StatusDetail,
		"the pill must agree with the badge, not repeat the stale stored count")
}

// An Esc interrupt ends the TURN, not the harness process — so the
// background shells it launched keep running, which is exactly the state
// the badge exists to show. The interrupt recovery path clears the
// sub-agent ledger (an aborted foreground Task fires no SubagentStop), and
// must NOT take the background-shell ledger with it: nothing ever
// re-announces a running background shell, and the liveness reconcile only
// confirms or retires entries the ledger already holds — it cannot invent
// one back. Deleting here would therefore be permanent.
func TestDashboardSnapshot_BgShellSurvivesUserInterrupt(t *testing.T) {
	const conv = "bgin-1111-2222-3333-4444"
	const label = "spwn-bgin"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBgShellReconcileCacheForTest)

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-bgin", f.TestCwd("bgin"))
	f.HaveMember("squad", conv)
	cwd := f.TestCwd("bgin")

	// A background shell is launched, and the agent is mid-turn (working)
	// when the user hits Esc.
	require.NoError(t, session.ApplyHook(bgLaunchHook(conv, cwd, "npm run dev", "task-1"), label))
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "SubagentStart", ConvID: conv, Cwd: cwd,
		AgentID: "ag-doomed", AgentType: "Explore",
	}, label))
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "UserPromptSubmit", ConvID: conv, Cwd: cwd,
	}, label))

	// Claude Code fires NO hook on an interrupt; the .jsonl marker rescan
	// drives this recovery instead.
	n, err := db.MarkSessionsIdleAfterInterrupt(conv)
	require.NoError(t, err)
	require.Positive(t, n, "the working row should have been flipped to idle")

	row, err := db.LoadSession(label)
	require.NoError(t, err)
	assert.Empty(t, db.ParseSubagentSet(row.SubagentsJSON),
		"an aborted foreground Task fires no SubagentStop, so that ledger IS cleared")
	shells := db.ParseBgShellSet(row.BgShellsJSON)
	require.Len(t, shells, 1,
		"the background shell outlives the interrupted turn — deleting it here would be unrecoverable")
	assert.Equal(t, "npm run dev", shells["task-1"].Command)

	// And it still reaches the dashboard, so the agent does not read as
	// plain idle after the interrupt.
	agentd.ResetBgShellReconcileCacheForTest()
	member := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
	require.NotNil(t, member)
	assert.Equal(t, 1, member.State.BgShellCount, "the surviving shell is still badged")
	assert.Equal(t, session.StatusMainAgentIdle, member.State.Status)
	assert.Equal(t, "1 background shell running", member.State.StatusDetail)
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
