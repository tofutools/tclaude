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
	present(`function handleSpawnPaste(`, "captures pasted clipboard files/images")
	// Dedup guard (JOH-307): keyboard auto-repeat on ⌘/Ctrl-V fires repeated
	// paste events; addSpawnAttachments must skip a file already pending so each
	// file/screenshot attaches once. attachKey is the content-identity used for it.
	present(`function attachKey(`, "content-identity key used to dedupe attachments")
	present(`spawnAttachments.some(a => a.key === key)`, "skips files already pending (keyboard-repeat dedup)")
	present(`function bindSpawnDragDrop(`, "wires Finder/Explorer drag-and-drop onto the dialog")
	present(`bindSpawnDragDrop();`, "drag-and-drop is wired at bind time")
	present(`function uploadSpawnAttachments(`, "uploads to /api/spawn-attachments")
	present(`/api/spawn-attachments`, "the upload endpoint path is wired client-side")
	present(`body.attachments = attachmentPaths`, "uploaded paths ride along in the spawn body")

	// CSS: the list styling + the drag-over highlight exist.
	present(`.spawn-attachments-list`, "the attachment-list CSS")
	present(`.cron-create-modal.spawn-drag-over`, "the drag-over highlight CSS")
}
