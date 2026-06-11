package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: an agent running on API/enterprise pricing accrues real
// dollar cost — written to the sessions row by the statusline hook
// (UpdateSessionCost, fed from Claude Code's cost.total_cost_usd, only
// when the statusline input carries no subscription rate-limit
// buckets). It must surface on /api/snapshot so the dashboard's status
// column can show a "$0.42" badge next to the state pill. Rides on the
// same row read as the context meter; no new poller, no new data
// source.
//
// Asserts the cost appears on BOTH the Agents[] roster and the group
// Members[] row (the two places memberRowHTML draws the status column).
func TestDashboardSnapshot_CostSurfaced(t *testing.T) {
	const conv = "cost-1111-2222-3333-4444"
	const label = "spwn-cost"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-cost", "/tmp/cost")
	f.HaveMember("squad", conv)

	// The statusline hook's write path: cost lands on the sessions row
	// keyed by tclaude session ID (the label).
	require.NoError(t, db.UpdateSessionCost(label, 1.37), "UpdateSessionCost")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.Equal(t, 1.37, agentRow.State.CostUSD, "Agents[] cost_usd")

	memberRow := findDashMember(snap, "squad", conv)
	require.NotNil(t, memberRow, "agent %s missing from group squad members", conv)
	assert.Equal(t, 1.37, memberRow.State.CostUSD, "Members[] cost_usd")
}

// Scenario: a subscription-plan agent (rate-limit buckets present in
// every statusline render) never gets an UpdateSessionCost write, as
// does a fresh agent whose statusbar hasn't ticked. /api/snapshot must
// report a clean zero — the JS costBadge treats 0 as "no cost data"
// and renders nothing, so the status column looks exactly as before.
func TestDashboardSnapshot_CostZeroWhenNotReported(t *testing.T) {
	const conv = "cosu-1111-2222-3333-4444"
	const label = "spwn-cosu"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveAliveSession(conv, label, "tmux-cosu", "/tmp/cosu")
	f.HaveEnrolledAgent(conv)

	// No UpdateSessionCost call — subscription plan, or no tick yet.

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.Zero(t, agentRow.State.CostUSD, "no-cost agent should report 0 cost_usd")
}

// Regression guard: a state-tracking hook's SaveSession (Stop /
// UserPromptSubmit, fired on every tick) must not wipe a recorded
// cost — the same INSERT OR REPLACE hazard that once zeroed the
// context-meter columns. cost_usd is out-of-band: owned by the
// statusline hook, untouched by SaveSession's UPSERT column list.
func TestDashboardSnapshot_CostSurvivesStateHookWrite(t *testing.T) {
	const conv = "coss-1111-2222-3333-4444"
	const label = "spwn-coss"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveAliveSession(conv, label, "tmux-coss", "/tmp/coss")
	f.HaveEnrolledAgent(conv)

	require.NoError(t, db.UpdateSessionCost(label, 0.42), "UpdateSessionCost")

	// A state-tracking hook fires, re-saving the session row.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: "tmux-coss",
		ConvID:      conv,
		Cwd:         "/tmp/coss",
		Status:      "idle",
	}), "state-update SaveSession")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.Equal(t, 0.42, agentRow.State.CostUSD,
		"cost_usd must survive a state-tracking hook's SaveSession")
}
