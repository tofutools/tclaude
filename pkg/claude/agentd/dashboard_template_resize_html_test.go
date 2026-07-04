package agentd

import (
	"strings"
	"testing"
)

// TestDashboardCSS_TemplateRowTextareaResizable pins the JOH-359 fix. The
// template editor's nested free-text fields (per-agent brief, work-pattern
// step body, process-phase criteria, rhythm body) are textareas that are
// DIRECT children of a column-flex .template-agent-row. The row's shared rule
// gives every control flex:1 (= flex-basis:0) so the grid inputs share width,
// but on a column-flex child that hands the height to the flex algorithm,
// which silently ignores the resize grip's dragged inline height — the grip
// renders yet dragging does nothing. Resetting the textareas to flex:0 0 auto
// lets their own (resized) height govern.
//
// It is CSS-only, so we string-pin the embedded rule rather than run a
// browser: a refactor that re-adds flex:1 or drops the reset would otherwise
// re-break resizing silently in the dashboard.
func TestDashboardCSS_TemplateRowTextareaResizable(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	must(".template-agent-row textarea { resize: vertical; flex: 0 0 auto; }",
		"nested template-editor textareas must reset flex-basis so the resize grip works")
}
