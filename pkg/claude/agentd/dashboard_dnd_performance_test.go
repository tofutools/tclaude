package agentd

import (
	"strings"
	"testing"
)

// Dragover is a pointer-rate hot path. Keep hover ownership local to each
// gesture: document-wide marker scans and unconditional text replacement turn
// otherwise compositor-only pill movement into repeated style/paint work.
func TestDashboardDnDHotPathAvoidsRedundantDOMWork(t *testing.T) {
	sources := map[string]string{
		"member":          string(mustReadFS(dashboardAssetsFS, "js/dnd.js")),
		"group":           string(mustReadFS(dashboardAssetsFS, "js/group-reorder.js")),
		"dock":            string(mustReadFS(dashboardAssetsFS, "js/dock-dnd.js")),
		"reverse dock":    string(mustReadFS(dashboardAssetsFS, "js/dock-save-dnd.js")),
		"process template": string(mustReadFS(dashboardAssetsFS, "js/process-template-dnd.js")),
	}

	for name, source := range sources {
		if !strings.Contains(source, "pill.textContent !==") {
			t.Errorf("%s drag rewrites unchanged pill text in its dragover path", name)
		}
	}

	for name, source := range map[string]string{
		"group":        sources["group"],
		"dock":         sources["dock"],
		"reverse dock": sources["reverse dock"],
	} {
		if strings.Contains(source, "$$('.group-drop-before, .group-drop-after, .group-drop-into, .group-drop-clone')") ||
			strings.Contains(source, "$$('.dock-drop-over')") ||
			strings.Contains(source, "$$('.dock-save-over')") {
			t.Errorf("%s drag scans the document for its exclusive hover marker", name)
		}
	}

	if !strings.Contains(sources["group"], "groupDragGroupsByName = snapshotGroupsByName()") ||
		!strings.Contains(sources["group"], "const byName = groupDragGroupsByName") {
		t.Error("group drag does not retain its topology map for the gesture")
	}
}
