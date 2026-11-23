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
	t.Cleanup(db.ResetForTest)

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

// subagent_count is a derived cache of the ledger, and every hook sweeps
// expired ledger entries first. A hook arm that saves and returns early
// must still store the swept count, or the fleet, agent status and
// federation status surfaces (which read the stored count) report a
// sub-agent that the ledger has already dropped.
func TestSubagentLedger_EarlySavesStoreTheSweptCount(t *testing.T) {
	expired := func() db.SubagentSet {
		return db.SubagentSet{"ag-old": {Type: "Explore", Seen: time.Now().Add(-2 * db.SubagentTTL)}}
	}

	t.Run("a best-effort SessionEnd", func(t *testing.T) {
		apply := ledgerWorld(t, "end-sess", "conv-end", &SessionState{
			Status: StatusWorking, Harness: "copilot", Subagents: expired(), SubagentCount: 1,
		})
		apply(HookCallbackInput{HookEventName: "SessionEnd", Reason: "logout"})

		got := loadState(t, "end-sess")
		assert.Empty(t, got.Subagents, "the sweep dropped the expired sub-agent")
		assert.Equal(t, 0, got.SubagentCount, "the stored count follows the ledger")
	})

	t.Run("a manual PostCompact", func(t *testing.T) {
		apply := ledgerWorld(t, "compact-sess", "conv-compact", &SessionState{
			Status: StatusWorking, StatusDetail: "compacting", Subagents: expired(), SubagentCount: 1,
		})
		apply(HookCallbackInput{HookEventName: "PostCompact", Trigger: "manual"})

		got := loadState(t, "compact-sess")
		require.Equal(t, StatusIdle, got.Status, "a manual compaction returns to idle")
		assert.Empty(t, got.Subagents, "the sweep dropped the expired sub-agent")
		assert.Equal(t, 0, got.SubagentCount, "the stored count follows the ledger")
	})
}

// Claude Code (2.1.330) can end a sub-agent with a SubagentHandback tool
// call instead of a plain assistant message. Its PostToolUse carries the
// sub-agent's agent_id and is the last hook naming it; no SubagentStop with
// that id reliably follows. Seen as a sighting, it left a phantom "+1" (and
// held every main-thread Stop at main_agent_idle) for a full TTL.
func TestSubagentLedger_HandbackPostToolUseRemovesEntry(t *testing.T) {
	apply := ledgerWorld(t, "handback-sess", "conv-handback", nil)

	// A foreground sub-agent: the main thread's Agent call is in flight.
	apply(HookCallbackInput{HookEventName: "PreToolUse", ToolName: "Agent"})
	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-hb", AgentType: "web-fetch"})
	apply(HookCallbackInput{HookEventName: "PreToolUse", ToolName: "WebFetch", AgentID: "ag-hb", AgentType: "web-fetch"})
	apply(HookCallbackInput{HookEventName: "PreToolUse", ToolName: "SubagentHandback", AgentID: "ag-hb", AgentType: "web-fetch"})
	require.Equal(t, 1, loadState(t, "handback-sess").SubagentCount,
		"the handback PreToolUse is still the sub-agent running")

	apply(HookCallbackInput{HookEventName: "PostToolUse", ToolName: "SubagentHandback", AgentID: "ag-hb", AgentType: "web-fetch"})
	got := loadState(t, "handback-sess")
	assert.Equal(t, 0, got.SubagentCount, "the handback's PostToolUse ends the sub-agent")
	assert.Empty(t, got.Subagents)
	assert.Equal(t, StatusWorking, got.Status, "the main thread's own turn is still in flight")
	assert.Equal(t, "Agent", got.StatusDetail, "the sub-agent's tool must not repaint the parent")

	apply(HookCallbackInput{HookEventName: "PostToolUse", ToolName: "Agent"})
	apply(HookCallbackInput{HookEventName: "Stop"})
	got = loadState(t, "handback-sess")
	assert.Equal(t, StatusIdle, got.Status, "the main Stop settles to idle, not main_agent_idle")
	assert.Equal(t, 0, got.SubagentCount)

	// A late SubagentStop for the same id, if one does arrive, finds it gone
	// and must not move the settled status.
	apply(HookCallbackInput{HookEventName: "SubagentStop", AgentID: "ag-hb", AgentType: "web-fetch"})
	got = loadState(t, "handback-sess")
	assert.Equal(t, StatusIdle, got.Status)
	assert.Equal(t, 0, got.SubagentCount)
}

// A background sub-agent that hands back after the main thread's turn ended
// settles the parent from main_agent_idle to idle, as its SubagentStop would.
func TestSubagentLedger_BackgroundHandbackSettlesParent(t *testing.T) {
	apply := ledgerWorld(t, "bg-handback-sess", "conv-bg-handback", nil)

	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-bg", AgentType: "Explore"})
	apply(HookCallbackInput{HookEventName: "Stop"})
	require.Equal(t, StatusMainAgentIdle, loadState(t, "bg-handback-sess").Status)

	apply(HookCallbackInput{HookEventName: "PostToolUse", ToolName: "SubagentHandback", AgentID: "ag-bg", AgentType: "Explore"})
	got := loadState(t, "bg-handback-sess")
	assert.Equal(t, StatusIdle, got.Status, "the handback settles the parent")
	assert.Empty(t, got.StatusDetail)
	assert.Equal(t, 0, got.SubagentCount)
}

// A handback that failed does not end the sub-agent: it carries on, so the
// failure is an ordinary sighting.
func TestSubagentLedger_FailedHandbackKeepsEntry(t *testing.T) {
	apply := ledgerWorld(t, "hb-fail-sess", "conv-hb-fail", nil)

	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-f", AgentType: "Explore"})
	apply(HookCallbackInput{HookEventName: "Stop"})
	apply(HookCallbackInput{HookEventName: "PostToolUseFailure", ToolName: "SubagentHandback", AgentID: "ag-f", AgentType: "Explore"})

	got := loadState(t, "hb-fail-sess")
	assert.Equal(t, 1, got.SubagentCount)
	assert.Equal(t, StatusMainAgentIdle, got.Status)
}

// A background sub-agent handing back while the main thread waits on a
// permission prompt must leave the parent awaiting the human, as a
// SubagentStop does — not settle it to idle and fire the stop side effects.
func TestSubagentLedger_HandbackLeavesAwaitingAlone(t *testing.T) {
	apply := ledgerWorld(t, "hb-await-sess", "conv-hb-await", nil)

	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-w", AgentType: "Explore"})
	apply(HookCallbackInput{HookEventName: "PermissionRequest", ToolName: "Bash"})
	require.Equal(t, StatusAwaitingPermission, loadState(t, "hb-await-sess").Status)

	apply(HookCallbackInput{HookEventName: "PostToolUse", ToolName: "SubagentHandback", AgentID: "ag-w", AgentType: "Explore"})
	got := loadState(t, "hb-await-sess")
	assert.Equal(t, StatusAwaitingPermission, got.Status, "the parent is still waiting on the human")
	assert.Equal(t, 0, got.SubagentCount)
}

// A background sub-agent acting while the MAIN thread's own permission
// prompt is pending proves nothing about that prompt. Taking it as the
// prompt being answered moved the parent to main_agent_idle, and the
// sub-agent's later stop then settled a parent still waiting on the human
// to idle, firing the stop side effects.
func TestSubagentLedger_SubagentHookLeavesMainThreadPromptAlone(t *testing.T) {
	for _, end := range []HookCallbackInput{
		{HookEventName: "SubagentStop", AgentID: "ag-bg", AgentType: "Explore"},
		{HookEventName: "PostToolUse", ToolName: "SubagentHandback", AgentID: "ag-bg", AgentType: "Explore"},
	} {
		t.Run(end.HookEventName, func(t *testing.T) {
			apply := ledgerWorld(t, "main-prompt-sess", "conv-main-prompt", nil)

			apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-bg", AgentType: "Explore"})
			apply(HookCallbackInput{HookEventName: "PermissionRequest", ToolName: "Bash"})
			apply(HookCallbackInput{HookEventName: "Notification", NotificationType: "permission_prompt", Message: "Bash"})
			require.Equal(t, StatusAwaitingPermission, loadState(t, "main-prompt-sess").Status)

			apply(HookCallbackInput{HookEventName: "PreToolUse", ToolName: "SubagentHandback", AgentID: "ag-bg", AgentType: "Explore"})
			assert.Equal(t, StatusAwaitingPermission, loadState(t, "main-prompt-sess").Status,
				"another agent acting does not answer the main thread's prompt")

			apply(end)
			got := loadState(t, "main-prompt-sess")
			assert.Equal(t, StatusAwaitingPermission, got.Status, "the parent is still waiting on the human")
			assert.Equal(t, 0, got.SubagentCount)
		})
	}
}

// The prompt a sub-agent raised is answered by that sub-agent acting again,
// not by a sibling sub-agent acting.
func TestSubagentLedger_OnlyThePromptingSubagentAnswersIt(t *testing.T) {
	apply := ledgerWorld(t, "sibling-sess", "conv-sibling", nil)

	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-a"})
	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-b"})
	apply(HookCallbackInput{HookEventName: "Stop"})
	apply(HookCallbackInput{HookEventName: "PermissionRequest", ToolName: "Bash", AgentID: "ag-a"})
	// Claude Code pairs the request with a Notification; one without an
	// agent_id must not erase who raised the prompt.
	apply(HookCallbackInput{HookEventName: "Notification", NotificationType: "permission_prompt", Message: "Bash"})
	require.Equal(t, StatusAwaitingPermission, loadState(t, "sibling-sess").Status)

	apply(HookCallbackInput{HookEventName: "PostToolUse", ToolName: "Read", AgentID: "ag-b"})
	require.Equal(t, StatusAwaitingPermission, loadState(t, "sibling-sess").Status,
		"a sibling acting does not answer ag-a's prompt")

	apply(HookCallbackInput{HookEventName: "PostToolUse", ToolName: "Bash", AgentID: "ag-a"})
	got := loadState(t, "sibling-sess")
	assert.Equal(t, StatusMainAgentIdle, got.Status, "ag-a acting again answers its own prompt")
	assert.False(t, got.Subagents.HasPrompt("ag-a"), "the answered prompt is forgotten")
}

// A prompt flag left from an earlier episode the main thread moved past must
// not claim a later prompt the main thread raised itself.
func TestSubagentLedger_StalePromptFlagDoesNotClaimALaterPrompt(t *testing.T) {
	apply := ledgerWorld(t, "stale-prompt-sess", "conv-stale-prompt", nil)

	apply(HookCallbackInput{HookEventName: "SubagentStart", AgentID: "ag-s"})
	apply(HookCallbackInput{HookEventName: "PermissionRequest", ToolName: "Bash", AgentID: "ag-s"})
	// Answered, but the main thread's hook lands before ag-s acts again.
	apply(HookCallbackInput{HookEventName: "PreToolUse", ToolName: "Read"})
	apply(HookCallbackInput{HookEventName: "PermissionRequest", ToolName: "Write"})
	require.Equal(t, StatusAwaitingPermission, loadState(t, "stale-prompt-sess").Status)

	apply(HookCallbackInput{HookEventName: "PostToolUse", ToolName: "Bash", AgentID: "ag-s"})
	assert.Equal(t, StatusAwaitingPermission, loadState(t, "stale-prompt-sess").Status,
		"the main thread's own prompt is still pending")
}

// A sub-agent waiting on the human fires no hooks, so a prompt left open
// past db.SubagentTTL must not sweep the entry that says who raised it:
// once answered, that sub-agent's next hook still resolves the prompt.
func TestSubagentLedger_LongPromptKeepsItsAttribution(t *testing.T) {
	old := time.Now().Add(-2 * db.SubagentTTL)
	apply := ledgerWorld(t, "long-prompt-sess", "conv-long-prompt", &SessionState{
		Status:       StatusAwaitingPermission,
		StatusDetail: "Bash",
		Subagents: db.SubagentSet{
			"ag-slow":  {Type: "Explore", Seen: old, Prompt: true},
			"ag-other": {Type: "Plan", Seen: old},
		},
	})

	apply(HookCallbackInput{HookEventName: "PostToolUse", ToolName: "Bash", AgentID: "ag-slow"})
	got := loadState(t, "long-prompt-sess")
	assert.Equal(t, StatusMainAgentIdle, got.Status, "the prompting sub-agent acting answers its prompt")
	assert.Equal(t, 1, got.SubagentCount, "only the prompting entry outlives the TTL")
	assert.Contains(t, got.Subagents, "ag-slow")
}
