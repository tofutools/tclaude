package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive the background-shell ledger through the real
// ApplyHook path (reusing ledgerWorld from subagent_ledger_test.go), so
// they pin what the hook stream alone can and cannot do. What it CAN do:
// record a launch, honour a TaskStop, and clear at a process boundary.
// What it cannot: notice a shell exiting on its own — that is the
// daemon's liveness reconcile, covered in bgshell_test.go and the agentd
// flow tests.

func TestBgShellLedger_LaunchSurvivesTheTurnAndHoldsTheStatus(t *testing.T) {
	apply := ledgerWorld(t, "bg-sess", "conv-bg", nil)

	launch := bashPostToolUse("npm run dev --port 4321", true, "task-1")
	apply(launch)
	got := loadState(t, "bg-sess")
	require.Len(t, got.BgShells, 1, "a backgrounded Bash is recorded")
	assert.Equal(t, "npm run dev --port 4321", got.BgShells["task-1"].Command,
		"the command is what the liveness reconcile later matches on")

	// The crux: the agent's own turn ends while the shell runs on. It must
	// NOT settle to plain idle — that is the bug this feature fixes.
	apply(HookCallbackInput{HookEventName: "Stop"})
	got = loadState(t, "bg-sess")
	assert.Equal(t, StatusMainAgentIdle, got.Status,
		"an agent waiting on a background command is not idle")
	assert.Equal(t, "1 background shell running", got.StatusDetail)
	assert.Len(t, got.BgShells, 1, "the ledger survives the turn boundary")

	// TaskStop is the one exit signal the hook stream does carry.
	apply(HookCallbackInput{
		HookEventName: "PostToolUse",
		ToolName:      "TaskStop",
		ToolInput:     json.RawMessage(`{"task_id":"task-1"}`),
	})
	got = loadState(t, "bg-sess")
	assert.Empty(t, got.BgShells, "TaskStop removes the task it named")

	apply(HookCallbackInput{HookEventName: "Stop"})
	got = loadState(t, "bg-sess")
	assert.Equal(t, StatusIdle, got.Status, "with nothing left running, the agent is idle")
	assert.Equal(t, "", got.StatusDetail)
}

// A FOREGROUND Bash — by far the common case, firing on every tool use —
// must never touch the ledger.
func TestBgShellLedger_ForegroundBashIsNotRecorded(t *testing.T) {
	apply := ledgerWorld(t, "fg-sess", "conv-fg", nil)

	apply(bashPostToolUse("go test ./...", false, ""))
	apply(HookCallbackInput{HookEventName: "PostToolUse", ToolName: "Read"})
	apply(HookCallbackInput{HookEventName: "Stop"})

	got := loadState(t, "fg-sess")
	assert.Empty(t, got.BgShells)
	assert.Equal(t, StatusIdle, got.Status, "an ordinary turn still settles to idle")
}

// Sub-agents and background shells are independent: neither may mask the
// other, and the combined detail names both.
func TestBgShellLedger_CoexistsWithSubagents(t *testing.T) {
	apply := ledgerWorld(t, "both-sess", "conv-both", nil)

	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-1", AgentType: "Explore"})
	apply(bashPostToolUse("npm run dev", true, "task-1"))
	apply(HookCallbackInput{HookEventName: "Stop"})

	got := loadState(t, "both-sess")
	assert.Equal(t, StatusMainAgentIdle, got.Status)
	assert.Equal(t, "1 subagents, 1 background shell running", got.StatusDetail)

	// The sub-agent finishes; the shell is still running, so the agent
	// must NOT fall back to idle.
	apply(HookCallbackInput{HookEventName: "SubagentStop", AgentID: "ag-1"})
	got = loadState(t, "both-sess")
	assert.Equal(t, StatusMainAgentIdle, got.Status,
		"the surviving background shell keeps the agent off plain idle")
	assert.Equal(t, "1 background shell running", got.StatusDetail)
	assert.Equal(t, 0, got.SubagentCount)
}

// A shell backgrounded from inside a SUB-AGENT is a child of the same
// harness process and outlives the parent's turn just the same, so it
// belongs in the session's ledger — even though the sub-agent status gate
// returns early for such hooks.
func TestBgShellLedger_RecordedWhenLaunchedFromASubagent(t *testing.T) {
	apply := ledgerWorld(t, "sub-bg-sess", "conv-sub-bg", nil)

	launch := bashPostToolUse("tail -f /var/log/build.log", true, "task-1")
	launch.AgentID = "ag-1"
	apply(launch)

	got := loadState(t, "sub-bg-sess")
	require.Len(t, got.BgShells, 1, "a sub-agent's background shell still counts")
	assert.NotEqual(t, StatusWorking, got.Status,
		"the sub-agent status gate still applies — the parent is not repainted")
}

// Process boundaries are the ledger's known-zero points: background shells
// are children of the harness process and cannot outlive it.
func TestBgShellLedger_ClearedWhenTheProcessGoesAway(t *testing.T) {
	// Session ids stay charset-safe (safeSessionIDRe) — an id with a space
	// takes a different path through ApplyHook entirely.
	for name, boundary := range map[string]HookCallbackInput{
		"startup-SessionStart": {HookEventName: "SessionStart", Source: "startup"},
		"SessionEnd-exit":      {HookEventName: "SessionEnd", Reason: "prompt_input_exit"},
	} {
		t.Run(name, func(t *testing.T) {
			sess := "boundary-" + name
			apply := ledgerWorld(t, sess, "conv-"+name, nil)
			apply(bashPostToolUse("npm run dev", true, "task-1"))
			require.Len(t, loadState(t, sess).BgShells, 1)

			apply(boundary)
			assert.Empty(t, loadState(t, sess).BgShells,
				"a process that went away took its background shells with it")
		})
	}
}

// A /clear or an interactive /resume ends the CONVERSATION but keeps the
// harness process — and its children — alive. Clearing the ledger there
// would blank the badge while the shells kept running, so only a startup
// SessionStart is a true known-zero.
func TestBgShellLedger_SurvivesAnInProcessConvRotation(t *testing.T) {
	apply := ledgerWorld(t, "clear-sess", "conv-clear", nil)
	apply(bashPostToolUse("npm run dev", true, "task-1"))
	require.Len(t, loadState(t, "clear-sess").BgShells, 1)

	apply(HookCallbackInput{HookEventName: "SessionStart", Source: "clear"})
	assert.Len(t, loadState(t, "clear-sess").BgShells, 1,
		"a /clear does not restart the process, so its background shells live on")
}

// Codex fires PostToolUse too. Without the capability gate its rows would
// accumulate entries that nothing — no TaskStop, no reconcile — would ever
// retire before the TTL.
func TestBgShellLedger_NotTrackedForCodex(t *testing.T) {
	apply := ledgerWorld(t, "codex-sess", "conv-codex", &SessionState{
		Status:  StatusIdle,
		Harness: "codex",
	})

	apply(bashPostToolUse("npm run dev", true, "task-1"))
	got := loadState(t, "codex-sess")
	assert.Empty(t, got.BgShells, "Codex has no background-shell mechanism to track")

	apply(HookCallbackInput{HookEventName: "Stop"})
	assert.Equal(t, StatusIdle, loadState(t, "codex-sess").Status,
		"and its status is unaffected")
}
