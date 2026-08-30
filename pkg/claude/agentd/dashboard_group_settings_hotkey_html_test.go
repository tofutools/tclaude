package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardGroupSettingsSubmitHotkey pins the group editor to the shared
// management-overlay keyboard-submit contract. Group startup context is a
// multiline field, so plain Enter must remain available while Ctrl/Cmd+Enter
// saves from anywhere in the dialog.
func TestDashboardGroupSettingsSubmitHotkey(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}

	dialog := read("js/action-dialog-island.js")
	const groupSettingsOverlay = `id="group-settings-modal" labelledby="group-settings-title" onClose=${() => actions.close(descriptor)} onSubmitHotkey=${submit}`
	if !strings.Contains(dialog, groupSettingsOverlay) {
		t.Fatal("group settings dialog does not submit through the shared Ctrl/Cmd+Enter hotkey")
	}
	for _, line := range strings.Split(dialog, "\n") {
		if strings.Contains(line, `id="group-settings-modal"`) && strings.Contains(line, "onSubmitEnter") {
			t.Fatal("group settings dialog must preserve plain Enter for its multiline startup-context field")
		}
	}

	overlay := read("js/management-overlay.js")
	const submitHotkeyBranch = `if (
        onSubmitHotkey &&
        event.key === 'Enter' &&
        !event.isComposing &&
        event.keyCode !== 229 &&
        (event.ctrlKey || event.metaKey)
      ) {
        event.preventDefault();
        onSubmitHotkey();
      }`
	if !strings.Contains(overlay, submitHotkeyBranch) {
		t.Fatal("management overlay no longer provides the guarded Ctrl/Cmd+Enter submit contract")
	}
}
