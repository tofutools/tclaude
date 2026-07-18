package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Scenario: an agent spawns a BACKGROUND sub-agent and then its own main
// turn ends while that sub-agent is still running. Claude Code fires the
// hooks in this order:
//
//	SubagentStart   (count 0 → 1)
//	Stop            (main turn ends; count ≥ 1 ⇒ status main_agent_idle)
//	SubagentStop    (count 1 → 0; status falls back to idle)
//
// The dashboard's "+n" badge (groups-member-table.js — ActivityBadges) is driven purely
// by state.subagent_count and is NOT gated on the agent being "busy". The
// concern this pins: after the parent's Stop, the snapshot must still
// report subagent_count=1 (so "+1" shows) and a NON-idle status
// (main_agent_idle, styled busy) — i.e. an idle-looking parent with live
// background work is visibly flagged, not silently blank. Then once the
// sub-agent stops, the count clears and the status settles to plain idle.
//
// Asserts on the group Members[] row — the surface memberRowHTML draws
// the "+n" from.
func TestDashboardSnapshot_SubagentCountSurvivesMainAgentStop(t *testing.T) {
	const conv = "suba-1111-2222-3333-4444"
	const label = "spwn-suba"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-suba", f.TestCwd("suba"))
	f.HaveMember("squad", conv)

	// agentID mirrors the real payloads: the Subagent* events carry the
	// sub-agent's agent_id; the PARENT's own Stop is a main-thread hook
	// and carries none (agent_id is the documented discriminator for
	// "fired from inside a sub-agent" — a main-thread event stamped with
	// one would be swallowed by the sub-agent status gate).
	apply := func(event, agentID string) {
		t.Helper()
		in := session.HookCallbackInput{
			HookEventName: event,
			ConvID:        conv,
			Cwd:           f.TestCwd("suba"),
			AgentID:       agentID,
		}
		if agentID != "" {
			in.AgentType = "Explore"
		}
		require.NoError(t, session.ApplyHook(in, label), "ApplyHook(%s)", event)
	}

	// 1) A background sub-agent starts.
	apply("SubagentStart", "ag-1")
	member := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
	require.NotNil(t, member, "agent %s missing from group squad members", conv)
	assert.Equal(t, 1, member.State.SubagentCount, "subagent_count after SubagentStart")

	// 2) The PARENT's main turn ends while the sub-agent is still running.
	//    This is the crux: an idle-looking parent must still report the
	//    live sub-agent so the "+1" badge renders.
	apply("Stop", "")
	member = findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
	require.NotNil(t, member, "agent %s missing after Stop", conv)
	assert.Equal(t, 1, member.State.SubagentCount, "subagent_count must survive the parent's Stop")
	assert.Equal(t, session.StatusMainAgentIdle, member.State.Status,
		"a parent that stopped with a live sub-agent is main_agent_idle, not idle")

	// 3) The sub-agent finishes — count clears, status settles to idle.
	apply("SubagentStop", "ag-1")
	member = findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
	require.NotNil(t, member, "agent %s missing after SubagentStop", conv)
	assert.Equal(t, 0, member.State.SubagentCount, "subagent_count cleared after SubagentStop")
	assert.Equal(t, session.StatusIdle, member.State.Status, "status settles to idle once no sub-agents remain")
}

// Scenario: the SubagentStop is LOST — the documented interrupt case
// (Claude Code fires no hooks at all on Esc, anthropics/claude-code#11189)
// or any dropped hook callback. The badge must not show a phantom "+1"
// forever: a main-thread SessionStart (the process restarting / resuming)
// is a known-zero boundary and clears the sub-agent ledger.
func TestDashboardSnapshot_SubagentPhantomClearedOnSessionStart(t *testing.T) {
	const conv = "subb-1111-2222-3333-4444"
	const label = "spwn-subb"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-subb", f.TestCwd("subb"))
	f.HaveMember("squad", conv)

	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "SubagentStart",
		ConvID:        conv,
		Cwd:           f.TestCwd("subb"),
		AgentType:     "Explore",
		AgentID:       "ag-doomed",
	}, label), "ApplyHook(SubagentStart)")
	member := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
	require.NotNil(t, member, "agent %s missing from group squad members", conv)
	assert.Equal(t, 1, member.State.SubagentCount, "sub-agent running")

	// The user interrupts (no SubagentStop ever fires) and later restarts
	// the process: SessionStart arrives for the same conv.
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "SessionStart",
		Source:        "startup",
		ConvID:        conv,
		Cwd:           f.TestCwd("subb"),
	}, label), "ApplyHook(SessionStart)")
	member = findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
	require.NotNil(t, member, "agent %s missing after SessionStart", conv)
	assert.Equal(t, 0, member.State.SubagentCount,
		"a (re)starting process has no sub-agents — the phantom must not survive SessionStart")
}

// Scenario: the whole session dies while its sub-agent count is non-zero
// (kill -9, crash — no SessionEnd, no SubagentStop). Sub-agents run
// INSIDE the harness process, so an offline agent's snapshot must report
// zero regardless of what the stale row says.
func TestDashboardSnapshot_OfflineAgentReportsZeroSubagents(t *testing.T) {
	const conv = "subc-1111-2222-3333-4444"
	const label = "spwn-subc"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-subc", f.TestCwd("subc"))
	f.HaveMember("squad", conv)

	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "SubagentStart",
		ConvID:        conv,
		Cwd:           f.TestCwd("subc"),
		AgentType:     "Explore",
		AgentID:       "ag-orphan",
	}, label), "ApplyHook(SubagentStart)")
	member := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
	require.NotNil(t, member, "agent %s missing from group squad members", conv)
	assert.Equal(t, 1, member.State.SubagentCount, "sub-agent running while alive")

	// The process dies without any farewell hooks.
	f.MarkOffline("tmux-subc")
	member = findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
	require.NotNil(t, member, "agent %s missing after going offline", conv)
	assert.Equal(t, 0, member.State.SubagentCount,
		"a dead process has no sub-agents — the stale row count must not surface")
}

// Scenario: the SubagentStop was lost and NO further hook ever fires —
// the ledger entry expires via the TTL, which the badge respects
// (LiveCount) but no write path re-settles the stored status. The
// snapshot must not self-contradict (badge 0 next to a busy
// "N subagents running"): the read side settles the DISPLAY to idle.
func TestDashboardSnapshot_ExpiredLedgerSettlesStatusToIdle(t *testing.T) {
	const conv = "subd-1111-2222-3333-4444"
	const label = "spwn-subd"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-subd", f.TestCwd("subd"))
	f.HaveMember("squad", conv)

	// Stage the wedged row directly: main_agent_idle with a ledger whose
	// only entry expired > TTL ago (a hook can't produce this state in a
	// test without waiting out the TTL — expiry-without-hooks is the
	// point of the scenario).
	row, err := db.LoadSession(label)
	require.NoError(t, err)
	row.Status = session.StatusMainAgentIdle
	row.StatusDetail = "1 subagents running"
	row.SubagentCount = 1
	row.SubagentsJSON = db.SubagentSet{
		"ag-expired": {Type: "Explore", Seen: time.Now().Add(-db.SubagentTTL - time.Minute)},
	}.Encode()
	require.NoError(t, db.SaveSession(row))

	member := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
	require.NotNil(t, member, "agent %s missing from group squad members", conv)
	assert.Equal(t, 0, member.State.SubagentCount, "expired ledger entry must not count")
	assert.Equal(t, session.StatusIdle, member.State.Status,
		"status display settles to idle when the TTL-filtered count is zero")
	assert.Empty(t, member.State.StatusDetail, "no 'N subagents running' next to a zero badge")
}

// Scenario: Codex records a collaboration child as interrupted in its rollout
// but does not deliver the configured SubagentStop hook. The shared hook ledger
// is therefore still fresh and says one child is running; waiting for its
// 15-minute TTL made the dashboard visibly wrong. The rollout is authoritative
// for Codex and must settle the dashboard immediately.
func TestDashboardSnapshot_CodexInterruptedSubagentOverridesFreshHookLedger(t *testing.T) {
	const conv = "019ec004-4250-79b1-9ade-ebaea4170191"
	const label = "spwn-codex-subagent"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	agentd.ResetCodexContextRefreshForTest()

	f := newFlow(t)
	f.HaveGroup("codex-squad")
	cx := f.HaveAliveCodexSession(conv, label, "tmux-codex-subagent", f.TestCwd("codex-subagent"))
	f.HaveMember("codex-squad", conv)
	require.NoError(t, cx.WriteSubagentActivity("child-review", "/root/reviewer", "started"))
	require.NoError(t, cx.WriteSubagentActivity("child-review", "/root/reviewer", "interrupted"))

	// Model the missing hook: SQLite still carries a fresh, non-expired child
	// and the parent status produced by its main-thread Stop event.
	row, err := db.LoadSession(label)
	require.NoError(t, err)
	row.Status = session.StatusMainAgentIdle
	row.StatusDetail = "1 subagents running"
	row.SubagentCount = 1
	row.SubagentsJSON = db.SubagentSet{
		"child-review": {Type: "reviewer", Seen: time.Now()},
	}.Encode()
	require.NoError(t, db.SaveSession(row))

	member := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "codex-squad", conv)
	require.NotNil(t, member)
	assert.Zero(t, member.State.SubagentCount,
		"Codex rollout interrupted event overrides the stale-but-fresh hook ledger")
	assert.Equal(t, session.StatusIdle, member.State.Status,
		"known-zero Codex activity settles main_agent_idle immediately")
	assert.Empty(t, member.State.StatusDetail)
}
