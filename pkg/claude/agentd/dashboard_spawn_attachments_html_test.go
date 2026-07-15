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
	present(`const addAttachments = (files) => {`, "controlled owner adds chosen/pasted files")
	present(`const removeAttachment = (id) => {`, "controlled owner removes one attachment")
	present(`URL.revokeObjectURL(attachment.url)`, "unmount clears + revokes object URLs")
	present(`const paste = (event) => {`, "component captures pasted clipboard files/images")
	// Keyboard-repeat guard (JOH-307): holding ⌘/Ctrl-V auto-repeats the paste
	// event; handleSpawnPaste drops a file that repeats the previous paste within
	// a short burst window, scoped to paste so picker/drag stay un-deduped.
	present(`function attachKey(`, "per-file signature used by the key-repeat guard")
	present(`const PASTE_REPEAT_MS = 1000`, "the key-repeat burst window")
	present(`pasteState.current.keys`, "tracks the previous paste's files to detect repeats")
	present(`onDragEnter=${dragEnter}`, "wires Finder/Explorer drag-and-drop onto the dialog")
	present(`onDrop=${drop}`, "drop delivery is component-owned")
	present(`async uploadAttachments(attachments)`, "plain actions upload to /api/spawn-attachments")
	present(`/api/spawn-attachments`, "the upload endpoint path is wired client-side")
	present(`body.attachments = [...attachmentPaths]`, "uploaded paths ride along in the spawn body")

	// CSS: the list styling + the drag-over highlight exist.
	present(`.spawn-attachments-list`, "the attachment-list CSS")
	present(`.cron-create-modal.spawn-drag-over`, "the drag-over highlight CSS")
}
