//go:build rewire

package agentd_test

import (
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestReincarnate_OfRN_ProducesRNplus1 pins the monotonic-from-prev
// suffix policy: reincarnating a conv titled `worker-r-3` produces
// `worker-r-4` (NOT bare `r-1` and not `worker-r-3-r-1` nesting),
// even when the chronological -r-1/-r-2 ancestors aren't in
// conv_index. This is the bug the strategy doc calls scenario #2.
//
// What the daemon does end-to-end:
//  1. Snapshot identity (groups/perms/ownerships) keyed on the
//     old conv-id.
//  2. Spawn a fresh `tclaude session new` (mocked here; we
//     synthesise the session row + alive flag).
//  3. Migrate identity onto the new conv-id.
//  4. Compute the new title via uniqueReincarnateTitle.
//  5. Fire a goroutine that waits-for-alive then injects
//     `/rename <newTitle>` into the new pane.
//  6. Inject `/exit` into the old pane.
//
// The flow test guards the coordination across all of that — unit
// tests on uniqueReincarnateTitle alone would miss e.g. a regression
// where the daemon computes `worker-r-4` correctly but injects the
// wrong slash command.
func TestReincarnate_OfRN_ProducesRNplus1(t *testing.T) {
	f := newFlow(t)

	const oldConv = "old-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-old-001"
	const oldTmux = "tclaude-spwn-old-001"

	// Given: an old worker titled "worker-r-3" with a live tmux
	// pane and one group membership. We seed identity so we can
	// later assert it migrated.
	f.HaveConvWithTitle(oldConv, "worker-r-3")
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv, "worker")

	// When: a human reincarnates the old worker (no follow-up).
	r := f.AsHuman().Reincarnate(oldConv, "")

	// Then: the new title is `worker-r-4` (monotonic-from-prev),
	// not `r-1` (smallest-free) and not nested.
	f.AssertReincarnateTitle(r, "worker-r-4")

	// Then: the post-spawn goroutine injected `/rename worker-r-4`
	// into the new pane.
	f.AssertSentContains(r.TmuxTarget(), "/rename worker-r-4", 5*time.Second)

	// Then: the old pane received `/exit` (synchronous in the
	// handler — already happened before the response returned).
	if !f.World.Tmux.WaitForSendKeys(oldTmux+":0.0", "/exit", 1*time.Second) {
		t.Errorf("old pane should have received /exit; sent=%+v", f.World.Tmux.Sent())
	}

	// Then: identity migrated from old to new. Group membership
	// moved (old removed, new added under the same alias/role).
	oldMember, _ := db.FindMemberInGroup(g.ID, oldConv)
	if oldMember != nil {
		t.Errorf("old conv still a member of %s: %+v", g.Name, oldMember)
	}
	newMember, _ := db.FindMemberInGroup(g.ID, r.NewConv)
	if newMember == nil {
		t.Errorf("new conv was not added to %s", g.Name)
	} else if newMember.Alias != "worker" {
		t.Errorf("new member alias = %q, want %q (migration should preserve)", newMember.Alias, "worker")
	}
}
