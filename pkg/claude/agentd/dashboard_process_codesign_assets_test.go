package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardProcessCodesignAssets pins the load-bearing wiring of the
// editor's AI co-design surface (E4): the docked scribe (chat + terminal) and
// the on-canvas proposal review. LITERAL needles: editing the JS/CSS means
// updating them here.
func TestDashboardProcessCodesignAssets(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		body, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("embedded %s missing: %v", name, err)
		}
		return string(body)
	}
	mustContain := func(name, source string, needles ...string) {
		t.Helper()
		for _, needle := range needles {
			if !strings.Contains(source, needle) {
				t.Errorf("%s missing %q", name, needle)
			}
		}
	}

	diff := read("js/process-draft-diff.js")
	mustContain("process-draft-diff.js", diff,
		"export function diffTemplateDrafts(",
		"export function mergeDraftDecisions(",
		"export function overlayDraftDiff(",
	)
	for _, banned := range []string{"document.", "fetch(", "innerHTML"} {
		if strings.Contains(diff, banned) {
			t.Errorf("process-draft-diff.js must stay pure so Node tests cover the shipped file; found %q", banned)
		}
	}

	island := read("js/process-editor-island.js")
	mustContain("process-editor-island.js", island,
		"import { GetHelpMenu } from './get-help-menu.js';",
		"import { DiffReviewBar } from './process-diff-review.js';",
		"<${DiffReviewBar} controller=${controller} view=${view} />",
		"onClick=${() => controller.openDiffReview()}>Review on canvas<//>",
	)

	for _, gone := range []string{"process-scribe-dock", "scribeDock", "toggleScribeDock"} {
		if strings.Contains(island, gone) || strings.Contains(read("js/processes-island.js"), gone) {
			t.Errorf("the docked scribe is gone (tcl-zdnw9w); found %q", gone)
		}
	}
	mustContain("dashboard.css", read("dashboard.css"),
		".process-diff-review {", ".process-node.is-diff-added .process-node-shape",
	)
}
