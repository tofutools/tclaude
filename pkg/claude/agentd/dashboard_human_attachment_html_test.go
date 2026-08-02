package agentd

import (
	"strings"
	"testing"
)

func TestDashboardHTML_HumanAttachmentWired(t *testing.T) {
	island := string(mustReadFS(dashboardAssetsFS, "js/mail-island.js"))
	shared := string(mustReadFS(dashboardAssetsFS, "js/human-attachments.js"))
	css := string(mustReadFS(dashboardAssetsFS, "dashboard.css"))

	for needle, why := range map[string]string{
		"function HumanAttachment(":   "the Preact Messages reader renders attachment metadata",
		"messageAttachments(message)": "the reader renders every published file, not just the first",
		`class="mail-attachment"`:     "the reader includes the attachment card",
	} {
		if !strings.Contains(island, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}
	for needle, why := range map[string]string{
		"export function messageAttachments(":                              "surfaces share one reading of the attachment list",
		"/api/human-messages/${encodeURIComponent(message.id)}/attachment": "the card falls back to the authenticated download route",
	} {
		if !strings.Contains(shared, needle) {
			t.Errorf("shared attachment helper missing %q (%s)", needle, why)
		}
	}
	if !strings.Contains(css, ".mail-attachment {") {
		t.Error("dashboard CSS is missing the attachment card styles")
	}
}
