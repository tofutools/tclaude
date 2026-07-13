package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// Preact keeps the group and dock nodes stable across snapshot publishes, but
// native HTML5 drag events still live at the document boundary. Each binder
// must therefore expose an idempotent teardown rather than accumulating global
// listeners if the owning page lifecycle ends.
func TestDashboardDndBindersAreDisposable(t *testing.T) {
	directTerminal := map[string]string{
		"dnd.js":           "row.addEventListener('dragend', endDndDrag, { once: true });",
		"group-reorder.js": "handle.addEventListener('dragend', endGroupDrag, { once: true });",
		"dock-dnd.js":      "card.addEventListener('dragend', endDockDrag, { once: true });",
		"dock-save-dnd.js": "reverseSource.addEventListener('dragend', endReverseDrag, { once: true });",
	}
	for name, terminalNeedle := range directTerminal {
		body, err := fs.ReadFile(dashboardAssetsFS, "js/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		source := string(body)
		for _, needle := range []string{
			"const removers = [];",
			"target.removeEventListener(type, listener, options)",
			"if (cleaned) return;",
			"for (const remove of removers.splice(0).reverse()) remove();",
		} {
			if !strings.Contains(source, needle) {
				t.Errorf("%s missing disposable-listener contract %q", name, needle)
			}
		}
		if !strings.Contains(source, terminalNeedle) {
			t.Errorf("%s missing source-local terminal cleanup %q", name, terminalNeedle)
		}
	}

	if !strings.Contains(dashboardAssets, "for (const cleanup of dndCleanups.reverse()) cleanup?.();") {
		t.Error("dashboard page lifecycle does not invoke the DnD binder teardowns")
	}
	if !strings.Contains(dashboardAssets, "if (event.persisted) return;") {
		t.Error("dashboard pagehide teardown must retain DnD listeners for bfcache restores")
	}
}
