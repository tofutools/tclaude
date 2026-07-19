package agentd

import (
	"io/fs"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	processmodel "github.com/tofutools/tclaude/pkg/claude/process/model"
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
		"export function processSelectionRenderedCenter(",
		"layoutProcessGraph({",
		"export const MAX_UNDO",
		"export const PALETTE_PRIMITIVES",
		"export const PALETTE_SNIPPETS",
		// The unique-(from,outcome) invariant and the start pseudo edge are the
		// two server contracts the pure model enforces client-side; self-loops
		// are refused because v1 processes are acyclic and saves are advisory.
		"already has a connector labelled",
		"outcome: START_OUTCOME",
		"self-loop edges are not supported",
		// The phase-4 run-view seam: mode-level insertion permission plus
		// per-node/edge predicates for existing items.
		"canInsert: config.canInsert !== false",
		// Draft ids participate in dirty/discard state outside graph history;
		// restore always retains the current identity.
		"this.savedTemplateID = this.template.id || ''",
		"this.template.id = id",
		"setParams(params)",
	)
	blankStart := strings.Index(editModel, "export function blankEditView(")
	if blankStart < 0 {
		t.Fatal("process-edit-model.js blankEditView body missing")
	}
	blankEnd := strings.Index(editModel[blankStart:], "\n}\n\n// A blank shell")
	if blankEnd < 0 {
		t.Fatal("process-edit-model.js blankEditView body missing")
	}
	blankFactory := editModel[blankStart : blankStart+blankEnd]
	mustContain("process-edit-model.js blankEditView", blankFactory,
		"start: 'start'",
		"start: { type: 'start' }",
		"{ from: '', outcome: START_OUTCOME, to: 'start' }",
		"layout: { nodes: { start: { x: 120, y: 90 } } }",
		"sourceHash: ''",
	)
	for _, forbidden := range []string{"end: {", "to: 'end'", "result: 'success'"} {
		if strings.Contains(blankFactory, forbidden) {
			t.Errorf("process-edit-model.js blankEditView still prepopulates End via %q", forbidden)
		}
	}
	externalChange := read("js/process-external-change.js")
	mustContain("process-external-change.js", externalChange,
		"export function reconcileExternalChange(",
		"export function keepExternalChange(",
		"export function templateHeadFromEditView(",
		"export function summarizeTemplateChange(",
		"export function attachExternalReview(",
		"export function templateHeadSignature(",
		"nodeIDCharacters: 96",
		"nodeIDBytes: 192",
		"serializedBytes: 65536",
		"renderedBytes: 16384",
		"shortenedNodeID: '… [ID shortened]'",
		"prior.kind === 'kept' && prior.ref === current && prior.sourceHash === currentSource",
		"sourceHash: String(head?.sourceHash || '')",
	)
	for _, banned := range []string{"document.", "fetch(", "setTimeout(", "setInterval("} {
		if strings.Contains(externalChange, banned) {
			t.Errorf("process-external-change.js must stay pure; found %q", banned)
		}
	}
	if strings.Contains(editModel, "document.") || strings.Contains(editModel, "fetch(") {
		t.Error("process-edit-model.js must stay pure (no DOM, no fetch) so Node tests cover the shipped file")
	}
	clipboard := read("js/process-editor-clipboard.js")
	mustContain("process-editor-clipboard.js", clipboard,
		"export const PROCESS_CLIPBOARD_PREFIX",
		"export const PROCESS_CLIPBOARD_MAX_BYTES = 256 * 1024",
		"export function validateProcessSelectionPayload(",
		"export function validateProcessEditNode(",
		"export function createProcessSelectionPayload(",
		"export function serializeProcessSelection(",
		"export function parseProcessSelection(",
		"Clipboard selection contains duplicate edge outcomes.",
		"Clipboard selection contains an edge with a missing endpoint.",
		"Clipboard selection contains an unsupported process graph cycle.",
		"delete node.next",
	)
	// Keep the browser's synchronous untrusted-data gate mechanically locked to
	// the exact recursive Go edit wire. Semantic rules remain server-owned, but
	// adding/removing a JSON struct field cannot silently leave clipboard shape
	// validation behind.
	fieldSet := func(name string) []string {
		t.Helper()
		block := regexp.MustCompile(`(?s)const ` + name + ` = new Set\(\[(.*?)\]\);`).FindStringSubmatch(clipboard)
		if len(block) != 2 {
			t.Fatalf("process-editor-clipboard.js missing %s declaration", name)
		}
		matches := regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(block[1], -1)
		fields := make([]string, 0, len(matches))
		for _, match := range matches {
			fields = append(fields, match[1])
		}
		sort.Strings(fields)
		return fields
	}
	jsonFields := func(value any) []string {
		t.Helper()
		typ := reflect.TypeOf(value)
		fields := make([]string, 0, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				fields = append(fields, name)
			}
		}
		sort.Strings(fields)
		return fields
	}
	for name, want := range map[string][]string{
		"NODE_FIELDS":      jsonFields(processmodel.Node{}),
		"STEP_FIELDS":      jsonFields(processmodel.Step{}),
		"PERFORMER_FIELDS": jsonFields(processmodel.Performer{}),
		"CONTACT_FIELDS":   jsonFields(processmodel.ContactSchedule{}),
		"RETRY_FIELDS":     jsonFields(processmodel.RetryPolicy{}),
		"WAIT_FIELDS":      jsonFields(processmodel.WaitConfig{}),
	} {
		if got := fieldSet(name); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("process-editor-clipboard.js %s drifted from Go edit wire: got %v, want %v", name, got, want)
		}
	}
	for _, banned := range []string{"document.", "fetch(", "navigator.clipboard", "localStorage"} {
		if strings.Contains(clipboard, banned) {
			t.Errorf("process-editor-clipboard.js must stay pure and event-agnostic; found %q", banned)
		}
	}
	snippetLibrary := read("js/process-snippet-library.js")
	mustContain("process-snippet-library.js", snippetLibrary,
		"import { validateProcessSelectionPayload } from './process-editor-clipboard.js';",
		"const API = '/api/process/snippets';",
		"credentials: 'same-origin'",
		"base.payload = validateProcessSelectionPayload(raw.envelope)",
		"export function validateProcessSnippetName(value)",
		"body: JSON.stringify({ name, envelope })",
		"body: JSON.stringify({ name, revision: snippet.revision })",
		"body: JSON.stringify({ revision: snippet.revision })",
		"Never retain or surface the rejected raw bytes.",
	)
	for _, banned := range []string{"localStorage", "sessionStorage", "indexedDB", "document.", "innerHTML"} {
		if strings.Contains(snippetLibrary, banned) {
			t.Errorf("process-snippet-library.js must use the dashboard API and shared selection envelope only; found %q", banned)
		}
	}

	editor := read("js/process-editor.js")
	connectionFeedback := read("js/process-connection-feedback.js")
	portAvailability := read("js/process-port-availability.js")
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
		// Read-time awareness stays in the controller and reloads in place; no
		// dirty buffer is replaced without confirmation.
		"observeExternalHead({ ref: currentRef, sourceHash: currentSourceHash, actor, authoredAt } = {})",
		"sameTemplateGeneration(this.externalChange, reviewed)",
		"reloadExternalChange()",
		"this.options.confirmDiscard?.()",
		"this.externalDecisionPending = true",
		"decision.model.sourceHash === decision.sourceHash",
		"this.modalDispose === decision.modal",
		"this.externalChange.sourceHash === decision.targetSourceHash",
		"guardedModel.rev !== guardedRev",
		"externalInteractionPending(this)",
		"const { review: exactExternalReview, ...externalChange } = this.externalChange",
		"review: { summary: structuredClone(exactExternalReview.summary) }",
		"this.refresh();",
		// IDs are creation-time store keys and mutations cross the semantic
		// controller boundary rather than reading controls from the DOM.
		"setTemplateID(value)",
		"this.model.setTemplateID(String(value || '').trim())",
		"Template id is fixed once an existing version is selected.",
		"const savedID = id",
		"this.model.template.id = savedID",
		// Template-level metadata has an explicit editor affordance and travels
		// through setTemplateMeta, the same dirty/undo gate as graph edits.
		"if (selection?.type === 'template')",
		"this.graph?.setSelection?.(null)",
		"if (this.selection?.type !== 'template')",
		"if (this.savePending || externalInteractionPending(this)) return false",
		"if (requestSeq !== this.saveSeq) return",
		"this.saveSeq += 1",
		"setTemplateMeta(fields)",
		"this.model.setTemplateMeta(clean)",
		"Save before instantiating",
		"unsaved editor state is never instantiated",
		"ref: this.model.currentRef",
		// Rewire affordance on mid-graph node deletion.
		"'Delete + rewire through'",
		// The pure semantic resolver is shared by presentation and commit
		// preflight, so feedback cannot drift from accepted editor gestures.
		"prepareProcessConnectionFeedback, resolveProcessConnectionFeedback,",
		"connectionFeedback: (request, prepared) => resolveProcessConnectionFeedback(this.model, request, prepared)",
		"connectionFeedbackPreparation: () => prepareProcessConnectionFeedback(this.model)",
		"const feedback = resolveProcessConnectionFeedback(this.model, {",
		// Editor semantics cross the one explicit adapter. Pointer-frame state
		// never becomes controller or Signals state.
		"createProcessGraphAdapter(host, {",
		"nodeDragEnd: (event) => this.commitNodeDrag(event)",
		"portDragEnd: (event) => this.onPortDragEnd(event)",
		"canvasDrop: (event) => this.onCanvasDrop(event)",
		"graphInteraction(this)",
		"This removes the current highlighted selection.",
		"requestCommandPalette()",
		"commandContext()",
		"addNodeType(payload.type, point)",
		"duplicateSelection()",
		"this.model.duplicateNodes(",
		"onEditorCopy(event)",
		"onEditorPaste(event)",
		"resolveProcessPastePlacement(fingerprint, this.pasteTargetPoint()",
		"canvasPointerMove: (event) => this.onCanvasPointerMove(event)",
		"canvasPointerLeave: () => this.onCanvasPointerLeave()",
		"this.graph?.containsClientPoint?.(pointer.clientX, pointer.clientY)",
		"event?.isTrusted === false",
		"hasNonCollapsedDOMSelection(event)",
		"event.clipboardData.setData('text/plain', text)",
		"event.clipboardData.getData('text/plain')",
		"this.model.insertClipboardSelection(payload",
		"loadProcessSnippets({ signal: this.abort.signal })",
		"createProcessSelectionPayload(this.model, this.selection, layout?.nodes || [])",
		"validateProcessSelectionPayload(snippet.payload)",
		"this.snippetLoadSeq += 1",
		"result.generation === expectedGeneration",
		"const loaded = await this.loadCustomSnippets()",
		"this.validation?.focusIssue(delta)",
	)
	if strings.Contains(editor, "navigator.clipboard") {
		t.Error("process editor clipboard must use trusted ClipboardEvent data without a permission API flow")
	}
	mustContain("process-connection-feedback.js", connectionFeedback,
		"export function prepareProcessConnectionFeedback(",
		"export function resolveProcessConnectionFeedback(",
		"processEdgePortAvailability(model.node(from), model.node(to))",
		"processNodePortAvailable(sourceNode, source.port)",
		"Self-loop connections are not supported because v1 processes are acyclic.",
		"Connect this input to an output port or another node body.",
		"Adding connected nodes is not allowed in this view.",
	)
	mustContain("process-port-availability.js", portAvailability,
		"export function processNodePortAvailable(",
		"export function processNodePortAvailability(",
		"export function processPortUnavailableMessage(",
		"export function processEdgePortAvailability(",
		"Start nodes cannot have incoming connections.",
		"End nodes cannot have outgoing connections.",
	)
	for _, banned := range []string{"document.", "fetch(", "setTimeout(", "setInterval("} {
		if strings.Contains(portAvailability, banned) {
			t.Errorf("process-port-availability.js must stay pure; found %q", banned)
		}
	}
	for _, banned := range []string{"document.", "fetch(", "setTimeout(", "setInterval("} {
		if strings.Contains(connectionFeedback, banned) {
			t.Errorf("process-connection-feedback.js must stay pure; found %q", banned)
		}
	}
	island := read("js/process-editor-island.js")
	mustContain("process-editor-island.js", island,
		"export function ProcessEditorApp(",
		"export function mountProcessEditorIsland(",
		"import { NodeDialog } from './process-node-dialog.js';",
		"import { ParamsDialog } from './process-params-dialog.js';",
		"templateIDEditable(view.blank, model.sourceHash)",
		"controller.setSelection({ type: 'template' })",
		"controller.setTemplateMeta({ name })",
		"params…", "instantiate…", "⌘K commands",
		"Review changes", "Keep editing",
		"Keep editing preserves this draft; Save still uses CAS and will stop on a 409 conflict.",
		"process-issues-panel",
		"descriptor.kind === 'node'", "descriptor.kind === 'params'",
		"key=${descriptor.generation}",
		`class="process-editor-inspector" inert=${pending}`,
		"discardBufferedChange.current",
		"onCopy=${(event) => controller.onEditorCopy(event)}",
		"onPaste=${(event) => controller.onEditorPaste(event)}",
		"Built-in snippets",
		"Custom snippets",
		"controller.saveSelectionAsSnippet()",
		"controller.insertPaletteItem(payload)",
		"controller.renameCustomSnippet(payload.id)",
		"controller.deleteCustomSnippet(payload.id)",
		"validateProcessSnippetName(name)",
		`class="field process-editor-field process-snippet-name-field"`,
		`placeholder="e.g. Release review"`,
		`aria-describedby="process-snippet-name-help process-snippet-name-error"`,
	)
	adapter := read("js/process-graph-adapter.js")
	mustContain("process-graph-adapter.js", adapter,
		"export class ProcessGraphAdapter",
		"new ProcessGraph(host, graph",
		"onNodeDragEnd:", "onNodeDragCancel:", "onPortDragStart:", "onCanvasDrop:",
		"onCanvasPointerMove: emit('canvasPointerMove')",
		"onCanvasPointerLeave: emit('canvasPointerLeave')",
		"containsClientPoint(clientX, clientY)",
		"interactionSnapshot()", "hasActiveInteraction()", "dispose()",
	)
	for _, banned := range []string{"document.", "querySelector(", "innerHTML", "replaceChildren(", ".widget"} {
		if strings.Contains(editor, banned) {
			t.Errorf("process-editor.js crosses the Preact/graph ownership boundary with %q", banned)
		}
	}
	graphConsumers := map[string]string{
		"process-editor.js":        editor,
		"process-editor-island.js": island,
		"process-validation.js":    read("js/process-validation.js"),
		"process-node-dialog.js":   read("js/process-node-dialog.js"),
		"process-params-dialog.js": read("js/process-params-dialog.js"),
	}
	for name, source := range graphConsumers {
		for _, banned := range []string{
			"new ProcessGraph(", "from './process-graph.js'", ".widget",
			".graph.layout;", ".graph.layout.", ".graph.layout[",
			".graph.view;", ".graph.view.", ".graph.view[",
			".graph.root", ".graph.svg", ".graph.pointer",
		} {
			if strings.Contains(source, banned) {
				t.Errorf("%s bypasses process-graph-adapter.js with %q", name, banned)
			}
		}
	}
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
		"validateNow()",
		"focusIssue(delta = 1)",
	)
	commands := read("js/process-command-registry.js")
	mustContain("process-command-registry.js", commands,
		"export function buildProcessNodeTypeCommands(",
		"export function buildProcessEditorCommands(",
		"editor.deleteSelection()",
		"editor.save()",
		"editor.requestInstantiate()",
		"actions?.activateSubtab?.('templates')",
		"actions?.activateSubtab?.('runs')",
	)
	nodeTypes := read("js/process-node-types.js")
	mustContain("process-node-types.js", nodeTypes,
		"export const PROCESS_NODE_TYPES",
		"type: 'task'", "type: 'decision'", "type: 'parallel'", "type: 'wait'", "type: 'start'", "type: 'end'",
	)
	chooser := read("js/process-node-chooser.js")
	mustContain("process-node-chooser.js", chooser,
		"buildProcessNodeTypeCommands", "rankCommands", "role: 'combobox'",
		"role: 'listbox'", "role: 'option'", "aria-activedescendant",
		"event.key !== 'Escape'", "aria-disabled", "disabledReason", "onOutsidePointerDown",
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
		"onInstantiate: actions?.openInstantiation",
		"describeActor: actions?.describeActor",
		"onOpenActor: actions?.openActor",
		"editor?.destroy?.()",
		"document.addEventListener('tclaude:snapshot', poll)",
		"void actions.load('worklist', { quiet: true })",
		"void actions.observeTemplateHeads()",
		"function InstantiateDialog(",
		"useDialogFocus({",
		"initialParamValues(params)",
		"initializedRef.current === spec.ref",
		"data-process-param-input",
		"type === 'boolean'",
		"actions.submitInstantiation(resolved)",
		"<option value=\"\">Not set</option>",
		"viewerBackRef.current?.focus({ preventScroll: true })",
		"registerCommandProvider('process-editor'",
		"buildProcessEditorCommands({ editor: state.currentEditor(), actions })",
	)
	if strings.Contains(processes, "String(Number(value))") {
		t.Error("processes-island.js must preserve number-param strings without JS precision round-tripping")
	}
	params := read("js/process-params-dialog.js")
	mustContain("process-params-dialog.js", params,
		"export function ParamsDialog(",
		"export function openProcessParamsDialog(",
		"<${Overlay}",
		"registered?.({ isDirty, requestClose })",
		"dispose.isDirty = () => !!handle?.isDirty?.()",
		"dispose.requestClose = () => handle?.requestClose?.()",
		"confirmDiscard = async () => false",
		"model.setParams(params)",
		"process-param-default-enabled",
		"row.param.default",
		"Renamed or deleted references are reported by live validation.",
	)
	mustContain("process-editor.js", editor,
		"confirmDiscard: this.options.confirmDiscard",
	)
	actions := read("js/processes-actions.js")
	mustContain("processes-actions.js", actions,
		"publishMatchingHead(generation, heads)",
		"generation.model.currentRef !== generation.ref",
		"generation.model.sourceHash !== generation.sourceHash",
		"generation.editor.observeExternalHead?.(head)",
		"name === 'templates' && (requestBusy(lifecycle) || headObservationPending)",
		"'/v1/process/template-heads'",
		"export function processActorPresentation(",
		"`/api/open-window/${encodeURIComponent(agentId)}`",
		"async function openInstantiation(",
		"mintAttemptID = mintUUID",
		"const runId = `${id}-${mintAttemptID()}`",
		"body.currentRef !== ref",
		"async function submitInstantiation(params)",
		"fetchImpl('/v1/process/runs'",
		"body: JSON.stringify({ templateRef: spec.ref, runId: spec.runId, params })",
		"openViewer(body.run.id)",
	)

	css := read("dashboard.css")
	mustContain("dashboard.css", css,
		".process-editor-header",
		".process-editor-external",
		// TCL-583: actionable error statuses carry recovery instructions, so
		// they take a full-width wrapping row rather than a truncated slot in
		// the toolbar. Neither skin overrides .is-error layout, so the same
		// rule serves the default and wizard chrome; the markup must not fall
		// back to a pointer-only title.
		".process-editor-status.is-error {",
		"flex: 1 0 100%",
		"white-space: normal",
		// An edge outcome may legally be 512 unbroken characters
		// (PROCESS_CLIPBOARD_MAX_OUTCOME) and is quoted verbatim in the
		// message, so the row needs explicit break-anywhere containment or it
		// overflows the clipping editor mount and loses the recovery clause.
		"min-width: 0",
		"overflow-wrap: anywhere",
		"body.wizard .process-editor-external",
		".process-external-review",
		"body.wizard .process-external-source-summary",
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
		".process-param-dialog",
		".process-instantiate-dialog",
		"body.wizard .process-param-dialog",
		"body.wizard .process-instantiate-dialog",
		`.process-editor-modal .process-editor-field input:hover:not(:disabled):not(:focus):not([aria-invalid="true"])`,
		`.process-editor-modal .process-editor-field input[aria-invalid="true"]`,
		".process-editor-modal .process-editor-field input:disabled",
		".process-editor-modal .process-editor-field input:-webkit-autofill",
		"body.wizard .process-editor-modal",
		".palette-item.disabled",
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

// TestDashboardProcessEditorScrollbarsScoped pins TCL-571's ownership and
// theme contract. Actual editor scroll owners opt into one marker; the command
// palette and instantiate dialog are shared portals, so they may join only
// under the existing active-editor marker. Viewer/worklist and unrelated
// dashboard scroll regions must never be captured by this block.
func TestDashboardProcessEditorScrollbarsScoped(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		body, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("embedded %s missing: %v", name, err)
		}
		return string(body)
	}
	css := read("dashboard.css")
	start := strings.Index(css, "/* Actual editor-owned overflow containers carry this marker.")
	end := strings.Index(css, ".process-editor-header {")
	if start < 0 || end <= start {
		t.Fatal("dashboard.css process scrollbar contract is missing or misplaced")
	}
	block := css[start:end]
	for _, needle := range []string{
		".process-scroll-surface,",
		"body:has(#tab-processes.active #process-editor-view) #command-palette-modal .palette-list,",
		"body:has(#tab-processes.active #process-editor-view) .process-instantiate-dialog {",
		"body.wizard:has(#tab-processes.active #process-editor-view) #command-palette-modal .palette-list,",
		"--process-scrollbar-track: #0d1117;",
		"--process-scrollbar-thumb: #6e7681;",
		"--process-scrollbar-thumb-hover: #8b949e;",
		"--process-scrollbar-thumb-active: #b1bac4;",
		"--process-scrollbar-corner: #161b22;",
		"--process-scrollbar-track: #181226;",
		"--process-scrollbar-thumb: #755da0;",
		"--process-scrollbar-thumb-hover: #957ac0;",
		"--process-scrollbar-thumb-active: #d4af37;",
		"--process-scrollbar-corner: #211832;",
		"scrollbar-width: thin;",
		"scrollbar-color: var(--process-scrollbar-thumb) var(--process-scrollbar-track);",
		".process-scroll-surface::-webkit-scrollbar-thumb:hover,",
		".process-scroll-surface::-webkit-scrollbar-thumb:active,",
		".process-scroll-surface::-webkit-scrollbar-corner,",
		"@media (forced-colors: active)",
		"scrollbar-width: auto;",
		"scrollbar-color: auto;",
		"background: ButtonText;",
		"background: Highlight;",
	} {
		if !strings.Contains(block, needle) {
			t.Errorf("dashboard.css process scrollbar block missing %q", needle)
		}
	}
	for _, banned := range []string{
		".process-viewer",
		".process-worklist",
		"body.wizard #command-palette-modal .palette-list",
		"body.wizard .process-instantiate-dialog",
	} {
		if strings.Contains(block, banned) {
			t.Errorf("dashboard.css process scrollbar block widened into %q", banned)
		}
	}

	for name, needle := range map[string]string{
		"js/processes-island.js":      "process-canvas-view${spec.kind === 'editor' ? ' process-scroll-surface' : ''}",
		"js/process-editor-island.js": "process-editor-palette process-scroll-surface",
		"js/process-node-chooser.js":  "process-node-chooser-list process-scroll-surface",
		"js/process-node-dialog.js":   "process-node-dialog-body process-scroll-surface",
		"js/process-params-dialog.js": "process-param-list process-scroll-surface",
	} {
		if !strings.Contains(read(name), needle) {
			t.Errorf("%s missing editor scroll ownership marker %q", name, needle)
		}
	}

	for _, theme := range []struct {
		name, anchor string
	}{
		{name: "default", anchor: ".process-scroll-surface,"},
		{name: "wizard", anchor: "body.wizard .process-scroll-surface,"},
	} {
		ruleStart := strings.Index(block, theme.anchor)
		if ruleStart < 0 {
			t.Fatalf("%s scrollbar theme rule is missing", theme.name)
		}
		ruleOpen := strings.Index(block[ruleStart:], "{")
		if ruleOpen < 0 {
			t.Fatalf("%s scrollbar theme rule is malformed", theme.name)
		}
		ruleBodyStart := ruleStart + ruleOpen
		ruleClose := strings.Index(block[ruleBodyStart:], "}")
		if ruleClose < 0 {
			t.Fatalf("%s scrollbar theme rule is malformed", theme.name)
		}
		rule := block[ruleBodyStart : ruleBodyStart+ruleClose]
		tokens := map[string]string{}
		for _, match := range regexp.MustCompile(`--process-scrollbar-(track|thumb|thumb-hover|thumb-active): (#[0-9a-f]{6});`).FindAllStringSubmatch(rule, -1) {
			tokens[match[1]] = match[2]
		}
		if len(tokens) != 4 {
			t.Fatalf("%s scrollbar theme has %d contrast tokens, want 4", theme.name, len(tokens))
		}
		normal := cssContrastRatio(t, tokens["thumb"], tokens["track"])
		hover := cssContrastRatio(t, tokens["thumb-hover"], tokens["track"])
		active := cssContrastRatio(t, tokens["thumb-active"], tokens["track"])
		for state, ratio := range map[string]float64{"normal": normal, "hover": hover, "active": active} {
			if ratio < 3 {
				t.Errorf("%s %s thumb contrast is %.2f:1, want at least 3:1", theme.name, state, ratio)
			}
		}
		if !(normal < hover && hover < active) {
			t.Errorf("%s thumb contrast must increase normal < hover < active; got %.2f < %.2f < %.2f", theme.name, normal, hover, active)
		}
	}
}

func cssContrastRatio(t *testing.T, foreground, background string) float64 {
	t.Helper()
	luminance := func(value string) float64 {
		value = strings.TrimPrefix(value, "#")
		if len(value) != 6 {
			t.Fatalf("invalid CSS hex color %q", value)
		}
		channels := make([]float64, 3)
		for i := range channels {
			parsed, err := strconv.ParseUint(value[i*2:i*2+2], 16, 8)
			if err != nil {
				t.Fatalf("invalid CSS hex color %q: %v", value, err)
			}
			channel := float64(parsed) / 255
			if channel <= 0.04045 {
				channels[i] = channel / 12.92
			} else {
				channels[i] = math.Pow((channel+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*channels[0] + 0.7152*channels[1] + 0.0722*channels[2]
	}
	a, b := luminance(foreground), luminance(background)
	return (math.Max(a, b) + 0.05) / (math.Min(a, b) + 0.05)
}
