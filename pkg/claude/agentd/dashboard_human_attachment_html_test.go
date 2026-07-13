package agentd

import (
	"strings"
	"testing"
)

func TestDashboardHTML_HumanAttachmentWired(t *testing.T) {
	for needle, why := range map[string]string{
		"function humanAttachmentHTML(":                              "the Messages reader renders attachment metadata",
		"/api/human-messages/${encodeURIComponent(m.id)}/attachment": "the card targets the authenticated download route",
		`class="mail-attachment"`:                                    "the reader includes the attachment card",
		".mail-attachment {":                                         "the attachment card styles ship",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}
}
