package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
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
// The dashboard's "+n" badge (render.js — memberRowHTML) is driven purely
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
	f.HaveAliveSession(conv, label, "tmux-suba", "/tmp/suba")
	f.HaveMember("squad", conv)

	apply := func(event string) {
		t.Helper()
		require.NoError(t, session.ApplyHook(session.HookCallbackInput{
			HookEventName: event,
			ConvID:        conv,
			Cwd:           "/tmp/suba",
			AgentType:     "Explore",
			AgentID:       "ag-1",
		}, label), "ApplyHook(%s)", event)
	}

	// 1) A background sub-agent starts.
	apply("SubagentStart")
	member := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
	require.NotNil(t, member, "agent %s missing from group squad members", conv)
	assert.Equal(t, 1, member.State.SubagentCount, "subagent_count after SubagentStart")

	// 2) The PARENT's main turn ends while the sub-agent is still running.
	//    This is the crux: an idle-looking parent must still report the
	//    live sub-agent so the "+1" badge renders.
	apply("Stop")
	member = findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
	require.NotNil(t, member, "agent %s missing after Stop", conv)
	assert.Equal(t, 1, member.State.SubagentCount, "subagent_count must survive the parent's Stop")
	assert.Equal(t, session.StatusMainAgentIdle, member.State.Status,
		"a parent that stopped with a live sub-agent is main_agent_idle, not idle")

	// 3) The sub-agent finishes — count clears, status settles to idle.
	apply("SubagentStop")
	member = findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "squad", conv)
	require.NotNil(t, member, "agent %s missing after SubagentStop", conv)
	assert.Equal(t, 0, member.State.SubagentCount, "subagent_count cleared after SubagentStop")
	assert.Equal(t, session.StatusIdle, member.State.Status, "status settles to idle once no sub-agents remain")
}
