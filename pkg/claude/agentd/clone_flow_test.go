package agentd_test

import (
	"net/http"
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
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))
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
	f.AssertCloneTitle(c, "alpha", "worker-c-1", 10*time.Second)
	f.AssertGroupMember("alpha", c.NewConv, "worker-c-1", 10*time.Second)
	f.AssertGroupMember("alpha", oldConv, "worker", 10*time.Second)
}

// Scenario: cloning an agent WITH a follow-up handoff must not let the
// handoff's inbox nudge bleed into the clone's /rename.
//
// Setup: a titled conv "worker" alive in group "alpha".
//
// Action: the human clones "worker" with a follow-up. Post-clone, the
// new pane is both renamed to "worker-c-1" AND nudged about the queued
// handoff ("[system: new agent message #N for you. ...]").
//
// Regression: the rename and the handoff nudge used to run as two
// separate goroutines (runClonePostInit + deliverHandoffViaFlush) that
// both woke when the pane came online and send-keys'd into it
// concurrently. The nudge text could land inside the still-unsubmitted
// /rename line, so the clone's title became
// "worker-c-1[system: new agent message #N ...]". Post-init is now a
// single ordered goroutine — rename → settle gap → flush — so the
// rename submits as a clean line of its own before the nudge is typed.
//
// Expected: the clone's title is EXACTLY "worker-c-1" (no nudge
// suffix), and the handoff nudge is still delivered to the pane.
func TestClone_FollowUpNudgeDoesNotCorruptTitle(t *testing.T) {
	f := newFlow(t)

	const oldConv = "old-aaaa-bbbb-cccc-eeee"
	const oldLabel = "spwn-old-002"
	const oldTmux = "tclaude-spwn-old-002"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	c := f.AsHuman().CloneWith(oldConv, map[string]any{
		"no_copy_conv": true,
		"follow_up":    "pick up the merge conflict work",
	})
	if c.Code != http.StatusOK {
		t.Fatalf("clone with follow-up: status=%d body=%s", c.Code, c.Raw)
	}

	// The title settles to the clean derived form — never the
	// nudge-concatenated variant. AssertCloneTitle matches the title
	// exactly, so a leaked "[system: ...]" suffix fails here.
	f.AssertCloneTitle(c, "alpha", "worker-c-1", 10*time.Second)

	// And the handoff nudge is still delivered (as its own line, after
	// the rename) — folding flush into the ordered post-init goroutine
	// must not drop the message.
	f.AssertSentContains(c.TmuxTarget(), "new agent message", 10*time.Second)
}
