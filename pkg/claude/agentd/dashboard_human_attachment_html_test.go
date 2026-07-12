package agentd

import (
	"strings"
	"testing"
)

func TestDashboardHTML_HumanAttachmentWired(t *testing.T) {
	island := string(mustReadFS(dashboardAssetsFS, "js/mail-island.js"))
	css := string(mustReadFS(dashboardAssetsFS, "dashboard.css"))

	for needle, why := range map[string]string{
		"function HumanAttachment(":                                        "the Preact Messages reader renders attachment metadata",
		"/api/human-messages/${encodeURIComponent(message.id)}/attachment": "the card targets the authenticated download route",
		`class="mail-attachment"`:                                          "the reader includes the attachment card",
	} {
		if !strings.Contains(island, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}
	if !strings.Contains(css, ".mail-attachment {") {
		t.Error("dashboard CSS is missing the attachment card styles")
	}
}
