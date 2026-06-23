package agentd_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// dashGroupMemberTitle pulls one member's title out of a dashboard
// snapshot — the value `/api/snapshot` puts on the group-member row,
// i.e. exactly what the dashboard renders. Fatals if the member is
// absent so a regression points at the missing row, not a bare "".
func dashGroupMemberTitle(t *testing.T, snap dashSnapshot, group, convID string) string {
	t.Helper()
	for _, g := range snap.Groups {
		if g.Name != group {
			continue
		}
		for _, m := range g.Members {
			if m.ConvID == convID {
				return m.Title
			}
		}
	}
	t.Fatalf("conv %s not found in dashboard group %q; snapshot=%+v", convID, group, snap)
	return ""
}

// Scenario: `tclaude agent spawn alpha --name pending-reviewer`.
//
// A freshly-spawned agent has no conversation title until the daemon's
// post-spawn `/rename` injection lands — a couple of seconds later. In
// that gap the dashboard used to show "(unknown)". The spawn now records
// the `--name` value as the agent's pending name (agent_enrollment.
// pending_name), and agent.FreshTitle uses it as a display fallback, so
// the intended name shows immediately.
//
// This pins the two guarantees:
//   - Before any `/rename`, the pending name surfaces on both the group-
//     members view and the dashboard snapshot — not "(unknown)", not the
//     conversation's summary or welcome line.
//   - A later self-rename supersedes it cleanly: once a custom title
//     exists, FreshTitle resolves through conv_index and the pending name
//     is never consulted again — no flicker back to the pending value.
//
// Determinism: the spawn's own `/rename` injection is blocked with an
// hour-long command delay, so the conversation provably has NO custom
// title for the whole pending-name window. The self-rename later uses a
// distinct name so superseding is observable rather than a same-string
// no-op.
func TestSpawn_PendingNameShownThenSupersededBySelfRename(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		// fetchDashSnapshot hits /api/snapshot, which checkDashboardAuth
		// gates on a popup-base-pinned Origin; the test handler only injects
		// that header when a base URL is set.
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

		f.HaveGroup("alpha")

		spawn := f.AsHuman().Spawn("alpha", "pending-reviewer")

		// Block the spawn's own /rename injection so the conversation never
		// gets a custom-title turn — the deterministic stand-in for "agent
		// spawned, has not renamed itself yet".
		cc := f.World.CCs.GetByConvID(spawn.ConvID)
		require.NotNil(t, cc, "spawned CCSim should be registered")
		cc.SetCommandDelay("/rename ", time.Hour)

		// Let the post-spawn injection settle. The welcome [system: …] turn
		// lands as a user turn, so conv_index now carries a real first prompt
		// (and the simulator's startup summary) — proving the pending name
		// outranks BOTH, not merely an otherwise-empty row.
		agentd.WaitForBackgroundForTest()

		// Members surface — what `tclaude agent groups members alpha` renders.
		// The intended name shows, sourced from agent_enrollment.pending_name.
		f.AssertGroupMember("alpha", spawn.ConvID, "pending-reviewer", 3*time.Second)

		// Dashboard snapshot — the surface the human actually watches. No
		// "(unknown)" placeholder.
		snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
		require.Equal(t, "pending-reviewer",
			dashGroupMemberTitle(t, snap, "alpha", spawn.ConvID),
			"dashboard should show the pending name before the agent renames itself")

		// The agent renames itself to a different name. The spawn's blocked
		// /rename goroutine is still asleep; clearing the delay lets this
		// fresh keystroke dispatch immediately.
		cc.SetCommandDelay("/rename ", 0)
		cc.Receive("/rename real-reviewer")
		cc.Receive("Enter")

		// Self-rename supersedes the pending name cleanly: a custom title now
		// exists, so FreshTitle resolves through conv_index from here on.
		f.AssertGroupMember("alpha", spawn.ConvID, "real-reviewer", 5*time.Second)
	})
}
