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
		"member row":      {"js/groups-member-table.js", "human-notification-attention"},
		"hover preview":   {"js/groups-member-table.js", "human-notification-preview"},
		"preview a11y":    {"js/groups-member-table.js", "aria-describedby"},
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
		"auto read":       {"js/groups-notification-reader.js", "/api/human-messages/read"},
		"attachment":      {"js/groups-notification-reader.js", "/attachment"},
		"mail bridge":     {"js/mail-bridge.js", "openHumanNotifications"},
		"mail controller": {"js/mail.js", "selectMessage(first.id)"},
		"group a11y":      {"js/groups-list.js", "a member has unread notifications"},
		"global a11y":     {"js/shell-island.js", "one or more agents have unread notifications"},
		"styles":          {"dashboard.css", ".human-notification-attention"},
		"drawer styles":   {"dashboard.css", ".human-notification-drawer"},
	}
	for name, check := range checks {
		asset := dashboardAssetFile(t, check.asset)
		if !strings.Contains(asset, check.needle) {
			t.Errorf("%s (%s) does not contain %q", name, check.asset, check.needle)
		}
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
