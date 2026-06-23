package agentd_test

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: four agents — one that exited cleanly (a graceful SessionEnd
// recorded a reason), one that died unexpectedly (the reaper stamped
// 'unexpected'), one that exited before the exit_reason column existed
// (NULL), and one still live. The dashboard snapshot must carry
// exit_reason through stateForConv so the UI can render the crashed one
// distinctly while the clean and the legacy ones stay plain offline.
//
// Pins: an unexpected death is surfaced as exit_reason='unexpected'; a
// clean exit's reason flows through; a NULL (pre-migration) reason
// reports as "" — never retroactively a crash; a live agent carries no
// exit_reason at all.
func TestDashboardSnapshot_ExitReasonSurfacesCrashedVsClean(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
		t.Cleanup(restoreURL)

		f := newFlow(t)

		const cleanConv = "cln0-aaaa-bbbb-cccc-dddddddddddd"
		const crashConv = "crsh-aaaa-bbbb-cccc-dddddddddddd"
		const legacyConv = "lgcy-aaaa-bbbb-cccc-dddddddddddd"
		const liveConv = "live-aaaa-bbbb-cccc-dddddddddddd"
		f.HaveConvWithTitle(cleanConv, "clean-exit-worker")
		f.HaveConvWithTitle(crashConv, "crashed-worker")
		f.HaveConvWithTitle(legacyConv, "legacy-worker")
		f.HaveConvWithTitle(liveConv, "live-worker")
		f.HaveAliveSession(cleanConv, "spwn-cln", "tmux-cln", "/tmp/cln")
		f.HaveAliveSession(crashConv, "spwn-crsh", "tmux-crsh", "/tmp/crsh")
		f.HaveAliveSession(legacyConv, "spwn-lgcy", "tmux-lgcy", "/tmp/lgcy")
		f.HaveAliveSession(liveConv, "spwn-live", "tmux-live", "/tmp/live")

		f.HaveGroup("crew")
		f.HaveMember("crew", cleanConv)
		f.HaveMember("crew", crashConv)
		f.HaveMember("crew", legacyConv)
		f.HaveMember("crew", liveConv)

		// The clean exit: a graceful SessionEnd recorded its reason.
		require.NoError(t, db.SetSessionExitReason("spwn-cln", "logout"))
		// The unexpected death: the reaper stamped 'unexpected'.
		require.NoError(t, db.SetSessionExitReason("spwn-crsh", "unexpected"))
		// The legacy agent records nothing — its exit_reason stays NULL.

		// The three exited agents go offline; the live one stays up.
		f.MarkOffline("tmux-cln")
		f.MarkOffline("tmux-crsh")
		f.MarkOffline("tmux-lgcy")

		snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

		memberOf := func(conv string) *dashMember {
			for _, g := range snap.Groups {
				for i := range g.Members {
					if g.Members[i].ConvID == conv {
						return &g.Members[i]
					}
				}
			}
			return nil
		}

		// Clean exit: offline, the SessionEnd reason flows through — the
		// dashboard renders this as a plain "offline".
		cln := memberOf(cleanConv)
		require.NotNil(t, cln, "clean-exit conv should be a crew member")
		assert.False(t, cln.Online, "a cleanly-exited agent is offline")
		assert.Equal(t, "logout", cln.State.ExitReason,
			"a clean exit's recorded reason must reach the snapshot")

		// Unexpected death: offline, exit_reason='unexpected' — the
		// dashboard renders this distinctly as "crashed".
		crsh := memberOf(crashConv)
		require.NotNil(t, crsh, "crashed conv should be a crew member")
		assert.False(t, crsh.Online, "a crashed agent is offline")
		assert.Equal(t, "unexpected", crsh.State.ExitReason,
			"an unexpected death must surface as exit_reason=unexpected")

		// Legacy / pre-migration: exit_reason NULL → "". Must NOT be a crash.
		lgcy := memberOf(legacyConv)
		require.NotNil(t, lgcy, "legacy conv should be a crew member")
		assert.False(t, lgcy.Online, "the legacy agent is offline")
		assert.Equal(t, "", lgcy.State.ExitReason,
			"a NULL (pre-migration) exit_reason surfaces as empty, never a crash")

		// A live agent never carries an exit_reason — it has not exited.
		live := memberOf(liveConv)
		require.NotNil(t, live, "live conv should be a crew member")
		assert.True(t, live.Online, "the live agent is online")
		assert.Equal(t, "", live.State.ExitReason,
			"a live agent must not carry an exit_reason")
	})
}

// End-to-end: a default Claude Code agent's pane dies with no
// SessionEnd hook; a reaper sweep stamps exit_reason='unexpected'; the
// dashboard snapshot then surfaces it as a crash. The reaper→db and
// db→dashboard halves are each covered separately — this pins the seam
// between them, which a regression in stateForConv's row selection
// could slip past.
func TestDashboardSnapshot_ReapedAgentSurfacesAsCrashed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
		t.Cleanup(restoreURL)

		f := newFlow(t)
		const conv = "reap-aaaa-bbbb-cccc-dddddddddddd"
		f.HaveConvWithTitle(conv, "reaped-worker")
		f.HaveAliveSession(conv, "spwn-reap", "tmux-reap", "/tmp/reap")
		f.HaveGroup("crew")
		f.HaveMember("crew", conv)

		// The Claude Code pane dies with no SessionEnd; a reaper sweep marks
		// it exited and — finding no recorded reason — stamps it
		// 'unexpected'.
		f.MarkOffline("tmux-reap")
		reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
		require.Equal(t, 1, reaper.Tick(), "the dead session is reaped")

		snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
		var member *dashMember
		for _, g := range snap.Groups {
			for i := range g.Members {
				if g.Members[i].ConvID == conv {
					member = &g.Members[i]
				}
			}
		}
		require.NotNil(t, member, "reaped conv should still be a crew member")
		assert.False(t, member.Online, "a reaped agent is offline")
		assert.Equal(t, "unexpected", member.State.ExitReason,
			"a reaper-stamped death must surface on the dashboard as a crash")
	})
}
