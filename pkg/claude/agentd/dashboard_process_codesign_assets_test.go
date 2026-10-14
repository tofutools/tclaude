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
		"import { ScribeDock } from './process-scribe-dock.js';",
		"<${ScribeDock} controller=${controller} view=${view} />",
		"import { DiffReviewBar } from './process-diff-review.js';",
		"<${DiffReviewBar} controller=${controller} view=${view} />",
		"onClick=${() => controller.openDiffReview()}>Review on canvas</button>",
	)

	dock := read("js/process-scribe-dock.js")
	mustContain("process-scribe-dock.js", dock,
		"process-scribe-dock process-scroll-surface",
		"`/api/scribe/${encodeURIComponent(agentId)}/chat`",
	)
	if strings.Contains(dock, "innerHTML") || strings.Contains(dock, "dangerouslySetInnerHTML") {
		t.Error("scribe replies are untrusted text and must render as text")
	}
	mustContain("processes-island.js", read("js/processes-island.js"),
		"scribeDock: actions?.openScribe ? { scribes: () => state.view.value.scribes, openTerminal: actions.openScribe } : null",
	)
	mustContain("dashboard.css", read("dashboard.css"),
		".process-scribe-dock {", ".process-diff-review {", ".process-node.is-diff-added .process-node-shape",
	)
}
