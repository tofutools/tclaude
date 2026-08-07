package agentd

import (
	"strings"
	"testing"
)

func TestDashboardHTML_HumanAttachmentWired(t *testing.T) {
	island := string(mustReadFS(dashboardAssetsFS, "js/mail-island.js"))
	shared := string(mustReadFS(dashboardAssetsFS, "js/human-attachments.js"))
	preview := string(mustReadFS(dashboardAssetsFS, "js/image-preview-overlay.js"))
	css := string(mustReadFS(dashboardAssetsFS, "dashboard.css"))

	for needle, why := range map[string]string{
		"function HumanAttachment(":   "the Preact Messages reader renders attachment metadata",
		"messageAttachments(message)": "the reader renders every published file, not just the first",
		`class="mail-attachment"`:     "the reader includes the attachment card",
		`ImageAttachmentPreview`:      "Messages reuses the shared image preview",
	} {
		if !strings.Contains(island, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}
	for needle, why := range map[string]string{
		"export function messageAttachments(":                             "surfaces share one reading of the attachment list",
		"/api/human-messages/${encodeURIComponent(messageID)}/attachment": "the card falls back to the authenticated download route",
	} {
		if !strings.Contains(shared, needle) {
			t.Errorf("shared attachment helper missing %q (%s)", needle, why)
		}
	}
	if !strings.Contains(css, ".mail-attachment {") {
		t.Error("dashboard CSS is missing the attachment card styles")
	}
	// The zoom footer is a <footer>, and the page-level `footer { position:
	// fixed }` at the top of the stylesheet would otherwise pin it to the
	// window instead of the dialog. Behaviour: jstest/dialog-resize.test.mjs
	// covers the resizing itself.
	footerRule, _, _ := strings.Cut(css[strings.Index(css, ".image-preview-footer {"):], "}")
	for _, reset := range []string{"position: static", "z-index: auto"} {
		if !strings.Contains(footerRule, reset) {
			t.Errorf("the image viewer footer must reset %q, or the page-level footer rule reaches into the dialog", reset)
		}
	}
	if !strings.Contains(css, ".dialog-resizer {") {
		t.Error("dashboard CSS is missing the resize grip shared by the attachment viewers")
	}
	for _, name := range []string{"js/image-preview-overlay.js", "js/markdown-attachment.js"} {
		if !strings.Contains(dashboardAssetFile(t, name), "DialogResizer") {
			t.Errorf("%s does not offer the resize grip", name)
		}
	}
	for needle, why := range map[string]string{
		"role=\"dialog\"":     "the image viewer exposes dialog semantics",
		"aria-modal=\"true\"": "the image viewer is modal",
		"method: 'HEAD'":      "the image viewer distinguishes missing files before loading",
	} {
		if !strings.Contains(preview, needle) {
			t.Errorf("image preview source missing %q (%s)", needle, why)
		}
	}
}
