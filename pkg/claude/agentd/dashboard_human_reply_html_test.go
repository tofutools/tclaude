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
	island := string(mustReadFS(dashboardAssetsFS, "js/mail-island.js"))
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The Preact reader renders the delegated reply/focus controls with the
	// sender attributes the dialog needs. The controller keeps the shared
	// online helper exported for both the island and dialog.
	if !strings.Contains(island, `data-act="msg-reply"`) {
		t.Error("Messages island is missing the delegated reply action")
	}
	if !strings.Contains(island, `data-agent=${message.from_agent || ''}`) ||
		!strings.Contains(island, `data-conv=${message.from_conv}`) {
		t.Error("Messages island reply control is missing sender identity attributes")
	}
	must("function senderOnline(", "mail.js defines the shared sender-online helper")
	must("focusAccessRequest, openMailbox, senderOnline,", "mail.js exports senderOnline for the reply dialog")

	// row-actions.js: the delegated msg-reply handler opens the dialog.
	must("case 'msg-reply':", "row-actions.js handles the msg-reply click")
	must("openHumanReplyModal({", "the msg-reply handler opens the reply dialog")

	// modal-human-reply.js: the dialog module, its online gate, and the POST.
	must("export { openHumanReplyModal, bindHumanReplyModal }", "the reply modal module exports its entrypoints")
	must("function syncReplyOnline(", "the dialog re-derives the sender's live/offline state")
	must("senderOnline(replyCtx.agent, replyCtx.conv)", "the dialog gates Send on the sender being online in the accepted snapshot")
	must("'/api/human-messages/reply'", "the dialog POSTs to the reply endpoint")
	must("document.addEventListener('tclaude:snapshot', syncReplyOnline)", "the dialog follows accepted snapshots while open")
	must("document.removeEventListener('tclaude:snapshot', syncReplyOnline)", "the dialog removes its snapshot listener on close")
	if strings.Contains(dashboardAssets, "setInterval(pollReplyOnline") {
		t.Error("reply modal retains its duplicate snapshot polling loop")
	}
	must(`code === 'offline'`, "the dialog trusts the server's offline verdict on a 409")

	// dashboard.js: the module is bound at init.
	must("bindHumanReplyModal();", "the reply modal is bound at dashboard init")

	// dashboard.html: the modal shell + its fields.
	must(`id="human-reply-modal"`, "the reply modal markup ships")
	must(`id="human-reply-body"`, "the reply textarea ships")
	must(`id="human-reply-status"`, "the online-status line ships")
	must(`id="human-reply-submit"`, "the Send button ships")
}
