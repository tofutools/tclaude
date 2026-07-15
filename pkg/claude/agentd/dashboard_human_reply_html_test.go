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
	html := string(mustReadFS(dashboardAssetsFS, "dashboard.html"))
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

	// The controller is the sole launcher seam; the Preact dialog derives its
	// gate from the accepted snapshot and the action boundary owns the POST.
	must("export function openHumanReplyModal(context = {})", "the controller exports the reply launcher")
	must("senderOnline(snapshot, context.agent || '', context.conv || '')", "the dialog gates Send on live snapshot state")
	must("'/api/human-messages/reply'", "the dialog POSTs to the reply endpoint")
	must("useEffect(() => { setServerOffline(false); }, [snapshot])", "accepted snapshots reconcile a prior server-offline verdict")
	if strings.Contains(dashboardAssets, "setInterval(pollReplyOnline") {
		t.Error("reply modal retains its duplicate snapshot polling loop")
	}
	must(`cause?.code === 'offline'`, "the dialog trusts the server's offline verdict on a 409")

	// The static shell owns only the stable island host; dialog ids belong to
	// the component so a legacy duplicate cannot coexist in the viewport.
	if strings.Contains(html, `id="human-reply-modal"`) {
		t.Error("dashboard.html still contains the legacy human-reply modal")
	}
	must(`id="message-access-dialog-root"`, "the stable dialog island host ships")
	must(`id="human-reply-modal"`, "the Preact reply component owns the modal id")
	must(`id="human-reply-body"`, "the reply textarea ships")
	must(`id="human-reply-status"`, "the online-status line ships")
	must(`id="human-reply-submit"`, "the Send button ships")
}
