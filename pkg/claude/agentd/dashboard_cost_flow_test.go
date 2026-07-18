package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: an agent running on API/enterprise pricing accrues real
// dollar cost — written to the sessions row by the statusline hook
// (UpdateSessionCost, fed from Claude Code's cost.total_cost_usd, only
// when the statusline input carries no subscription rate-limit
// buckets). It must surface on /api/snapshot so the dashboard can
// append a "$0.42" token to the per-agent harness/model line
// ("CC · O4.8 1M high $0.42"). Rides on the same row read as the
// context meter; no new poller, no new data source.
//
// Asserts the cost appears on BOTH the Agents[] roster and the group
// Members[] row (the two places memberRowHTML draws the harness line).
func TestDashboardSnapshot_CostSurfaced(t *testing.T) {
	const conv = "cost-1111-2222-3333-4444"
	const label = "spwn-cost"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-cost", f.TestCwd("cost"))
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
// report a clean zero — harnessLine treats 0 as "no cost data" and
// emits no cost token, so the line looks exactly as before.
func TestDashboardSnapshot_CostZeroWhenNotReported(t *testing.T) {
	const conv = "cosu-1111-2222-3333-4444"
	const label = "spwn-cosu"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveAliveSession(conv, label, "tmux-cosu", f.TestCwd("cosu"))
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
	f.HaveAliveSession(conv, label, "tmux-coss", f.TestCwd("coss"))
	f.HaveEnrolledAgent(conv)

	require.NoError(t, db.UpdateSessionCost(label, 0.42), "UpdateSessionCost")

	// A state-tracking hook fires, re-saving the session row.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: "tmux-coss",
		ConvID:      conv,
		Cwd:         f.TestCwd("coss"),
		Status:      "idle",
	}), "state-update SaveSession")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.Equal(t, 0.42, agentRow.State.CostUSD,
		"cost_usd must survive a state-tracking hook's SaveSession")
}

// Scenario: a SUBSCRIPTION agent — the statusline carries rate-limit
// buckets, so the hook records cost.total_cost_usd as the hypothetical
// pay-per-token-equivalent via UpdateSessionVirtualCost. It must surface on
// /api/snapshot as virtual_cost_usd (the Groups-tab WHAT-IF badge), with the
// real cost_usd left clean at 0 so the two never conflate.
func TestDashboardSnapshot_VirtualCostSurfaced(t *testing.T) {
	const conv = "virt-1111-2222-3333-4444"
	const label = "spwn-virt"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-virt", f.TestCwd("virt"))
	f.HaveMember("squad", conv)

	require.NoError(t, db.UpdateSessionVirtualCost(label, 2.50), "UpdateSessionVirtualCost")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.Equal(t, 2.50, agentRow.State.VirtualCostUSD, "Agents[] virtual_cost_usd")
	assert.Zero(t, agentRow.State.CostUSD, "real cost_usd stays 0 for a subscription agent")

	memberRow := findDashMember(snap, "squad", conv)
	require.NotNil(t, memberRow, "agent %s missing from group squad members", conv)
	assert.Equal(t, 2.50, memberRow.State.VirtualCostUSD, "Members[] virtual_cost_usd")
}

// TestDashboardSnapshot_CostTabVisibilityRule pins the Costs-tab auto-hide /
// WHAT-IF flags the front-end keys off:
//   - no cost at all + no opt-in → hidden (a subscription-only account).
//   - real pay-per-token spend → visible, real mode (not WHAT-IF).
//   - no real spend + cost.show_on_subscription opt-in → visible, WHAT-IF mode.
func TestDashboardSnapshot_CostTabVisibilityRule(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)

	// 1. Fresh, nothing spent, no opt-in → tab hidden.
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.False(t, snap.CostTabVisible, "no cost + no opt-in hides the Costs tab")
	assert.False(t, snap.CostTabWhatIf, "not in WHAT-IF mode when hidden")

	// 2. Opt in on a subscription (no real cost) → visible, WHAT-IF mode.
	require.NoError(t, config.Save(&config.Config{
		Cost: &config.CostConfig{ShowOnSubscription: true},
	}), "save config with show_on_subscription")
	snap = fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.True(t, snap.CostTabVisible, "opt-in shows the Costs tab on a subscription")
	assert.True(t, snap.CostTabWhatIf, "subscription opt-in with no real spend is WHAT-IF mode")

	// 3. Real pay-per-token spend → visible, real mode — even with the opt-in
	//    still on, real spend wins (there's real money to show).
	const label = "spwn-vis"
	f.HaveAliveSession("visi-1111-2222-3333-4444", label, "tmux-vis", f.TestCwd("vis"))
	require.NoError(t, db.UpdateSessionCost(label, 1.00), "UpdateSessionCost")
	snap = fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.True(t, snap.CostTabVisible, "real spend shows the Costs tab")
	assert.False(t, snap.CostTabWhatIf, "real spend is real mode, not WHAT-IF")
}
