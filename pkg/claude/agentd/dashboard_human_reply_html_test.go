package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_HumanReplyWired guards the reply-to-a-notification
// feature's cross-file wiring: the Human folder reader's `reply` button,
// the dialog markup, the module that drives it, and the delegated click
// handler all have to stay in lockstep — a rename in one silently breaks
// the feature in the browser. Asserting on the embedded concatenation
// (dashboard.html + css + every js/*.js) catches it at `go test ./...`,
// the same string-pin approach as the slop / wizard guards.
func TestDashboardHTML_HumanReplyWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// mail.js: the reader button renders with the msg-reply action and the
	// sender attributes the dialog needs, and the online helper it shares
	// with the focus button is exported for the dialog to gate on.
	must("function humanReplyButton(", "mail.js renders the reply button")
	must(`data-act="msg-reply"`, "the reply button carries the msg-reply action")
	must("${humanReplyButton(m)}${humanFocusButton(m)}", "the reply button is placed in the human-folder reader actions")
	must("function senderOnline(", "mail.js defines the shared sender-online helper")
	must("openMailbox, senderOnline }", "mail.js exports senderOnline for the reply dialog")

	// row-actions.js: the delegated msg-reply handler opens the dialog.
	must("case 'msg-reply':", "row-actions.js handles the msg-reply click")
	must("openHumanReplyModal({", "the msg-reply handler opens the reply dialog")

	// modal-human-reply.js: the dialog module, its online gate, and the POST.
	must("export { openHumanReplyModal, bindHumanReplyModal }", "the reply modal module exports its entrypoints")
	must("function syncReplyOnline(", "the dialog re-derives the sender's live/offline state")
	must("senderOnline(replyCtx.agent, replyCtx.conv)", "the dialog gates Send on the sender being online")
	must("'/api/human-messages/reply'", "the dialog POSTs to the reply endpoint")
	must("tclaude:snapshot", "the dialog re-syncs online state on each snapshot tick")

	// dashboard.js: the module is bound at init.
	must("bindHumanReplyModal();", "the reply modal is bound at dashboard init")

	// dashboard.html: the modal shell + its fields.
	must(`id="human-reply-modal"`, "the reply modal markup ships")
	must(`id="human-reply-body"`, "the reply textarea ships")
	must(`id="human-reply-status"`, "the online-status line ships")
	must(`id="human-reply-submit"`, "the Send button ships")
}
