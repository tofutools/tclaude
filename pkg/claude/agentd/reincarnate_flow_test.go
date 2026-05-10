//go:build rewire

package agentd_test

import (
	"testing"
	"time"
)

// Scenario: a worker on its third reincarnation gets reincarnated
// again.
//
// Setup: a worker is running under the title "worker-r-3" with a
// live tmux pane. It belongs to group "alpha" with alias "worker".
// (Reincarnation replaces a CC instance with a fresh successor that
// inherits the same identity but starts with a clean conversation.)
//
// Action: the human reincarnates the worker.
//
// Expected:
//   - The new instance is titled "worker-r-4" — the suffix
//     increments from the parent's. Even if intermediate -r-1/-r-2
//     ancestors are no longer in the index, the new suffix is
//     strictly greater than the parent's.
//   - The new pane is renamed to "worker-r-4".
//   - The old pane receives `/exit`.
//   - Group membership moves from old to new under the same alias;
//     the old conv is no longer a member.
func TestReincarnate_OfRN_ProducesRNplus1(t *testing.T) {
	f := newFlow(t)

	const oldConv = "old-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-old-001"
	const oldTmux = "tclaude-spwn-old-001"

	f.HaveConvWithTitle(oldConv, "worker-r-3")
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv, "worker")

	r := f.AsHuman().Reincarnate(oldConv, "")

	f.AssertReincarnateTitle(r, "worker-r-4")
	f.AssertSentContains(r.TmuxTarget(), "/rename worker-r-4", 5*time.Second)

	if !f.World.Tmux.WaitForSendKeys(oldTmux+":0.0", "/exit", 1*time.Second) {
		t.Errorf("old pane should have received /exit; sent=%+v", f.World.Tmux.Sent())
	}

	// Surface-level invariants the human would see post-reincarnate
	// in `tclaude agent groups members alpha`:
	//   - the new conv shows up under the worker alias with the
	//     bumped -r-4 title (catches both the daemon's title-bump
	//     math AND the rename actually landing as a renderable title);
	//   - the old conv is gone (catches the membership migration
	//     that reincarnate has to do atomically).
	f.AssertGroupMember(g.Name, r.NewConv, "worker", "worker-r-4", 5*time.Second)
	f.AssertNotGroupMember(g.Name, oldConv)
}
