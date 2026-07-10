package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardProcessEditorAssets pins the load-bearing wiring of the
// template editor shell (TCL-296) inside the embedded dashboard assets.
// These are LITERAL needles: editing the JS/CSS means updating them here.
func TestDashboardProcessEditorAssets(t *testing.T) {
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

	editModel := read("js/process-edit-model.js")
	mustContain("process-edit-model.js", editModel,
		"export class ProcessEditModel",
		"export function blankEditView(",
		"export function graphEdgeID(",
		"export const MAX_UNDO",
		"export const PALETTE_PRIMITIVES",
		"export const PALETTE_SNIPPETS",
		// The unique-(from,outcome) invariant and the start pseudo edge are the
		// two server contracts the pure model enforces client-side; self-loops
		// are refused because v1 processes are acyclic and saves are advisory.
		"duplicate edge",
		"outcome: START_OUTCOME",
		"self-loop edges are not supported",
		// The phase-4 run-view seam: mode-level insertion permission plus
		// per-node/edge predicates for existing items.
		"canInsert: config.canInsert !== false",
	)
	if strings.Contains(editModel, "document.") || strings.Contains(editModel, "fetch(") {
		t.Error("process-edit-model.js must stay pure (no DOM, no fetch) so Node tests cover the shipped file")
	}

	editor := read("js/process-editor.js")
	mustContain("process-editor.js", editor,
		"export class ProcessTemplateEditor",
		"export async function openTemplateEditor(",
		// Palette drags carry ONLY the custom MIME (dock-dnd idiom): document
		// level DnD features key off text/plain and must ignore palette drags.
		"application/x-tclaude-process-palette",
		// The 409 conflict dialog is explicit — reload theirs or rebase-and-save;
		// never a silent overwrite.
		"process_template_conflict",
		"resolveConflict",
		"'Reload their version (discard mine)'",
		"'Save as new version anyway'",
		// Rewire affordance on mid-graph node deletion.
		"'Delete + rewire through'",
		// Hand-drawn self-loops are blocked at the gesture with a message.
		"Self-loop edges are not supported",
		// Editor semantics attach through the core's hooks, not core edits.
		"onPortDragStart:",
		"onCanvasDrop:",
	)
	if strings.Contains(editor, "localStorage") || strings.Contains(read("js/process-edit-model.js"), "localStorage") {
		t.Error("localStorage is banned; editor prefs belong in dashPrefs -> SQLite")
	}
	if strings.Contains(editor, "innerHTML") {
		t.Error("process-editor.js must not use innerHTML; template content is untrusted at render time")
	}

	processes := read("js/processes.js")
	mustContain("processes.js", processes,
		// The editor loads lazily after the feature-gated tab opens.
		"await import('./process-editor.js')",
		"openTemplateEditor(mount, { id, blank })",
		"confirmLeaveDirtyEditor",
	)

	css := read("dashboard.css")
	mustContain("dashboard.css", css,
		".process-editor-header",
		".process-editor-palette",
		".process-palette-card",
		".process-editor-inline-input",
		".process-editor-band",
		".process-editor-inspector",
		// Inline controls are explicitly dark-themed (UA-white trap).
		".process-inspector-select",
		"body.wizard .process-palette-card",
	)

	// The graph core stays out of every eager entry module: only the lazily
	// imported editor may import it (flag-off page loads render nothing).
	for _, entry := range []string{"js/dashboard.js", "js/tabs.js", "js/processes.js"} {
		if strings.Contains(read(entry), "process-graph.js") {
			t.Errorf("%s eagerly imports process-graph.js; flag-off must render nothing", entry)
		}
	}
}
