package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// findDashAgent returns the snapshot's Agents[] row for a conv-id.
func findDashAgent(snap dashSnapshot, convID string) *dashAgent {
	for i := range snap.Agents {
		if snap.Agents[i].ConvID == convID {
			return &snap.Agents[i]
		}
	}
	return nil
}

// findDashMember returns the group-member row for a conv-id within a
// named group.
func findDashMember(snap dashSnapshot, group, convID string) *dashMember {
	for gi := range snap.Groups {
		if snap.Groups[gi].Name != group {
			continue
		}
		for mi := range snap.Groups[gi].Members {
			if snap.Groups[gi].Members[mi].ConvID == convID {
				return &snap.Groups[gi].Members[mi]
			}
		}
	}
	return nil
}

// Scenario: an agent's context-window usage — written to the sessions
// row by the statusline hook (UpdateContextSnapshot) — must surface on
// /api/snapshot so the dashboard's vertical context-meter can render
// it. The meter rides entirely on this already-persisted data; no new
// poller, no new data source.
//
// Asserts the snapshot carries context_pct + the absolute token counts
// for the agent BOTH on the Agents[] roster and on the group Members[]
// row (the two places memberRowHTML / the agents table draw the
// meter). A known 60% value is the JS contract: with 5 segments at 20%
// each, ceil(60/20)=3 segments light bottom-up (2 green + 1 yellow).
func TestDashboardSnapshot_ContextMeterUsageSurfaced(t *testing.T) {
	const conv = "ctxm-1111-2222-3333-4444"
	const label = "spwn-ctxm"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-ctxm", "/tmp/ctxm")
	f.HaveMember("squad", conv)

	// The statusline hook's write path: context_pct + abs token counts
	// land on the sessions row keyed by tclaude session ID (the label).
	require.NoError(t,
		db.UpdateContextSnapshot(label, 60.0, 110000, 10000, 200000),
		"UpdateContextSnapshot")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.Equal(t, 60.0, agentRow.State.ContextPct, "Agents[] context_pct")
	assert.Equal(t, int64(110000), agentRow.State.TokensInput, "Agents[] tokens_input")
	assert.Equal(t, int64(10000), agentRow.State.TokensOutput, "Agents[] tokens_output")
	assert.Equal(t, int64(200000), agentRow.State.ContextWindowSize, "Agents[] context_window_size")

	memberRow := findDashMember(snap, "squad", conv)
	require.NotNil(t, memberRow, "agent %s missing from group squad members", conv)
	assert.Equal(t, 60.0, memberRow.State.ContextPct, "Members[] context_pct")
	assert.Equal(t, int64(200000), memberRow.State.ContextWindowSize, "Members[] context_window_size")
}

// Scenario: a freshly-spawned agent whose statusline hook has not yet
// fired has no context snapshot row. The dashboard meter must render a
// neutral / empty indicator, never a broken one — so /api/snapshot
// must report a clean zero (context_pct == 0, no token counts) rather
// than garbage. The JS treats this all-zero state as "unknown" and
// dims every segment.
func TestDashboardSnapshot_ContextMeterUnknownWhenNoUsage(t *testing.T) {
	const conv = "ctxu-1111-2222-3333-4444"
	const label = "spwn-ctxu"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveAliveSession(conv, label, "tmux-ctxu", "/tmp/ctxu")
	f.HaveEnrolledAgent(conv)

	// No UpdateContextSnapshot call — the statusline hook never fired.

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.Zero(t, agentRow.State.ContextPct, "no-usage agent should report 0 context_pct")
	assert.Zero(t, agentRow.State.TokensInput, "no-usage agent should report 0 tokens_input")
	assert.Zero(t, agentRow.State.TokensOutput, "no-usage agent should report 0 tokens_output")
	assert.Zero(t, agentRow.State.ContextWindowSize, "no-usage agent should report 0 window size")
}
