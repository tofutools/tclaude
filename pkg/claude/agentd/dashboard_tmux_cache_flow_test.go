package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// Scenario: one dashboard tick fires /api/snapshot, /api/retired and
// /api/conversations in parallel; each needs the live tmux set. Before TCL-370
// each forked its own `tmux ls` (~5-15ms fork+exec). This test pins the
// coalescing win: with the short-TTL cache the three back-to-back poll handlers
// share ONE `tmux list-sessions` instead of firing three.
//
// newFlow neutralizes the cache (TTL 0) so unrelated scenarios keep fresh
// per-fetch liveness; here we opt back into a real TTL to observe the dedup.
func TestDashboardPoll_ThreeHandlersShareOneTmuxProbe(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	// The cache reads real time.Now (only the TTL is overridden here, not the
	// clock), so the "three handlers land within one TTL" invariant is
	// wall-clock-dependent. Use a TTL vastly larger than any plausible test
	// duration — including a severe CI stall or GC pause between the first and
	// third handler — so the second and third calls are guaranteed cache hits
	// and this assertion can never flake. Cleanup (LIFO, before newFlow's)
	// restores the TTL-0 neutralization.
	t.Cleanup(agentd.SetTmuxCacheTTLForTest(time.Hour))

	const conv = "tmxc-aaaa-bbbb-cccc-dddddddddddd"
	f.HaveConvWithTitle(conv, "worker")
	f.HaveAliveSession(conv, "spwn-tmxc", "tmux-tmxc", f.TestCwd("tmxc"))

	mux := agentd.BuildDashboardHandlerForTest()

	lsBefore := f.World.Tmux.CommandCount("list-sessions")

	// The tick's three tmux-probing poll handlers, back to back within the TTL.
	_ = fetchSnapshotOnly(t, mux)
	_ = fetchListRows[dashRetired](t, mux, "/api/retired?limit=0")
	_ = fetchListRows[dashConversation](t, mux, "/api/conversations?limit=0")

	lsAfter := f.World.Tmux.CommandCount("list-sessions")

	assert.Equal(t, 1, lsAfter-lsBefore,
		"snapshot + retired + conversations within the TTL must share ONE tmux list-sessions probe")

	// Sanity: with the cache neutralized (as every other flow scenario runs),
	// the same three handlers each probe — proving the assertion above is the
	// cache's doing, not an unrelated collapse of the probes.
	restore := agentd.SetTmuxCacheTTLForTest(0)
	defer restore()
	lsBefore = f.World.Tmux.CommandCount("list-sessions")
	_ = fetchSnapshotOnly(t, mux)
	_ = fetchListRows[dashRetired](t, mux, "/api/retired?limit=0")
	_ = fetchListRows[dashConversation](t, mux, "/api/conversations?limit=0")
	lsAfter = f.World.Tmux.CommandCount("list-sessions")
	assert.Equal(t, 3, lsAfter-lsBefore,
		"with the cache neutralized each of the three handlers probes tmux independently")
}
