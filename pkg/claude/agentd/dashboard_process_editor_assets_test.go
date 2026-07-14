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
		// Draft ids participate in dirty/discard state outside graph history;
		// restore always retains the current identity.
		"this.savedTemplateID = this.template.id || ''",
		"this.template.id = id",
	)
	externalChange := read("js/process-external-change.js")
	mustContain("process-external-change.js", externalChange,
		"export function reconcileExternalChange(",
		"export function keepExternalChange(",
		"export function templateHeadSignature(",
		"prior.kind === 'kept' && prior.ref === current",
	)
	for _, banned := range []string{"document.", "fetch(", "setTimeout(", "setInterval("} {
		if strings.Contains(externalChange, banned) {
			t.Errorf("process-external-change.js must stay pure; found %q", banned)
		}
	}
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
		// Read-time awareness stays in the persistent imperative editor and
		// reloads in place; no dirty buffer is replaced without confirmation.
		"Template changed externally (new version)",
		"text: 'Keep editing'",
		"observeExternalRef(currentRef)",
		"reloadExternalChange()",
		"this.options.confirmDiscard?.()",
		"guardedModel.rev !== guardedRev",
		"this.savePending || this.externalReloadPending",
		"this.refresh();",
		// IDs are creation-time store keys. Existing templates render only the
		// title, and a blank template swaps its id input out after first save.
		"const showIDInput = templateIDEditable(this.blank, model.sourceHash)",
		"const idEditable = showIDInput && !this.savePending && !this.externalReloadPending",
		"this.idInput.disabled = !idEditable",
		"this.identity.replaceChildren(showIDInput ? this.idInput : this.titleLabel)",
		"this.model.setTemplateID(this.idInput.value.trim())",
		"Template id is fixed once an existing version is selected.",
		"const savedID = id",
		"this.model.template.id = savedID",
		// Template-level metadata has an explicit editor affordance and travels
		// through setTemplateMeta, the same dirty/undo gate as graph edits.
		"text: 'template settings…'",
		"this.settingsButton.addEventListener('click', () => this.setSelection({ type: 'template' })",
		"if (selection?.type === 'template')",
		"this.graph.select(null)",
		"if (this.selection?.type !== 'template')",
		"this.saveButton.disabled = this.savePending ||",
		"if (this.savePending || this.externalReloadPending) return false",
		"if (requestSeq !== this.saveSeq) return",
		"this.saveSeq += 1",
		"this.model.setTemplateMeta({ name:",
		// Rewire affordance on mid-graph node deletion.
		"'Delete + rewire through'",
		// Hand-drawn self-loops are blocked at the gesture with a message.
		"Self-loop edges are not supported",
		// Editor semantics attach through the core's hooks, not core edits.
		"onPortDragStart:",
		"onCanvasDrop:",
		"onMarqueeSelect:",
		"onNodeDragStart:",
		"wheelPan: true",
		"marqueeSelect: true",
		"This removes the current highlighted selection.",
	)
	if strings.Contains(editor, "localStorage") || strings.Contains(read("js/process-edit-model.js"), "localStorage") {
		t.Error("localStorage is banned; editor prefs belong in dashPrefs -> SQLite")
	}
	if strings.Contains(editor, "innerHTML") {
		t.Error("process-editor.js must not use innerHTML; template content is untrusted at render time")
	}

	validation := read("js/process-validation.js")
	mustContain("process-validation.js", validation,
		"export class ValidationScheduler",
		"export class LiveValidation",
		"export function mapDiagnostics(",
		"export function decorateGraph(",
		// Stale-response guard: a response is dropped unless it belongs to the
		// newest issued request.
		"seq !== this.seq",
		"'/v1/process/validate'",
		// An unserializable/rejected draft skips the round, never crashes it.
		"if (payload == null) return;",
		"if (!response.ok) return null;",
		// Badges are glyph-coded per severity, never color-only.
		"severityGlyph",
		"process-issues-panel",
	)
	if strings.Contains(validation, "innerHTML") {
		t.Error("process-validation.js must not use innerHTML; diagnostic text is untrusted at render time")
	}
	if strings.Contains(validation, "localStorage") {
		t.Error("localStorage is banned; panel prefs belong in dashPrefs -> SQLite")
	}

	mustContain("process-editor.js", editor,
		// Live validation wires through the editor's single post-mutation
		// choke point: decorate on refresh, debounce-schedule after.
		"new LiveValidation(",
		"this.validation.decorate(this.model.graph())",
		"this.validation?.schedule()",
		// Save/reload must sync the validation controller (cold-review
		// finding): a failed debounce round keeps prior diagnostics, so
		// markSaved and the 409-reload model swap each push their fresh
		// diagnostics into LiveValidation — otherwise badges/panel stay
		// stale until the next mutation.
		"this.validation?.applyDiagnostics(body.diagnostics || [])",
		"this.validation?.applyDiagnostics(view.diagnostics || [])",
	)

	processes := read("js/processes-island.js")
	mustContain("processes-island.js", processes,
		// The editor loads lazily after the feature-gated tab opens.
		"await import('./process-editor.js')",
		"loadEditor(mountRef.current, { id: spec.id, blank: spec.blank, config: { confirmDiscard } })",
		"editor?.destroy?.()",
		"document.addEventListener('tclaude:snapshot', poll)",
		"void actions.load('worklist', { quiet: true })",
		"void actions.observeTemplateHeads()",
	)
	actions := read("js/processes-actions.js")
	mustContain("processes-actions.js", actions,
		"publishMatchingHead(generation, heads)",
		"generation.model.currentRef !== generation.ref",
		"'/v1/process/template-heads'",
	)

	css := read("dashboard.css")
	mustContain("dashboard.css", css,
		".process-editor-header",
		".process-editor-external",
		"body.wizard .process-editor-external",
		".process-editor-palette",
		".process-palette-card",
		".process-editor-inline-input",
		".process-editor-band",
		".process-editor-inspector",
		".process-marquee",
		// Inline controls are explicitly dark-themed (UA-white trap).
		".process-inspector-select",
		"body.wizard .process-palette-card",
		"body.wizard .process-editor .process-action",
		// Live-validation issues panel, explicitly themed on both skins.
		".process-issues-panel",
		".process-issue:hover, .process-issue:focus-visible",
		"body.wizard .process-issues-panel",
	)

	// The graph core and validation loop stay out of every eager entry module:
	// only the lazily imported editor may import them (flag-off page loads
	// render nothing).
	for _, entry := range []string{"js/dashboard.js", "js/tabs.js", "js/processes.js", "js/processes-state.js", "js/processes-actions.js"} {
		for _, banned := range []string{"process-graph.js", "process-validation.js"} {
			if strings.Contains(read(entry), banned) {
				t.Errorf("%s eagerly imports %s; flag-off must render nothing", entry, banned)
			}
		}
	}
}
