package agentd_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario (JOH-177): a worker whose predecessor title carries a
// control character (a newline) is reincarnated. Both reincarnate
// rename injections — the old pane's `<prev>-x` archive rename and the
// new pane's `<base>` base-name rename — flow through deliverRename,
// which types the title into a live tmux pane via send-keys. A newline in
// that title would land as a premature Enter, splitting the keystroke
// stream: `/rename evil` submits and the bytes after the newline run as
// their own command. This is the keystroke-injection sink the cold
// review of #320 flagged (Minor 1) — byte-identical to pre-seam
// behavior, but a latent vector.
//
// Setup: a worker titled "evil\nrm -rf ~" (a hostile title with an
// embedded newline + payload) with a live pane, in group "alpha".
//
// Action: the human reincarnates it.
//
// Expected:
//   - Reincarnation SUCCEEDS and degrades gracefully — the rename is
//     skipped (not a hard failure), membership still migrates, and the
//     successor still receives its handoff. A rejected title must never
//     abort the whole lifecycle op.
//   - NO send-keys to EITHER pane ever carries the newline or the
//     "evil" / "rm -rf" payload fragments — deliverRename's
//     length-exempt charset gate (isValidRenameSink) rejects the title
//     before it reaches the pane.
//   - The old pane still receives `/exit` (soft-stop is independent of
//     the cosmetic archive rename).
func TestReincarnate_RejectsControlCharTitleAtSendKeysSink(t *testing.T) {
	f := newFlow(t)

	const oldConv = "evil-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-evil-001"
	const oldTmux = "tclaude-spwn-evil-001"
	// A hostile predecessor title: the newline is the injection vector
	// (a premature Enter in send-keys), and the bytes after it are the
	// payload that would run as its own command if the newline landed.
	const hostileTitle = "evil\nrm -rf ~"

	f.HaveConvWithTitle(oldConv, hostileTitle)
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, f.TestCwd("work"))
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	r := f.AsHuman().Reincarnate(oldConv, "fresh start handoff")

	// The successor's handoff nudge landing on the new pane is the
	// signal that the post-spawn goroutine ran to completion (it fires
	// AFTER the new-pane deliverRename attempt + flush), so by the time
	// we see it the new-pane rename decision has already been made.
	f.AssertSentContains(r.TmuxTarget(), "new agent message", 10*time.Second)

	// The old-pane sequence is synchronous in the handler, so /exit has
	// already been injected by the time Reincarnate() returned.
	assert.True(t, f.World.Tmux.WaitForSendKeys(oldTmux+":0.0", "/exit", 10*time.Second),
		"old pane should still receive /exit; sent=%+v", f.World.Tmux.Sent())

	// Core assertion: the hostile title never reached send-keys on
	// either pane. Pre-JOH-177 the old pane would have been typed
	// "/rename evil\nrm -rf ~-x" — the newline a premature Enter, the
	// payload a separate command. Scan EVERY recorded send-keys.
	assertNoInjectedTitle(t, f.World.Tmux.Sent())

	// Graceful degradation: the rename being rejected must not abort the
	// reincarnation. Membership migrates old → new and the successor is
	// present in the group the human would see via `agent groups members`.
	assertMemberPresent(t, f.ListGroupMembers(g.Name), r.NewConv)
	f.AssertNotGroupMember(g.Name, oldConv)
}

// assertNoInjectedTitle fails if any recorded send-keys carries a raw
// newline (the early-submit vector) or a fragment of the hostile title
// — proof the charset gate rejected it before the pane.
func assertNoInjectedTitle(t *testing.T, sent []testharness.SentKey) {
	t.Helper()
	for _, sk := range sent {
		assert.NotContains(t, sk.Text, "\n",
			"a raw newline reached send-keys (early-submit injection); target=%s text=%q", sk.Target, sk.Text)
		assert.False(t, strings.Contains(sk.Text, "evil") || strings.Contains(sk.Text, "rm -rf"),
			"hostile title payload reached send-keys; target=%s text=%q", sk.Target, sk.Text)
	}
}

// assertMemberPresent fails unless convID appears in the group's member
// list (title-agnostic — the successor's rename was deliberately skipped,
// so it carries a derived title, not the hostile one).
func assertMemberPresent(t *testing.T, members []testharness.MemberView, convID string) {
	t.Helper()
	for _, m := range members {
		if m.ConvID == convID {
			return
		}
	}
	t.Errorf("successor conv %s not found in group members; got %+v", convID, members)
}
