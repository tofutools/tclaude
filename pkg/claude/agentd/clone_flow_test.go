package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Scenario: the human clones a worker, and the clone's title is
// derived from the original's.
//
// Setup: a conversation titled "worker" is a member of group
// "alpha". The pane is live.
//
// Action: the human clones "worker". (Cloning forks the agent into
// an independent copy that inherits identity but keeps running
// alongside the original.)
//
// Expected: the clone is renamed to "worker-c-1" — the daemon
// derives the clone's title as `<original-title>-c-<N>` and injects
// it via /rename on the new pane. The original keeps its "worker"
// title (no stray rename on the source). Membership rows carry no
// name of their own; the clone's single name is its title.
//
// CloneFresh is used (no_copy_conv: true) to skip the .jsonl
// copy path so the test stays off convops.CopyConversationToPath.
func TestClone_DerivesTitleFromOriginal(t *testing.T) {
	f := newFlow(t)

	const oldConv = "old-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-old-001"
	const oldTmux = "tclaude-spwn-old-001"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	c := f.AsHuman().CloneFresh(oldConv)

	assert.Equal(t, oldConv, c.OldConv, "OldConv")

	// Surface-level invariants the human would see post-clone in
	// `tclaude agent groups members alpha`:
	//   - both members visible (clone is ADD-only — original isn't
	//     touched);
	//   - the new clone shows the derived title "worker-c-1" (the
	//     post-spawn /rename rendering at the members surface);
	//   - the original retains its title (no stray /rename on the
	//     source).
	f.AssertCloneTitle(c, "alpha", "worker-c-1", 5*time.Second)
	f.AssertGroupMember("alpha", c.NewConv, "worker-c-1", 5*time.Second)
	f.AssertGroupMember("alpha", oldConv, "worker", 1*time.Second)
}
