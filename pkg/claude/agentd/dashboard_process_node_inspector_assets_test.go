package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardProcessNodeInspectorAssets pins the load-bearing wiring of the
// editor's docked node inspector (E3, which replaced the TCL-298 node dialog)
// inside the embedded dashboard assets. These are LITERAL needles: editing
// the JS/CSS means updating them here.
func TestDashboardProcessNodeInspectorAssets(t *testing.T) {
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

	if _, err := fs.Stat(dashboardAssetsFS, "js/process-node-dialog.js"); err == nil {
		t.Error("the modal node dialog is retired; the docked inspector is the node editor")
	}

	blocks := read("js/process-node-blocks.js")
	mustContain("process-node-blocks.js", blocks,
		"export const NODE_BLOCKS",
		"export function visibleBlocks(",
		"export function addableBlocks(",
		"export function removeBlock(",
	)
	if strings.Contains(blocks, "document.") || strings.Contains(blocks, "fetch(") {
		t.Error("process-node-blocks.js must stay pure (no DOM, no fetch) so Node tests cover the shipped file")
	}

	fields := read("js/process-node-fields.js")
	mustContain("process-node-fields.js", fields,
		// Every performer slot goes through the ONE shared editor.
		"export function PerformerEditor(",
		"process-choice-outcomes",
		"setChoiceOutcome",
		// Program performers are command execution (design §10) — said in
		// the UI next to the command field.
		"Program performers are command execution",
	)

	inspector := read("js/process-node-inspector.js")
	mustContain("process-node-inspector.js", inspector,
		"export function NodeInspector(",
		// Every edit is one undoable, validated step through the editor gate.
		"controller.editNode(nodeId, mutate)",
		// The editability seam renders locked nodes read-only.
		"!model.config.nodeEditable(nodeId)",
		"is-readonly",
		"process-node-readonly-badge",
		// Decisions route by edge, so their choices carry no pass/fail map.
		"choiceRouting=${type !== 'decision'}",
		// Edges are canvas-owned; the inspector only summarizes them.
		"Edges are drawn and relabelled on the canvas.",
		// Block order is a per-kind UI preference in dashPrefs.
		"tclaude.dash.processInspector.blockOrder.",
		"nodeInspectorActions",
	)
	for name, source := range map[string]string{"process-node-inspector.js": inspector, "process-node-fields.js": fields} {
		if strings.Contains(source, "innerHTML") {
			t.Errorf("%s must not use innerHTML; template content is untrusted at render time", name)
		}
		if strings.Contains(source, "localStorage") {
			t.Errorf("%s: localStorage is banned; inspector prefs belong in dashPrefs -> SQLite", name)
		}
		if strings.Contains(source, "fetch(") {
			t.Errorf("%s must not talk REST; it mutates the client edit model only", name)
		}
	}

	editModel := read("js/process-edit-model.js")
	mustContain("process-edit-model.js", editModel,
		// The node editor's single mutation gate: draft-based, no-op-safe, undoable.
		"updateNode(id, mutate)",
	)

	editor := read("js/process-editor.js")
	mustContain("process-editor.js", editor,
		// Logical zoom: double-click (and Enter) focuses the docked inspector.
		"openNodeSettings(",
		"this.openNodeSettings(node.id)",
		"this.nodeFocusRequest += 1",
		"editNode(nodeId, mutate)",
		"this.mutate(() => this.model.updateNode(nodeId, mutate))",
	)

	css := read("dashboard.css")
	mustContain("dashboard.css", css,
		".process-node-inspector {",
		".pni-block {",
		".pni-add {",
		".process-node-section",
		".process-performer-editor",
		".process-node-readonly-badge",
		".process-node-security-note",
		// Inline controls are explicitly themed in both skins (UA-white trap).
		".process-node-close",
		"body.wizard .process-node-section",
		"body.wizard .process-node-close",
		"body.wizard .process-node-inspector",
	)
	if strings.Contains(css, "process-node-dialog") {
		t.Error("dashboard.css still carries retired node dialog rules")
	}

	// The node editor modules stay out of every eager entry module: only the
	// lazily imported editor may import them (flag-off page loads render
	// nothing, and the graph/editor bundle stays off the initial path).
	for _, entry := range []string{"js/dashboard.js", "js/tabs.js", "js/processes.js"} {
		source := read(entry)
		for _, module := range []string{"process-node-inspector.js", "process-node-fields.js", "process-node-form.js"} {
			if strings.Contains(source, module) {
				t.Errorf("%s eagerly imports %s; flag-off must render nothing", entry, module)
			}
		}
	}
}
