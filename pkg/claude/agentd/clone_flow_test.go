package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Scenario: the human clones a worker that was added to a group
// without a per-group alias.
//
// Setup: a conversation titled "worker" is a member of group
// "alpha". It joined by conv-id, so its agent_group_members row
// has an empty alias. The pane is live.
//
// Action: the human clones "worker" without specifying an alias
// for the new sibling. (Cloning forks the agent into an
// independent copy that inherits identity but keeps running
// alongside the original.)
//
// Expected: the clone's row in "alpha" has alias "worker-c-1".
// The daemon falls back to the conv's display title to derive
// the alias when the original member row had none — without that
// fallback the clone would land as bare "c-1" and be
// indistinguishable from clones of other untitled members in
// tmux/dashboard tiles.
//
// CloneFresh is used (no_copy_conv: true) to skip the .jsonl
// copy path so the test stays off convops.CopyConversationToPath.
func TestClone_EmptyAlias_DerivesFromOriginalTitle(t *testing.T) {
	f := newFlow(t)

	const oldConv = "old-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-old-001"
	const oldTmux = "tclaude-spwn-old-001"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv, "" /* intentionally no alias */)

	c := f.AsHuman().CloneFresh(oldConv, "")

	f.AssertCloneAliasInGroup(c, "alpha", "worker-c-1")

	assert.Equal(t, oldConv, c.OldConv, "OldConv")

	// Surface-level invariants the human would see post-clone in
	// `tclaude agent groups members alpha`:
	//   - both members visible (clone is ADD-only — original isn't
	//     touched);
	//   - the new clone has the computed alias + matching title
	//     "worker-c-1" (catches both the alias fallback and the
	//     post-spawn /rename actually rendering at the surface);
	//   - the original retains its title (no stray /rename on the
	//     source).
	f.AssertGroupMember("alpha", c.NewConv, "worker-c-1", "worker-c-1", 5*time.Second)
	f.AssertGroupMember("alpha", oldConv, "", "worker", 1*time.Second)
}
