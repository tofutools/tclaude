package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The sub-agent ledger (db.SubagentSet) exists because the hook stream is
// lossy: Claude Code fires no hooks at all on a user interrupt
// (anthropics/claude-code#11189) and SubagentStop has no documented
// guarantee for aborts/errors/process death. These tests pin the
// self-healing behaviours that replace the old blind +1/-1 counter.

// ledgerWorld seeds an isolated DB with one env-keyed session row and
// returns an apply() that drives ApplyHook against it as sessionID.
func ledgerWorld(t *testing.T, sessionID, convID string, seed *SessionState) func(input HookCallbackInput) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// ApplyHook consults the task-runner exemption via the env; make sure
	// a task-runner var leaking in from the host doesn't change paths.
	t.Setenv("TCLAUDE_TASK_SIGNAL", "")
	db.ResetForTest()

	if seed == nil {
		seed = &SessionState{Status: StatusIdle}
	}
	seed.ID = sessionID
	seed.ConvID = convID
	require.NoError(t, SaveSessionState(seed))

	return func(input HookCallbackInput) {
		t.Helper()
		if input.ConvID == "" {
			input.ConvID = convID
		}
		if input.Cwd == "" {
			input.Cwd = dir
		}
		require.NoError(t, ApplyHook(input, sessionID), "ApplyHook(%s)", input.HookEventName)
	}
}

func loadState(t *testing.T, sessionID string) *SessionState {
	t.Helper()
	got, err := LoadSessionState(sessionID)
	require.NoError(t, err)
	return got
}

// A lost SubagentStop (the Esc-interrupt case) must not leave a phantom
// "+N" forever: a main-thread SessionStart is a known-zero boundary (a
// (re)starting process has no sub-agents) and clears the ledger.
func TestSubagentLedger_PhantomClearedOnSessionStart(t *testing.T) {
	apply := ledgerWorld(t, "ledger-sess", "conv-ledger", nil)

	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-1", AgentType: "Explore"})
	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-2", AgentType: "Plan"})
	assert.Equal(t, 2, loadState(t, "ledger-sess").SubagentCount, "two sub-agents started")

	// No SubagentStop ever arrives (interrupt / lost hook). The process
	// restarts: the ledger must reset to zero.
	apply(HookCallbackInput{HookEventName: "SessionStart", Source: "startup"})
	got := loadState(t, "ledger-sess")
	assert.Equal(t, 0, got.SubagentCount, "SessionStart is a known-zero boundary")
	assert.Empty(t, got.Subagents, "ledger cleared, not just the cached count")
}

// A lost SubagentStart self-heals via Sight(): the sub-agent's own tool
// hooks (which carry agent_id) re-add it — and must NOT flip the main
// thread's status while doing so (the badge's whole point is flagging
// work under an idle-looking parent).
func TestSubagentLedger_AddOnSightWithoutStatusPollution(t *testing.T) {
	apply := ledgerWorld(t, "sight-sess", "conv-sight", &SessionState{Status: StatusMainAgentIdle})

	// The SubagentStart was lost; the first evidence of the sub-agent is
	// its own PreToolUse.
	apply(HookCallbackInput{HookEventName: "PreToolUse", ToolName: "Bash", AgentID: "ag-lost", AgentType: "Explore"})
	got := loadState(t, "sight-sess")
	assert.Equal(t, 1, got.SubagentCount, "sub-agent re-added on sight")
	assert.Equal(t, StatusMainAgentIdle, got.Status,
		"a sub-agent's tool hook must not flip the parent's status")

	// Its SubagentStop then settles everything, including the
	// main_agent_idle → idle fallback.
	apply(HookCallbackInput{HookEventName: "SubagentStop", AgentID: "ag-lost"})
	got = loadState(t, "sight-sess")
	assert.Equal(t, 0, got.SubagentCount, "sight-added entry removed by its Stop")
	assert.Equal(t, StatusIdle, got.Status, "no sub-agents left: settle to idle")
}

// Regression for the pre-ledger wedge: a background sub-agent's tool
// hook flipped the parent to "working", and the SubagentStop fallback
// (which only fires from main_agent_idle) then left it stuck there
// forever. With the status gate the parent stays main_agent_idle
// throughout and settles to idle.
func TestSubagentLedger_BackgroundSubagentDoesNotWedgeWorking(t *testing.T) {
	apply := ledgerWorld(t, "wedge-sess", "conv-wedge", nil)

	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-bg", AgentType: "claude"})
	apply(HookCallbackInput{HookEventName: "Stop"}) // parent's turn ends, sub-agent lives on
	assert.Equal(t, StatusMainAgentIdle, loadState(t, "wedge-sess").Status)

	apply(HookCallbackInput{HookEventName: "PreToolUse", ToolName: "Read", AgentID: "ag-bg"})
	apply(HookCallbackInput{HookEventName: "PostToolUse", ToolName: "Read", AgentID: "ag-bg"})
	assert.Equal(t, StatusMainAgentIdle, loadState(t, "wedge-sess").Status,
		"background sub-agent activity must not repaint the parent as working")

	apply(HookCallbackInput{HookEventName: "SubagentStop", AgentID: "ag-bg"})
	got := loadState(t, "wedge-sess")
	assert.Equal(t, StatusIdle, got.Status, "parent settles to idle when the background sub-agent finishes")
	assert.Equal(t, 0, got.SubagentCount)
}

// The awaiting_* exception to the status gate: a sub-agent acting again
// is the evidence that its permission prompt (surfaced on the parent)
// was answered. The resolved state must be main_agent_idle — NOT
// "working" via the tool arms — because only main_agent_idle is a state
// the SubagentStop settle can take back to idle. The full sequence here
// is the cold-review repro of the wedge: with the old fall-through the
// parent ended this scenario stuck at "working: Bash" forever.
func TestSubagentLedger_SubagentPermissionResolutionDoesNotWedge(t *testing.T) {
	apply := ledgerWorld(t, "perm-sess", "conv-perm", nil)

	// Background sub-agent running, parent's own turn over.
	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-p"})
	apply(HookCallbackInput{HookEventName: "Stop"})
	assert.Equal(t, StatusMainAgentIdle, loadState(t, "perm-sess").Status)

	apply(HookCallbackInput{HookEventName: "PermissionRequest", ToolName: "Bash", AgentID: "ag-p"})
	assert.Equal(t, StatusAwaitingPermission, loadState(t, "perm-sess").Status,
		"a sub-agent's permission prompt surfaces on the parent")

	// The user grants; the sub-agent runs its tool.
	apply(HookCallbackInput{HookEventName: "PostToolUse", ToolName: "Bash", AgentID: "ag-p"})
	got := loadState(t, "perm-sess")
	assert.Equal(t, StatusMainAgentIdle, got.Status,
		"prompt answered: back to main_agent_idle, never 'working' (the wedge state)")
	assert.Equal(t, 1, got.SubagentCount)

	// The sub-agent finishes: the settle must reach plain idle.
	apply(HookCallbackInput{HookEventName: "SubagentStop", AgentID: "ag-p"})
	got = loadState(t, "perm-sess")
	assert.Equal(t, StatusIdle, got.Status,
		"post-permission lifecycle settles to idle — the parent must not wedge busy")
	assert.Equal(t, 0, got.SubagentCount)
}

// An entry that stops being seen ages out after db.SubagentTTL — the
// storage-side self-heal for a lost SubagentStop when no known-zero
// boundary comes along.
func TestSubagentLedger_StaleEntrySweptByTTL(t *testing.T) {
	stale := db.SubagentSet{
		"ag-phantom": {Type: "Explore", Seen: time.Now().Add(-db.SubagentTTL - time.Minute)},
		"ag-fresh":   {Type: "Plan", Seen: time.Now()},
	}
	apply := ledgerWorld(t, "ttl-sess", "conv-ttl",
		&SessionState{Status: StatusWorking, SubagentCount: 2, Subagents: stale})

	// Any hook triggers the sweep.
	apply(HookCallbackInput{HookEventName: "UserPromptSubmit"})
	got := loadState(t, "ttl-sess")
	assert.Equal(t, 1, got.SubagentCount, "expired phantom swept, fresh entry kept")
	assert.Contains(t, got.Subagents, "ag-fresh")
	assert.NotContains(t, got.Subagents, "ag-phantom")
}

// Payloads without agent_id still count (legacy semantics via synthetic
// anon entries), and a later real id folds into its anon placeholder
// instead of double-counting the same sub-agent.
func TestSubagentLedger_AnonEntriesAndSightFolding(t *testing.T) {
	apply := ledgerWorld(t, "anon-sess", "conv-anon", nil)

	apply(HookCallbackInput{HookEventName: "SubagentStart"}) // no agent_id
	apply(HookCallbackInput{HookEventName: "SubagentStart"}) // no agent_id
	assert.Equal(t, 2, loadState(t, "anon-sess").SubagentCount, "id-less starts still count")

	// One of them shows up with a real id: fold, don't double-count.
	apply(HookCallbackInput{HookEventName: "PreToolUse", ToolName: "Bash", AgentID: "ag-real"})
	got := loadState(t, "anon-sess")
	assert.Equal(t, 2, got.SubagentCount, "sighted id consumes an anon placeholder")
	assert.Contains(t, got.Subagents, "ag-real")

	// An id-less Stop removes the remaining anon entry, not the real id.
	apply(HookCallbackInput{HookEventName: "SubagentStop"})
	got = loadState(t, "anon-sess")
	assert.Equal(t, 1, got.SubagentCount)
	assert.Contains(t, got.Subagents, "ag-real", "anon-first removal keeps identified entries")

	// A Stop for an id already gone (its Start was lost, or a sibling
	// SessionEnd already removed it) must be a no-op, not steal another
	// sub-agent's entry.
	apply(HookCallbackInput{HookEventName: "SubagentStop", AgentID: "ag-never-seen"})
	assert.Equal(t, 1, loadState(t, "anon-sess").SubagentCount, "unknown-id Stop is a no-op")

	apply(HookCallbackInput{HookEventName: "SubagentStop", AgentID: "ag-real"})
	assert.Equal(t, 0, loadState(t, "anon-sess").SubagentCount)
}

// A real process exit clears the ledger: sub-agents live inside the
// process, so none can survive it.
func TestSubagentLedger_SessionEndExitClearsLedger(t *testing.T) {
	apply := ledgerWorld(t, "exit-sess", "conv-exit", nil)

	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-x"})
	apply(HookCallbackInput{HookEventName: "SessionEnd", Reason: "logout"})
	got := loadState(t, "exit-sess")
	assert.Equal(t, StatusExited, got.Status)
	assert.Equal(t, 0, got.SubagentCount, "a dead process has no sub-agents")
	assert.Empty(t, got.Subagents)
}

// A sub-agent's own SessionEnd (agent_id set) removes it from the ledger
// without touching the main thread's status — it complements
// SubagentStop, and removing the same id twice stays a no-op.
func TestSubagentLedger_SubagentSessionEndRemovesEntry(t *testing.T) {
	apply := ledgerWorld(t, "send-sess", "conv-send", &SessionState{Status: StatusWorking})

	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-a"})
	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-b"})

	apply(HookCallbackInput{HookEventName: "SessionEnd", Reason: "other", AgentID: "ag-a"})
	got := loadState(t, "send-sess")
	assert.Equal(t, 1, got.SubagentCount, "sub-agent SessionEnd removes its entry")
	assert.Equal(t, StatusWorking, got.Status, "main status untouched by a sub-agent's SessionEnd")

	// The paired SubagentStop for the same id arrives too: no-op.
	apply(HookCallbackInput{HookEventName: "SubagentStop", AgentID: "ag-a"})
	assert.Equal(t, 1, loadState(t, "send-sess").SubagentCount, "double removal of one sub-agent is a no-op")
}
