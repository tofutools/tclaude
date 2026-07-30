package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive the monitor ledger through the real ApplyHook path
// (reusing ledgerWorld from subagent_ledger_test.go). The point of the
// feature is the very first assertion below: an agent whose only
// outstanding work is a monitor used to settle to plain `idle`.

// monitorPostToolUse builds the PostToolUse payload Claude Code fires
// after a `Monitor` call, in the shape verified empirically from a live
// transcript: the watch's inputs ride in tool_input, and the resulting
// handle comes back as tool_response.taskId alongside the deadline the
// harness will enforce.
func monitorPostToolUse(command, description, taskID string, timeoutMs int64, persistent bool) HookCallbackInput {
	toolInput, _ := json.Marshal(map[string]any{
		"command":     command,
		"description": description,
		"timeout_ms":  timeoutMs,
		"persistent":  persistent,
	})
	resp := map[string]any{"timeoutMs": timeoutMs, "persistent": persistent}
	if taskID != "" {
		resp["taskId"] = taskID
	}
	toolResponse, _ := json.Marshal(resp)
	return HookCallbackInput{
		HookEventName: "PostToolUse",
		ToolName:      "Monitor",
		ToolInput:     toolInput,
		ToolResponse:  toolResponse,
	}
}

func TestMonitorLedger_WatchSurvivesTheTurnAndHoldsTheStatus(t *testing.T) {
	apply := ledgerWorld(t, "mon-sess", "conv-mon", nil)

	apply(monitorPostToolUse(
		"gh pr checks 123 --watch", "CI status for PR 123", "task-1", 600000, false))
	got := loadState(t, "mon-sess")
	require.Len(t, got.Monitors, 1, "a Monitor call is recorded")
	assert.Equal(t, "gh pr checks 123 --watch", got.Monitors["task-1"].Command,
		"the command is what the liveness reconcile later matches on")
	assert.Equal(t, "CI status for PR 123", got.Monitors["task-1"].Label)
	assert.False(t, got.Monitors["task-1"].Deadline.IsZero(),
		"a non-persistent watch records the deadline the harness enforces")

	// The crux: the agent's own turn ends while the watch runs on. It must
	// NOT settle to plain idle — that is the bug this feature fixes.
	apply(HookCallbackInput{HookEventName: "Stop"})
	got = loadState(t, "mon-sess")
	assert.Equal(t, StatusMainAgentIdle, got.Status,
		"an agent waiting on a monitor is not idle")
	assert.Equal(t, "1 monitor running", got.StatusDetail)
	assert.Len(t, got.Monitors, 1, "the ledger survives the turn boundary")

	// TaskStop is the one exit signal the hook stream carries for a
	// monitor, exactly as for a background shell.
	apply(HookCallbackInput{
		HookEventName: "PostToolUse",
		ToolName:      "TaskStop",
		ToolInput:     json.RawMessage(`{"task_id":"task-1"}`),
	})
	got = loadState(t, "mon-sess")
	assert.Empty(t, got.Monitors, "TaskStop removes the task it named")

	apply(HookCallbackInput{HookEventName: "Stop"})
	got = loadState(t, "mon-sess")
	assert.Equal(t, StatusIdle, got.Status, "with nothing left running, the agent is idle")
	assert.Equal(t, "", got.StatusDetail)
}

// A TaskStop names an id from ONE namespace shared by both kinds of
// background task. It must decrement the ledger that owns the id and
// leave the other alone — never both.
func TestMonitorLedger_TaskStopIsRoutedToTheOwningLedger(t *testing.T) {
	apply := ledgerWorld(t, "route-sess", "conv-route", nil)

	apply(bashPostToolUse("npm run dev --port 4321", true, "shell-1"))
	apply(monitorPostToolUse("tail -f app.log", "app errors", "mon-1", 600000, false))
	got := loadState(t, "route-sess")
	require.Len(t, got.BgShells, 1)
	require.Len(t, got.Monitors, 1)

	apply(HookCallbackInput{
		HookEventName: "PostToolUse",
		ToolName:      "TaskStop",
		ToolInput:     json.RawMessage(`{"task_id":"mon-1"}`),
	})
	got = loadState(t, "route-sess")
	assert.Empty(t, got.Monitors, "the monitor the stop named is gone")
	assert.Len(t, got.BgShells, 1, "and the background shell it did not name is untouched")

	apply(HookCallbackInput{HookEventName: "Stop"})
	got = loadState(t, "route-sess")
	assert.Equal(t, StatusMainAgentIdle, got.Status)
	assert.Equal(t, "1 background shell running", got.StatusDetail)
}

// A TaskStop naming something neither ledger owns — the docs allow an
// agent-team teammate or a named background agent — must not silently
// decrement either count.
func TestMonitorLedger_UnknownTaskStopDecrementsNothing(t *testing.T) {
	apply := ledgerWorld(t, "unknown-sess", "conv-unknown", nil)

	apply(monitorPostToolUse("tail -f app.log", "app errors", "mon-1", 600000, false))
	apply(HookCallbackInput{
		HookEventName: "PostToolUse",
		ToolName:      "TaskStop",
		ToolInput:     json.RawMessage(`{"task_id":"reviewer@team"}`),
	})

	got := loadState(t, "unknown-sess")
	assert.Len(t, got.Monitors, 1, "a stop naming a teammate is not evidence about this watch")
}

func TestMonitorLedger_CoexistsWithSubagentsAndShells(t *testing.T) {
	apply := ledgerWorld(t, "mix-sess", "conv-mix", nil)

	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "agent-1", AgentType: "explore"})
	apply(bashPostToolUse("npm run dev --port 4321", true, "shell-1"))
	apply(monitorPostToolUse("tail -f app.log", "app errors", "mon-1", 600000, false))
	apply(HookCallbackInput{HookEventName: "Stop"})

	got := loadState(t, "mix-sess")
	assert.Equal(t, StatusMainAgentIdle, got.Status)
	assert.Equal(t, "1 subagents, 1 background shell, 1 monitor running", got.StatusDetail)
}

func TestMonitorLedger_ClearedWhenTheProcessGoesAway(t *testing.T) {
	apply := ledgerWorld(t, "exit-sess", "conv-exit", nil)

	apply(monitorPostToolUse("tail -f app.log", "app errors", "mon-1", 600000, false))
	require.Len(t, loadState(t, "exit-sess").Monitors, 1)

	// A startup SessionStart is a NEW harness process — a known-zero
	// boundary, since a monitor runs inside the one that is gone.
	apply(HookCallbackInput{HookEventName: "SessionStart", Source: "startup"})
	assert.Empty(t, loadState(t, "exit-sess").Monitors)
}

// A /clear or /resume does NOT restart the harness process, so the watch
// it started really is still running and must not be blanked.
func TestMonitorLedger_SurvivesAnInProcessConvRotation(t *testing.T) {
	apply := ledgerWorld(t, "rot-sess", "conv-rot", nil)

	apply(monitorPostToolUse("tail -f app.log", "app errors", "mon-1", 600000, false))
	apply(HookCallbackInput{HookEventName: "SessionStart", Source: "clear"})
	assert.Len(t, loadState(t, "rot-sess").Monitors, 1,
		"the harness process — and the watch inside it — outlived the rotation")
}

func TestMonitorLedger_NotTrackedForCodex(t *testing.T) {
	apply := ledgerWorld(t, "codex-mon-sess", "conv-codex-mon", &SessionState{
		Status:  StatusIdle,
		Harness: "codex",
	})

	apply(monitorPostToolUse("tail -f app.log", "app errors", "mon-1", 600000, false))
	got := loadState(t, "codex-mon-sess")
	assert.Empty(t, got.Monitors, "Codex has no monitor mechanism to track")

	apply(HookCallbackInput{HookEventName: "Stop"})
	assert.Equal(t, StatusIdle, loadState(t, "codex-mon-sess").Status,
		"and its status is unaffected")
}
