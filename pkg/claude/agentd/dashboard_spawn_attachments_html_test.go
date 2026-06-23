package agentd

import (
	"strings"
	"testing"
)

// The spawn dialog gained file/screenshot attachments: a native file picker, a
// clipboard-image paste path, a pending-attachment list, and an upload-on-submit
// that hands the temp-dir paths to the spawn body. This guards the wiring across
// the embedded HTML / CSS / JS the same present-style way as the spawn-profiles
// UI test, searching the dashboardAssets concatenation.
func TestDashboardHTML_SpawnAttachmentsUI(t *testing.T) {
	present := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}

	// HTML: the attachments row, picker button, hidden file input, list mount.
	present(`id="agent-spawn-attachments-row"`, "the spawn dialog attachments row")
	present(`id="agent-spawn-attach-btn"`, "the attach-files button")
	present(`id="agent-spawn-attach-input"`, "the hidden native file input")
	present(`id="agent-spawn-attachments-list"`, "the pending-attachment list mount")

	// JS: the state helpers and the submit-time upload.
	present(`function addSpawnAttachments(`, "adds chosen/pasted files to the list")
	present(`function removeSpawnAttachment(`, "removes one attachment")
	present(`function clearSpawnAttachments(`, "clears + revokes object URLs on open/close")
	present(`function handleSpawnPaste(`, "captures pasted clipboard images")
	present(`function uploadSpawnAttachments(`, "uploads to /api/spawn-attachments")
	present(`/api/spawn-attachments`, "the upload endpoint path is wired client-side")
	present(`body.attachments = attachmentPaths`, "uploaded paths ride along in the spawn body")

	// CSS: the list styling exists.
	present(`.spawn-attachments-list`, "the attachment-list CSS")
}
