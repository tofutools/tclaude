package agentd_test

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Scenario: an agent starts a MONITOR — a `Monitor` watch tailing a log or
// polling a CI job — and its own turn then ends. Claude Code fires a
// PostToolUse when the watch STARTS and no hook whatsoever when it ends
// (the completion arrives in-transcript as a task notification, which no
// hook observes), so before this feature the agent settled to plain `idle`
// while it was actually waiting on that watch.
//
// This is the same shape as the background-shell scenario next door, and
// deliberately reuses its process-table stand-ins: a command monitor is a
// descendant process of the harness exactly as a background shell is.

// monitorLaunchHook builds the PostToolUse payload for a `Monitor` call,
// in the shape verified against a live Claude Code session: the watch's
// inputs in tool_input, and taskId / timeoutMs / persistent coming back in
// tool_response.
func monitorLaunchHook(conv, cwd, command, taskID string, timeoutMs int64) session.HookCallbackInput {
	toolInput, _ := json.Marshal(map[string]any{
		"command": command, "description": "watch", "timeout_ms": timeoutMs, "persistent": false,
	})
	toolResponse, _ := json.Marshal(map[string]any{
		"taskId": taskID, "timeoutMs": timeoutMs, "persistent": false,
	})
	return session.HookCallbackInput{
		HookEventName: "PostToolUse",
		ToolName:      "Monitor",
		ConvID:        conv,
		Cwd:           cwd,
		ToolInput:     toolInput,
		ToolResponse:  toolResponse,
	}
}

// wsMonitorLaunchHook is the websocket form: no command, so no descendant
// process for the reconcile to match.
func wsMonitorLaunchHook(conv, cwd, url, taskID string, timeoutMs int64) session.HookCallbackInput {
	toolInput, _ := json.Marshal(map[string]any{
		"ws": map[string]any{"url": url}, "timeout_ms": timeoutMs, "persistent": false,
	})
	toolResponse, _ := json.Marshal(map[string]any{
		"taskId": taskID, "timeoutMs": timeoutMs, "persistent": false,
	})
	return session.HookCallbackInput{
		HookEventName: "PostToolUse",
		ToolName:      "Monitor",
		ConvID:        conv,
		Cwd:           cwd,
		ToolInput:     toolInput,
		ToolResponse:  toolResponse,
	}
}

func TestDashboardSnapshot_MonitorCountSurvivesMainAgentStop(t *testing.T) {
	const conv = "mons-1111-2222-3333-4444"
	const label = "spwn-mons"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBgShellReconcileCacheForTest)
	stubLiveBgShellCommands(t, "gh pr checks 123 --watch", "tail -f /var/log/deploy.log")

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-mons", f.TestCwd("mons"))
	f.HaveMember("squad", conv)
	cwd := f.TestCwd("mons")

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

	// 1) A monitor starts.
	apply(monitorLaunchHook(conv, cwd, "gh pr checks 123 --watch", "task-1", 600000))
	assert.Equal(t, 1, member().State.MonitorCount, "monitor_count after the Monitor call")

	// 2) The crux: the agent's turn ends while the watch runs on. The
	//    badge must survive, and the agent must not render as plain idle.
	apply(session.HookCallbackInput{HookEventName: "Stop", ConvID: conv, Cwd: cwd})
	got := member()
	assert.Equal(t, 1, got.State.MonitorCount, "monitor_count must survive the agent's Stop")
	assert.Equal(t, session.StatusMainAgentIdle, got.State.Status,
		"an agent waiting on a monitor is not idle")
	assert.Equal(t, "1 monitor running", got.State.StatusDetail)

	// 3) A second one; the badge counts both.
	apply(monitorLaunchHook(conv, cwd, "tail -f /var/log/deploy.log", "task-2", 600000))
	assert.Equal(t, 2, member().State.MonitorCount)

	// 4) TaskStop cancels one by id — the single exit signal the hook
	//    stream does carry for a monitor.
	apply(session.HookCallbackInput{
		HookEventName: "PostToolUse", ToolName: "TaskStop", ConvID: conv, Cwd: cwd,
		ToolInput: json.RawMessage(`{"task_id":"task-2"}`),
	})
	assert.Equal(t, 1, member().State.MonitorCount, "TaskStop removes the task it named")

	// 5) The last one is cancelled too: the agent finally settles to idle.
	apply(session.HookCallbackInput{
		HookEventName: "PostToolUse", ToolName: "TaskStop", ConvID: conv, Cwd: cwd,
		ToolInput: json.RawMessage(`{"task_id":"task-1"}`),
	})
	apply(session.HookCallbackInput{HookEventName: "Stop", ConvID: conv, Cwd: cwd})
	got = member()
	assert.Zero(t, got.State.MonitorCount)
	assert.Equal(t, session.StatusIdle, got.State.Status, "nothing left running: plain idle")
}

// The two badges are independent, and a TaskStop names an id from the one
// namespace they share. It must decrement the ledger that owns the id and
// leave the other alone.
func TestDashboardSnapshot_MonitorAndBgShellBadgesAreIndependent(t *testing.T) {
	const conv = "monx-1111-2222-3333-4444"
	const label = "spwn-monx"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBgShellReconcileCacheForTest)
	stubLiveBgShellCommands(t, "npm run dev --port 4321", "gh pr checks 123 --watch")

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-monx", f.TestCwd("monx"))
	f.HaveMember("squad", conv)
	cwd := f.TestCwd("monx")

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

	apply(bgLaunchHook(conv, cwd, "npm run dev --port 4321", "shell-1"))
	apply(monitorLaunchHook(conv, cwd, "gh pr checks 123 --watch", "mon-1", 600000))
	apply(session.HookCallbackInput{HookEventName: "Stop", ConvID: conv, Cwd: cwd})

	got := member()
	assert.Equal(t, 1, got.State.BgShellCount)
	assert.Equal(t, 1, got.State.MonitorCount)
	assert.Equal(t, "1 background shell, 1 monitor running", got.State.StatusDetail,
		"the pill names both kinds beside the two badges")

	apply(session.HookCallbackInput{
		HookEventName: "PostToolUse", ToolName: "TaskStop", ConvID: conv, Cwd: cwd,
		ToolInput: json.RawMessage(`{"task_id":"mon-1"}`),
	})
	got = member()
	assert.Equal(t, 1, got.State.BgShellCount, "the shell the stop did not name is untouched")
	assert.Zero(t, got.State.MonitorCount)
}

// A monitor that simply ENDS — its stream closes, or its deadline passes —
// fires no hook at all, so the badge can only clear if the daemon notices
// the process is gone. This drives the real reconcile against real
// processes, the test's own children standing in for the agent's.
func TestDashboardSnapshot_MonitorReconcileRetiresAFinishedWatch(t *testing.T) {
	const conv = "monr-1111-2222-3333-4444"
	const label = "spwn-monr"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBgShellReconcileCacheForTest)

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-monr", f.TestCwd("monr"))
	f.HaveMember("squad", conv)
	cwd := f.TestCwd("monr")

	// The reconcile enumerates the processes below the session row's
	// recorded pid. Point it at this test process so its own children play
	// the part of the agent's watches. Re-stamped before every read for the
	// same reason as the background-shell test: ApplyHook re-derives the
	// row's pid from its own ancestry.
	usePID := func() {
		t.Helper()
		row, err := db.LoadSession(label)
		require.NoError(t, err)
		row.PID = os.Getpid()
		require.NoError(t, db.SaveSession(row))
	}

	marker := fmt.Sprintf("tcl-monitor-flow-%d", os.Getpid())
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

	apply(monitorLaunchHook(conv, cwd, alive, "mon-alive", 3600000))
	apply(monitorLaunchHook(conv, cwd, finished, "mon-finished", 3600000))
	apply(session.HookCallbackInput{HookEventName: "Stop", ConvID: conv, Cwd: cwd})

	// The ledger holds both (no end hook ever fires), but only one process
	// exists — so the badge must read 1, not 2.
	var got *dashMember
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got = member(); got.State.MonitorCount == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Equal(t, 1, got.State.MonitorCount,
		"the ledger holds 2 but only 1 process is alive — the ghost must not be badged\n%s",
		descendantDump())
	assert.Equal(t, session.StatusMainAgentIdle, got.State.Status,
		"the surviving watch still keeps the agent off plain idle")

	// The retirement is persisted, not just filtered at read time.
	row, err := db.LoadSession(label)
	require.NoError(t, err)
	stored := db.ParseMonitorSet(row.MonitorsJSON)
	assert.Len(t, stored, 1, "the dead entry was removed from the stored ledger")
	_, ghostKept := stored["mon-finished"]
	assert.False(t, ghostKept)

	// Now the survivor ends too. With no hook to announce it, the
	// reconcile is the ONLY thing that can clear the badge.
	stopAlive()
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got = member(); got.State.MonitorCount == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	assert.Zero(t, got.State.MonitorCount,
		"a finished watch clears the badge with no end hook involved")
	assert.Equal(t, session.StatusIdle, got.State.Status,
		"and the agent settles to idle once nothing is left running")
}

// A websocket watch runs inside the harness process. The reconcile must
// decline to have an opinion about it rather than retiring it the instant
// it looks — its deadline and the TTL are its only bounds.
func TestDashboardSnapshot_WebsocketMonitorSurvivesAnEmptyProcessTable(t *testing.T) {
	const conv = "monw-1111-2222-3333-4444"
	const label = "spwn-monw"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBgShellReconcileCacheForTest)
	// Positive evidence that NOTHING is running below the agent. A command
	// watch would be retired by this; a socket one must not be.
	stubLiveBgShellCommands(t)

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-monw", f.TestCwd("monw"))
	f.HaveMember("squad", conv)
	cwd := f.TestCwd("monw")

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

	apply(wsMonitorLaunchHook(conv, cwd, "wss://events.example.com/stream", "ws-1", 3600000))
	apply(monitorLaunchHook(conv, cwd, "gh pr checks 123 --watch", "cmd-1", 3600000))
	apply(session.HookCallbackInput{HookEventName: "Stop", ConvID: conv, Cwd: cwd})

	got := member()
	assert.Equal(t, 1, got.State.MonitorCount,
		"the command watch is retired by the empty process table; the socket one is not")
	assert.Equal(t, session.StatusMainAgentIdle, got.State.Status)

	row, err := db.LoadSession(label)
	require.NoError(t, err)
	stored := db.ParseMonitorSet(row.MonitorsJSON)
	require.Len(t, stored, 1)
	assert.True(t, stored["ws-1"].WS, "the surviving entry is the websocket watch")
}
