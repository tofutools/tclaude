package session

import (
	"encoding/json"
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

// A real sub-agent can arrive "unknown" at SubagentStop time when its
// ledger entry was swept during a long model turn. Its stop must still
// settle an otherwise-empty main_agent_idle parent; the unknown id only
// suppresses stopped-only side effects, not the lifecycle settle.
func TestSubagentLedger_StopAfterTTLSweepStillSettles(t *testing.T) {
	stale := db.SubagentSet{
		"ag-slow": {Type: "Explore", Seen: time.Now().Add(-db.SubagentTTL - time.Minute)},
	}
	apply := ledgerWorld(t, "slow-sess", "conv-slow", &SessionState{
		Status:        StatusMainAgentIdle,
		StatusDetail:  "1 subagent running",
		SubagentCount: 1,
		Subagents:     stale,
	})

	apply(HookCallbackInput{HookEventName: "SubagentStop", AgentID: "ag-slow", AgentType: "Explore"})

	got := loadState(t, "slow-sess")
	assert.Equal(t, StatusIdle, got.Status, "delayed real stop settles the parent after sweep")
	assert.Empty(t, got.StatusDetail)
	assert.Equal(t, 0, got.SubagentCount)
}

// SessionEnd may remove the ledger entry before the paired SubagentStop.
// That ordering has the same settle requirement as the TTL path.
func TestSubagentLedger_StopAfterSubagentSessionEndStillSettles(t *testing.T) {
	apply := ledgerWorld(t, "paired-sess", "conv-paired", &SessionState{Status: StatusMainAgentIdle})
	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-paired", AgentType: "Explore"})

	apply(HookCallbackInput{HookEventName: "SessionEnd", Reason: "other", AgentID: "ag-paired"})
	require.Equal(t, StatusMainAgentIdle, loadState(t, "paired-sess").Status)

	apply(HookCallbackInput{HookEventName: "SubagentStop", AgentID: "ag-paired", AgentType: "Explore"})

	got := loadState(t, "paired-sess")
	assert.Equal(t, StatusIdle, got.Status, "paired stop settles after SessionEnd removed the ledger entry")
	assert.Empty(t, got.StatusDetail)
	assert.Equal(t, 0, got.SubagentCount)
}

// Claude Code 2.1.220 ends EVERY main-thread turn with a synthetic
// SubagentStop: a freshly minted agent_id no SubagentStart ever
// announced, an empty agent_type, and an agent_transcript_path pointing
// at a file that does not exist — fired immediately after the turn's real
// Stop. Captured verbatim from a tapped 2.1.220 session; each turn's id
// differs, so it can never match a ledger entry.
//
// It describes the main thread finishing, not a sub-agent, so it must not
// reach the sub-agent lifecycle arm. These pin the contract rather than
// reproduce a live defect: at the ordering 2.1.220 produces (always after
// Stop) the old fall-through happened to be a no-op, so they pass either
// way. What they stop is a future ordering — or a future edit — turning a
// non-sub-agent event into a main-thread status transition.
func TestSubagentLedger_SyntheticTurnEndStopIsNotASubagent(t *testing.T) {
	apply := ledgerWorld(t, "phantom-sess", "conv-phantom", nil)

	// A background shell keeps the parent at main_agent_idle after Stop —
	// the state the sub-agent lifecycle arm acts on.
	apply(HookCallbackInput{
		HookEventName: "PostToolUse",
		ToolName:      "Bash",
		ToolInput:     json.RawMessage(`{"command":"sleep 300","run_in_background":true}`),
	})
	apply(HookCallbackInput{HookEventName: "Stop"})

	before := loadState(t, "phantom-sess")
	require.Equal(t, StatusMainAgentIdle, before.Status, "background shell holds the turn open")
	require.Len(t, before.BgShells, 1)

	apply(HookCallbackInput{HookEventName: "SubagentStop", AgentID: "a6e8d5931bc6aaaa1", AgentType: ""})

	got := loadState(t, "phantom-sess")
	assert.Equal(t, before.Status, got.Status, "synthetic turn-end stop must not move the main status")
	assert.Equal(t, before.StatusDetail, got.StatusDetail)
	assert.Equal(t, 0, got.SubagentCount)
	assert.Len(t, got.BgShells, 1, "the background-shell ledger is untouched")
}

// The same synthetic event must not be mistaken for "a sub-agent acted
// again, so the permission prompt parked on the parent was answered".
// Only a hook from a sub-agent the ledger actually knows carries that
// evidence; an unannounced id carries none.
func TestSubagentLedger_SyntheticTurnEndStopLeavesAwaitingAlone(t *testing.T) {
	apply := ledgerWorld(t, "phantom-perm-sess", "conv-phantom-perm", nil)

	apply(HookCallbackInput{HookEventName: "PermissionRequest", ToolName: "Bash"})
	require.Equal(t, StatusAwaitingPermission, loadState(t, "phantom-perm-sess").Status)

	apply(HookCallbackInput{HookEventName: "SubagentStop", AgentID: "adcba0fdc4cf55998", AgentType: ""})

	assert.Equal(t, StatusAwaitingPermission, loadState(t, "phantom-perm-sess").Status,
		"the parent is still waiting on the human")
}

// A SubagentStop for a sub-agent the ledger DOES know keeps its full
// lifecycle behaviour — the guard discriminates on ledger membership
// alone, so nothing about the real path changes.
func TestSubagentLedger_KnownSubagentStopStillSettles(t *testing.T) {
	apply := ledgerWorld(t, "known-sess", "conv-known", nil)

	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-real", AgentType: "Explore"})
	apply(HookCallbackInput{HookEventName: "Stop"})
	require.Equal(t, StatusMainAgentIdle, loadState(t, "known-sess").Status)

	apply(HookCallbackInput{HookEventName: "SubagentStop", AgentID: "ag-real", AgentType: "Explore"})

	got := loadState(t, "known-sess")
	assert.Equal(t, StatusIdle, got.Status, "a real sub-agent finishing still settles the parent")
	assert.Equal(t, 0, got.SubagentCount)
}
