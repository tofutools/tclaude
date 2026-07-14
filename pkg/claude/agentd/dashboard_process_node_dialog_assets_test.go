package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardProcessNodeDialogAssets pins the load-bearing wiring of the
// node edit dialogs (TCL-298) inside the embedded dashboard assets. These
// are LITERAL needles: editing the JS/CSS means updating them here.
func TestDashboardProcessNodeDialogAssets(t *testing.T) {
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

	form := read("js/process-node-form.js")
	mustContain("process-node-form.js", form,
		// The uniform performer contract: ONE field table, kind-scoped there
		// and nowhere else — plus the discipline rule surfaced in the module.
		"export const PERFORMER_FIELDS",
		"export const PERFORMER_KINDS",
		"kind-scoping it in the Go model first",
		// Kind switches prune wrong-kind fields instead of carrying them.
		"export function setPerformerKind(",
		"export const RETRY_ON_FAIL_MODES",
	)
	if strings.Contains(form, "document.") || strings.Contains(form, "fetch(") {
		t.Error("process-node-form.js must stay pure (no DOM, no fetch) so Node tests cover the shipped file")
	}

	dialog := read("js/process-node-dialog.js")
	mustContain("process-node-dialog.js", dialog,
		// One component, two modes: the editor dialog and the viewer's
		// read-only detail card share the same builder.
		"export function buildNodeDetail(",
		"export function openNodeDialog(",
		"is-readonly",
		"process-node-readonly-badge",
		// Program performers are command execution (design §10) — said in
		// the UI next to the command field.
		"Program performers are command execution",
		// Edges are canvas-owned; the dialog only summarizes them.
		"Edges are edited on the canvas, not in this dialog.",
		// Every slot's editor goes through the shared sub-component.
		"performerEditor",
		"process-choice-outcomes",
		"setChoiceOutcome",
		"choiceRouting: false",
		// Dialog edits are private until explicit Save, which commits exactly
		// once through the edit model's undo gate.
		"const original = structuredClone(model.node(nodeId))",
		"model.updateNode(nodeId, (node) => replaceNode(node, draft))",
		"confirmDiscard = async () => false",
		"process-node-save",
		"process-node-cancel",
		"bindDialogFocus",
		"dispose.requestClose = requestCancel",
		// The node dialog gets its own DB-backed size and opts out of the
		// natural-content floor so complex forms remain shrinkable/scrollable.
		"makeModalResizable(dialog, NODE_DIALOG_SIZE_PREF, { fitContent: false })",
		"tclaude.dash.modalSize.process-node-editor",
	)
	if strings.Contains(dialog, "innerHTML") {
		t.Error("process-node-dialog.js must not use innerHTML; template content is untrusted at render time")
	}
	if strings.Contains(dialog, "localStorage") {
		t.Error("localStorage is banned; dialog prefs belong in dashPrefs -> SQLite")
	}
	if strings.Contains(dialog, "fetch(") {
		t.Error("process-node-dialog.js must not talk REST; dialogs mutate the client edit model only")
	}
	if strings.Contains(dialog, "confirm-modal") {
		t.Error("process-node-dialog.js must not double-book the shared #confirm-modal singleton")
	}

	editModel := read("js/process-edit-model.js")
	mustContain("process-edit-model.js", editModel,
		// The dialogs' single mutation gate: draft-based, no-op-safe, undoable.
		"updateNode(id, mutate)",
	)

	editor := read("js/process-editor.js")
	mustContain("process-editor.js", editor,
		// Logical zoom entry points: double-click and the inspector button.
		"openNodeSettings(",
		"this.openNodeSettings(node.id)",
		// The TCL-296 editability seam picks the mode: locked nodes render
		// the same component read-only (the viewer detail card).
		"this.model.config.nodeEditable(nodeId) ? 'edit' : 'view'",
	)

	css := read("dashboard.css")
	mustContain("dashboard.css", css,
		".process-node-modal .process-node-dialog",
		".process-node-section",
		".process-performer-editor",
		".process-node-readonly-badge",
		".process-node-security-note",
		// Inline controls are explicitly themed in both skins (UA-white trap).
		".process-node-close",
		"body.wizard .process-node-section",
		"body.wizard .process-node-close",
		"body.wizard .process-node-dialog-actions",
		// Both axes resize within viewport-aware bounds. The body owns scroll,
		// while wide dialogs turn extra width into a two-column form.
		"resize: both;",
		"max-width: min(1100px, calc(100vw - 32px));",
		"max-height: calc(100vh - 32px);",
		".process-node-dialog-body { flex: 1 1 auto; overflow: auto;",
		"grid-template-columns: repeat(auto-fit, minmax(min(160px, 100%), 1fr));",
		"@container process-node-dialog (min-width: 840px)",
		"column-count: 2;",
		".process-node-section { break-inside: avoid;",
	)

	// The dialog modules stay out of every eager entry module: only the
	// lazily imported editor may import them (flag-off page loads render
	// nothing, and the graph/editor bundle stays off the initial path).
	for _, entry := range []string{"js/dashboard.js", "js/tabs.js", "js/processes.js"} {
		source := read(entry)
		for _, module := range []string{"process-node-dialog.js", "process-node-form.js"} {
			if strings.Contains(source, module) {
				t.Errorf("%s eagerly imports %s; flag-off must render nothing", entry, module)
			}
		}
	}
}
