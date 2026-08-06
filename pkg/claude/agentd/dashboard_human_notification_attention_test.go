package agentd

import (
	"strings"
	"testing"
)

func TestHumanNotificationAttentionAssetsAreWired(t *testing.T) {
	checks := map[string]struct {
		asset  string
		needle string
	}{
		"member row":    {"js/groups-member-table.js", "human-notification-attention"},
		"hover preview": {"js/groups-member-table.js", "human-notification-preview"},
		"preview a11y":  {"js/groups-member-table.js", "aria-describedby"},
		// A notification may carry a subject and an attachment but no body. Every
		// surface that shows a body must say the emptiness is deliberate rather
		// than leaving a blank gap (or, in the hover preview, calling it empty).
		"bodiless copy":   {"js/human-attachments.js", "the attached file is the notification"},
		"bodiless hover":  {"js/groups-member-table.js", "bodilessNotice(message)"},
		"bodiless reader": {"js/mail-island.js", "bodilessNotice(message)"},
		"bodiless drawer": {"js/groups-notification-reader.js", "bodilessNotice(message)"},
		"bodiless style":  {"dashboard.css", ".notification-bodiless"},
		"group bots":      {"js/groups-list.js", "human-notification-hint"},
		"global bots":     {"js/shell-island.js", "human-notification-hint"},
		"reader launch":   {"js/human-notification-attention.js", "tclaude:open-human-notification"},
		"reader island":   {"js/human-notification-reader-island.js", "mountHumanNotificationReaderIsland"},
		"reader mount":    {"js/dashboard.js", "mountHumanNotificationsFeature"},
		"reader stores":   {"js/preact-loader.js", "publishLocalEdit"},
		"reader root":     {"dashboard.html", "human-notification-reader-root"},
		"terminal tab":    {"js/terminal-shell-island.js", "mux-tab-attention"},
		"terminal reader": {"js/terminal-shell-island.js", "openHumanNotificationReader"},
		"terminal styles": {"mux.css", ".mux-tab-attention"},
		"quick reader":    {"js/groups-notification-reader.js", "GroupsNotificationReader"},
		"reader a11y":     {"js/groups-notification-reader.js", "aria-live=\"polite\""},
		"read action":     {"js/groups-notification-reader.js", "/api/human-messages/read"},
		"attachment":      {"js/groups-notification-reader.js", "attachmentHref(message, attachment)"},
		"mail bridge":     {"js/mail-bridge.js", "openHumanNotifications"},
		"mail controller": {"js/mail.js", "selectMessage(first.id)"},
		"group a11y":      {"js/groups-list.js", "a member has unread notifications"},
		"global a11y":     {"js/shell-island.js", "one or more agents have unread notifications"},
		"styles":          {"dashboard.css", ".human-notification-attention"},
		"drawer styles":   {"dashboard.css", ".human-notification-drawer"},
		"panel geometry":  {"dashboard.css", ".human-notification-drawer {\n  position: fixed; z-index: 90;\n  top: var(--dock-top); right: 0; bottom: var(--footer-h);"},
		"panel slide in":  {"dashboard.css", "@keyframes human-notification-drawer-in"},
		"panel slide out": {"dashboard.css", ".human-notification-drawer.closing"},
		"reduced motion":  {"dashboard.css", ".human-notification-drawer.closing { animation: none; }"},
		"shared inset":    {"js/chrome-inset.js", "applyChromeTopInset"},
		"inset on open":   {"js/human-notification-reader-island.js", "applyChromeTopInset()"},
		"wizard skin":     {"dashboard.css", "body.wizard .human-notification-drawer"},
		"vegas skin":      {"dashboard.css", "body.slop .human-notification-drawer"},
		"exit hold":       {"js/human-notification-reader-island.js", "CLOSE_ANIMATION_MS"},
	}
	for name, check := range checks {
		asset := dashboardAssetFile(t, check.asset)
		if !strings.Contains(asset, check.needle) {
			t.Errorf("%s (%s) does not contain %q", name, check.asset, check.needle)
		}
	}
}

// Opening the quick reader must leave the notification unread: clearing the
// attention marker is the operator's explicit decision, made with the reader's
// "Mark read" action (or implied by sending a reply, which the reply handler
// marks read server-side). Guard against the mark-on-open effect coming back.
func TestGroupsNotificationReaderDoesNotMarkReadOnOpen(t *testing.T) {
	asset := dashboardAssetFile(t, "js/groups-notification-reader.js")
	if strings.Contains(asset, "persistHumanMessageRead(state, message.id, true") {
		t.Error("the quick reader marks the notification read on open; that decision belongs to the operator")
	}
	if !strings.Contains(asset, "persistHumanMessageRead(state, message.id, !message.read") {
		t.Error("the quick reader no longer exposes the operator-driven read toggle")
	}
}

func TestHumanNotificationSearchMatchesStableAgentID(t *testing.T) {
	message := mailboxMessage{
		FromAgent: "agt_stable_sender",
		FromConv:  "old-conversation-generation",
		FromTitle: "old title",
	}
	if !humanMsgMatchesSearch(message, "agt_stable_sender") {
		t.Fatal("human notification search must match the rotation-stable sender id")
	}
	other := mailboxMessage{
		FromAgent: "agt_other",
		Subject:   "help with agt_stable_sender",
		Body:      "agt_stable_sender asked for this",
	}
	if humanMsgMatchesSender(other, "agt_stable_sender") {
		t.Fatal("structured sender filter must not match another agent mentioning the id")
	}
}
