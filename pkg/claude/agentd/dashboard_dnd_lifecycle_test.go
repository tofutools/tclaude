package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// Preact keeps the group and dock nodes stable across snapshot publishes, but
// native HTML5 drag events still live at the document boundary. Each binder
// must therefore expose an idempotent teardown rather than accumulating global
// listeners if the owning page lifecycle ends. The shell migration collects
// those teardown functions with every other page-owned island/binder cleanup.
func TestDashboardDndBindersAreDisposable(t *testing.T) {
	directTerminal := map[string]string{
		"dnd.js":           "row.addEventListener('dragend', endDndDrag, { once: true });",
		"group-reorder.js": "handle.addEventListener('dragend', finishGroupDrag, { once: true });",
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

	autoScroll, err := fs.ReadFile(dashboardAssetsFS, "js/groups-drag-autoscroll.js")
	if err != nil {
		t.Fatalf("read groups-drag-autoscroll.js: %v", err)
	}
	for _, needle := range []string{
		"source.addEventListener('dragend', stop, { once: true });",
		"activeSource?.removeEventListener('dragend', stop);",
		"document.removeEventListener('dragstart', start);",
		"document.removeEventListener('dragover', update);",
		"document.removeEventListener('dragleave', leave);",
		"document.removeEventListener('dragend', stop);",
		"document.removeEventListener('drop', stop);",
	} {
		if !strings.Contains(string(autoScroll), needle) {
			t.Errorf("groups-drag-autoscroll.js missing disposable-listener contract %q", needle)
		}
	}

	for _, needle := range []string{
		"const pageCleanups = [];",
		"pageCleanups.push(bindDnd(), bindGroupReorder(), bindGroupsDragAutoScroll());",
		"pageCleanups.push(bindDockDnd());",
		"pageCleanups.push(bindDockSaveDnd());",
		"for (const cleanup of pageCleanups.reverse()) cleanup?.();",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard shared page lifecycle missing %q", needle)
		}
	}
	if !strings.Contains(dashboardAssets, "if (event.persisted) return;") {
		t.Error("dashboard pagehide teardown must retain DnD listeners for bfcache restores")
	}
}
