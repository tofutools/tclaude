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
		"group bots":      {"js/groups-list.js", "human-notification-hint"},
		"global bots":     {"js/shell-island.js", "human-notification-hint"},
		"row action":      {"js/row-action-handler.js", "view-human-notifications"},
		"mail bridge":     {"js/mail-bridge.js", "openHumanNotifications"},
		"mail controller": {"js/mail.js", "openHumanNotifications"},
		"styles":          {"dashboard.css", ".human-notification-attention"},
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
}
