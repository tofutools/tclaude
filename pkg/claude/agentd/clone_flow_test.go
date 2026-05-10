//go:build rewire

package agentd_test

import (
	"testing"
)

// TestClone_EmptyAlias_DerivesFromOriginalTitle pins the alias-
// fallback fix from d0cb0e1 (clone fixes — alias fallback +
// post-spawn /rename). When the original conv joined a group with
// NO alias, cloning it should produce `<originalTitle>-c-1` in the
// new member row — NOT bare `c-1`, which is what the pre-fix
// daemon produced and made clones impossible to distinguish in
// tmux/dashboard tiles.
//
// The scenario:
//  - "worker" is in group "alpha" with alias="" (joined by conv-id).
//  - We clone "worker" with no alias (so the daemon must derive one).
//  - The clone's new member row in alpha should have alias
//    "worker-c-1", proving the daemon falls back to the original's
//    display title rather than the original's empty alias.
func TestClone_EmptyAlias_DerivesFromOriginalTitle(t *testing.T) {
	f := newFlow(t)

	const oldConv = "old-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-old-001"
	const oldTmux = "tclaude-spwn-old-001"

	// Given: a "worker" conv with title set, in group alpha with no
	// alias, and a live tmux pane.
	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv, "" /* alias intentionally empty */)

	// When: the human clones with no alias and no jsonl copy
	// (CloneFresh keeps the test off convops.CopyConversationToPath).
	c := f.AsHuman().CloneFresh(oldConv, "")

	// Then: the clone joined alpha with the title-derived alias.
	f.AssertCloneAliasInGroup(c, "alpha", "worker-c-1")

	// And: the original keeps its (empty-alias) seat in the group —
	// clone is ADD-only, in contrast to reincarnate's MOVE.
	if c.OldConv != oldConv {
		t.Errorf("OldConv = %q, want %q", c.OldConv, oldConv)
	}
}
