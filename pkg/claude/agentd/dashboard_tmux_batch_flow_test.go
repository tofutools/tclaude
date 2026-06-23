package agentd_test

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// Scenario: the dashboard snapshot used to fire one `tmux has-session`
// subprocess per session row across every recent conv, group member,
// group owner and retired agent — ~150 spawns per 5-second poll. This
// test pins the bulk-fetch optimisation: ONE `tmux list-sessions` for
// the whole snapshot, then per-row liveness via map lookup. A
// regression that reintroduces per-row probing fails the has-session
// count assertion immediately.
//
// Correctness is asserted alongside the call counts: with a mix of
// alive and killed sessions in a multi-member group, every member's
// Online flag in the snapshot must match its actual tmux liveness —
// otherwise the bulk path has diverged from the per-row semantics.
func TestDashboardSnapshot_OneTmuxListZeroHasSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
		t.Cleanup(restoreURL)

		f := newFlow(t)

		// Four members — enough that the old per-row path would fire
		// multiple has-session calls (so a count of zero is a meaningful
		// assertion, not a vacuous one). Mix of alive/offline so both
		// branches of the alive-set lookup are exercised.
		const (
			c1 = "snp1-aaaa-bbbb-cccc-dddddddddddd" // alive
			c2 = "snp2-aaaa-bbbb-cccc-dddddddddddd" // alive
			c3 = "snp3-aaaa-bbbb-cccc-dddddddddddd" // offline (tmux killed)
			c4 = "snp4-aaaa-bbbb-cccc-dddddddddddd" // offline (tmux killed)
		)
		f.HaveConvWithTitle(c1, "worker-1")
		f.HaveConvWithTitle(c2, "worker-2")
		f.HaveConvWithTitle(c3, "worker-3")
		f.HaveConvWithTitle(c4, "worker-4")
		f.HaveAliveSession(c1, "spwn-1", "tmux-1", "/tmp/c1")
		f.HaveAliveSession(c2, "spwn-2", "tmux-2", "/tmp/c2")
		f.HaveAliveSession(c3, "spwn-3", "tmux-3", "/tmp/c3")
		f.HaveAliveSession(c4, "spwn-4", "tmux-4", "/tmp/c4")

		f.HaveGroup("crew")
		f.HaveMember("crew", c1)
		f.HaveMember("crew", c2)
		f.HaveMember("crew", c3)
		f.HaveMember("crew", c4)

		// Flip the tmux side of c3 / c4 offline. HaveAliveSession registered
		// them; MarkOffline drops them from the sim's alive table.
		f.MarkOffline("tmux-3")
		f.MarkOffline("tmux-4")

		// Snapshot the call counts before the dashboard request — group/
		// member setup may have nudged tmux through unrelated paths and we
		// want to isolate ONLY what the snapshot itself fires.
		lsBefore := f.World.Tmux.CommandCount("list-sessions")
		hsBefore := f.World.Tmux.CommandCount("has-session")

		snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

		lsAfter := f.World.Tmux.CommandCount("list-sessions")
		hsAfter := f.World.Tmux.CommandCount("has-session")

		// The hot invariant: ONE list-sessions for the whole snapshot, and
		// ZERO per-row has-session probes. A future regression that
		// reintroduces per-row tmux probes in the snapshot path will fail
		// the has-session assertion long before it becomes a perf incident.
		assert.Equal(t, 1, lsAfter-lsBefore,
			"dashboard snapshot must fire exactly ONE list-sessions call")
		assert.Equal(t, 0, hsAfter-hsBefore,
			"dashboard snapshot must fire ZERO has-session subprocess probes")

		// Correctness: per-member Online must still match reality across the
		// alive/offline mix. Bulk-fetch path diverging from per-row
		// semantics would surface here.
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
		m1 := memberOf(c1)
		require.NotNil(t, m1, "c1 must appear in crew")
		assert.True(t, m1.Online, "c1 has a live tmux session → online")
		m2 := memberOf(c2)
		require.NotNil(t, m2, "c2 must appear in crew")
		assert.True(t, m2.Online, "c2 has a live tmux session → online")
		m3 := memberOf(c3)
		require.NotNil(t, m3, "c3 must appear in crew")
		assert.False(t, m3.Online, "c3 tmux is offline → not online")
		m4 := memberOf(c4)
		require.NotNil(t, m4, "c4 must appear in crew")
		assert.False(t, m4.Online, "c4 tmux is offline → not online")
	})
}
