// process-editor.js -- the template editor surface for the Processes tab
// (TCL-296): full-canvas ProcessGraph + palette dock + edit ops + save/CAS.
//
// Split of responsibilities:
//   - process-graph.js owns presentation + pointer mechanics (hooks only).
//   - process-edit-model.js owns the pure edit model + undo/redo (node-tested).
//   - This module owns the DOM chrome (header, palette, inspector, dialogs),
//     translates graph hooks into model mutations, and talks REST.
//
// Palette drag uses the established dock-dnd idiom: cards are drag sources
// with a CUSTOM MIME only (never text/plain), so the document-level DnD
// features (dnd.js member moves, group-reorder, dock-dnd) ignore these drags
// entirely — and this module never attaches document-level drop handlers; the
// graph core's own canvas dragover/drop is the sole drop target.
//
// Forward-compat seam (design §9): openTemplateEditor threads a config object
// (mode + per-node/edge editability) into ProcessEditModel. A later run-editing
// surface reuses this editor with completed nodes locked; nothing here may
// hard-code template-only assumptions beyond the defaults it passes.
//
// Template content is untrusted at render time: all text lands via
// textContent (the h() helper), never via HTML string injection — the assets
// test enforces this with a literal needle.

import { ProcessGraph } from './process-graph.js';
import {
  ProcessEditModel, blankEditView,
  PALETTE_PRIMITIVES, PALETTE_SNIPPETS, templateIDEditable,
} from './process-edit-model.js';
import { openNodeDialog } from './process-node-dialog.js';
import { openProcessParamsDialog } from './process-params-dialog.js';
import { LiveValidation } from './process-validation.js';
import {
  NO_EXTERNAL_CHANGE, keepExternalChange, reconcileExternalChange,
} from './process-external-change.js';
import {
  makeSelection, selectionContains, selectionItems, toggleSelection,
} from './process-selection.js';
import { requestCommandPalette } from './command-registry.js';

const SVG_NS = 'http://www.w3.org/2000/svg';
// Custom drag payload MIME (dock-dnd idiom): withholding text/plain keeps
// every other document-level DnD feature out of a palette drag.
const PALETTE_MIME = 'application/x-tclaude-process-palette';

export function isProcessEditorFormControl(target) {
  const tag = String(target?.tagName || '').toUpperCase();
  return tag === 'INPUT' || tag === 'SELECT' || tag === 'TEXTAREA';
}

function externalInteractionPending(editor) {
  return !!(editor.externalDecisionPending || editor.externalReloadPending);
}

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === undefined || value === null) continue;
    if (key === 'class') el.className = value;
    else if (key === 'text') el.textContent = value;
    else if (key.startsWith('on') && typeof value === 'function') el.addEventListener(key.slice(2), value);
    else el.setAttribute(key, String(value));
  }
  for (const child of children) if (child) el.append(child);
  return el;
}

function shortHash(hash) {
  return hash ? hash.slice(0, 8) : '';
}

async function fetchEditView(id, version) {
  const query = version ? `?version=${encodeURIComponent(version)}` : '';
  const response = await fetch(`/v1/process/templates/${encodeURIComponent(id)}${query}`);
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
  return body;
}

export class ProcessTemplateEditor {
  constructor(mount, view, options = {}) {
    this.mount = mount;
    this.options = options;
    this.model = new ProcessEditModel(view, {
      mode: options.mode || 'template',
      nodeEditable: options.nodeEditable,
      edgeEditable: options.edgeEditable,
    });
    this.blank = !!options.blank;
    this.selection = null;
    this.pendingMove = null;
    this.band = null;
    this.savePending = false;
    this.saveSeq = 0;
    this.externalReloadPending = false;
    this.externalDecisionPending = false;
    this.externalReloadSeq = 0;
    this.externalChange = NO_EXTERNAL_CHANGE;
    this.abort = new AbortController();
    this.buildDOM();
    this.graph = new ProcessGraph(this.stageHost, this.model.graph(), {
      ariaLabel: `Process template editor: ${this.model.template.id}`,
      colorScheme: 'dark',
      onNodeClick: (e) => this.onNodeClick(e),
      onNodeDblClick: (e) => this.onNodeDblClick(e),
      onEdgeClick: (e) => this.onEdgeClick(e),
      onCanvasClick: () => this.setSelection(null),
      onMarqueeSelect: (e) => this.setSelection(e.selection),
      onNodeDragStart: (e) => this.setSelection(e.selection),
      onNodeDrag: (e) => this.onNodeDrag(e),
      onPortDragStart: (e) => this.onPortDragStart(e),
      onPortDragMove: (e) => this.onPortDragMove(e),
      onPortDragEnd: (e) => this.onPortDragEnd(e),
      onCanvasDrop: (e) => this.onCanvasDrop(e),
      marqueeSelect: true,
      wheelPan: true,
    });
    // Live validation (TCL-299): debounced POST /v1/process/validate on every
    // model mutation, inline badges + issues panel. Constructed after the
    // graph so its initial diagnostics paint can decorate it.
    this.validation = new LiveValidation(this, options.validation || {});
    this.bindEditorEvents();
    this.updateChrome();
    // Test/automation handle (dashsnap drives states through this; not an API).
    this.mount.__processEditor = this;
  }

  // ---- DOM ---------------------------------------------------------------

  buildDOM() {
    this.statusLine = h('span', { class: 'process-editor-status', role: 'status' });
    this.dirtyBadge = h('span', { class: 'process-editor-dirty', text: '● modified', hidden: '' });
    this.versionBadge = h('span', { class: 'process-hash process-editor-version' });
    this.idInput = h('input', {
      class: 'process-editor-id-input', type: 'text', spellcheck: 'false',
      placeholder: 'template-id', 'aria-label': 'Template id',
    });
    this.idInput.value = this.model.template.id || '';
    this.titleLabel = h('strong', { class: 'process-editor-title' });
    this.identity = h('span', { class: 'process-editor-identity' });

    this.undoButton = h('button', { class: 'process-action', type: 'button', title: 'Undo (Ctrl+Z)', text: '↶ undo' });
    this.redoButton = h('button', { class: 'process-action', type: 'button', title: 'Redo (Ctrl+Shift+Z)', text: '↷ redo' });
    this.settingsButton = h('button', { class: 'process-action', type: 'button', title: 'Edit template name and description', text: 'template settings…' });
    this.paramsButton = h('button', { class: 'process-action', type: 'button', title: 'Declare template parameters', text: 'params…' });
    this.instantiateButton = h('button', { class: 'process-action', type: 'button', title: 'Instantiate this exact saved version', text: 'instantiate…' });
    this.commandsButton = h('button', {
      class: 'process-action', type: 'button',
      title: 'Process commands (Ctrl/Cmd-K)', 'aria-label': 'Open process commands (Ctrl/Cmd-K)',
      text: '⌘K commands',
    });
    this.scribeButton = h('button', { class: 'process-action process-scribe-action', type: 'button', title: 'Open an agent scoped to this exact process template' },
      h('span', { class: 'process-scribe-plain', text: 'Edit with agent' }),
      h('span', { class: 'process-scribe-wizard', text: 'Consult a process scribe' }));
    this.paletteButton = h('button', { class: 'process-action', type: 'button', title: 'Toggle the node palette', text: '⬒ palette' });
    this.saveButton = h('button', { class: 'process-action primary', type: 'button', title: 'Save a new version', text: 'Save' });

    const header = h('div', { class: 'process-editor-header' },
      this.identity,
      this.versionBadge,
      this.dirtyBadge,
      this.statusLine,
      h('span', { class: 'spacer' }),
      this.settingsButton, this.paramsButton, this.undoButton, this.redoButton, this.paletteButton, this.commandsButton, this.scribeButton, this.instantiateButton, this.saveButton,
    );

    this.externalMessage = h('span', { class: 'process-editor-external-message', text: 'Template changed externally (new version)' });
    this.externalReloadButton = h('button', { class: 'process-action primary', type: 'button', text: 'Reload' });
    this.externalKeepButton = h('button', { class: 'process-action', type: 'button', text: 'Keep editing' });
    this.externalBanner = h('div', {
      class: 'process-editor-external', role: 'status', hidden: '',
    }, this.externalMessage, h('span', { class: 'spacer' }), this.externalReloadButton, this.externalKeepButton);

    this.palette = this.buildPalette();
    this.stageHost = h('div', { class: 'process-editor-canvas-host' });
    this.inlineInput = h('input', {
      class: 'process-editor-inline-input', type: 'text', spellcheck: 'false', hidden: '',
    });
    this.stage = h('div', { class: 'process-editor-stage' }, this.stageHost, this.inlineInput);
    this.body = h('div', { class: 'process-editor-body' }, this.palette, this.stage);

    this.inspector = h('div', { class: 'process-editor-inspector' });
    this.root = h('div', { class: 'process-editor' }, header, this.externalBanner, this.body, this.inspector);
    this.mount.replaceChildren(this.root);
    this.mount.classList.add('process-editor-mount');
  }

  buildPalette() {
    const card = (payload, label, hint) => {
      const el = h('div', {
        class: 'process-palette-card',
        draggable: 'true',
        title: hint || '',
        'data-palette-item': JSON.stringify(payload),
      }, h('span', { class: 'process-palette-card-label', text: label }),
      h('span', { class: 'process-palette-card-hint', text: hint || '' }));
      return el;
    };
    const primitives = PALETTE_PRIMITIVES.map((p) => card({ kind: 'primitive', type: p.type }, p.label, p.hint));
    const snippets = PALETTE_SNIPPETS.map((s) => card({ kind: 'snippet', key: s.key }, s.label, s.hint));
    return h('aside', { class: 'process-editor-palette', 'aria-label': 'Node palette' },
      h('div', { class: 'process-palette-section', text: 'Primitives' }),
      ...primitives,
      h('div', { class: 'process-palette-section', text: 'Snippets' }),
      ...snippets,
      h('p', { class: 'process-palette-help', text: 'Drag onto the canvas to add. Drag a port to another node to connect.' }),
    );
  }

  bindEditorEvents() {
    const signal = this.abort.signal;
    this.saveButton.addEventListener('click', () => this.save(), { signal });
    this.instantiateButton.addEventListener('click', () => this.requestInstantiate(), { signal });
    this.commandsButton.addEventListener('click', () => requestCommandPalette(), { signal });
    this.scribeButton.addEventListener('click', () => this.requestScribe(), { signal });
    this.externalReloadButton.addEventListener('click', () => this.reloadExternalChange(), { signal });
    this.externalKeepButton.addEventListener('click', () => this.keepExternalChange(), { signal });
    this.undoButton.addEventListener('click', () => this.applyHistory('undo'), { signal });
    this.redoButton.addEventListener('click', () => this.applyHistory('redo'), { signal });
    this.settingsButton.addEventListener('click', () => this.setSelection({ type: 'template' }), { signal });
    this.paramsButton.addEventListener('click', () => this.openParamsSettings(), { signal });
    this.paletteButton.addEventListener('click', () => {
      this.palette.hidden = !this.palette.hidden;
    }, { signal });
    if (this.blank) {
      this.idInput.addEventListener('change', () => {
        if (this.savePending || externalInteractionPending(this)) {
          this.idInput.value = this.model.template.id || '';
          return;
        }
        if (!this.model.setTemplateID(this.idInput.value.trim())) {
          this.idInput.value = this.model.template.id || '';
          this.status('Template id is fixed once an existing version is selected.', true);
        }
        this.updateChrome();
      }, { signal });
    }

    this.palette.addEventListener('dragstart', (event) => {
      const card = event.target.closest?.('.process-palette-card');
      if (!card) return;
      // Custom MIME only — see the module header.
      event.dataTransfer.setData(PALETTE_MIME, card.getAttribute('data-palette-item'));
      event.dataTransfer.effectAllowed = 'copy';
      this.paletteDragPayload = card.getAttribute('data-palette-item');
      card.classList.add('is-dragging');
    }, { signal });
    this.palette.addEventListener('dragend', (event) => {
      this.paletteDragPayload = null;
      event.target.closest?.('.process-palette-card')?.classList.remove('is-dragging');
    }, { signal });

    // Node-move commit: the graph core intentionally snaps a dragged node back
    // on release (position ownership lives here). Its pointerup handler runs
    // first (bound earlier on the same svg), then this one commits the pin.
    this.graph.svg.addEventListener('pointerup', () => this.commitPendingMove(), { signal });
    this.graph.svg.addEventListener('pointercancel', () => {
      this.pendingMove = null;
      this.removeBand();
    }, { signal });

    this.root.addEventListener('keydown', (event) => this.onEditorKeyDown(event), { signal });
  }

  destroy() {
    // Invalidate any delayed save completion before tearing down its DOM and
    // callbacks. Fetch is not tied to the event-listener AbortController, so
    // the request generation is the authoritative stale-response guard.
    this.saveSeq += 1;
    this.externalReloadSeq += 1;
    this.savePending = false;
    this.externalReloadPending = false;
    this.externalDecisionPending = false;
    this.abort.abort();
    this.closeInline(false);
    this.validation?.destroy();
    this.validation = null;
    this.graph.destroy();
    // Parent teardown follows an already-approved navigation/unmount. It is
    // the one forced-close path; user-driven modal replacement goes through
    // requestClose below so a dirty node draft cannot disappear silently.
    this.modalDispose?.(null);
    delete this.mount.__processEditor;
    this.mount.classList.remove('process-editor-mount');
    this.mount.replaceChildren();
  }

  get dirty() {
    return this.model.dirty || !!this.modalDispose?.isDirty?.();
  }

  // ---- chrome ------------------------------------------------------------

  refresh({ fit = false } = {}) {
    // decorate() re-anchors the last known diagnostics on the fresh graph
    // (badges for deleted targets drop immediately); schedule() debounces the
    // next validation round for the mutated draft.
    const graph = this.validation ? this.validation.decorate(this.model.graph()) : this.model.graph();
    this.graph.setGraph(graph, { fit });
    // setGraph re-renders the SVG; re-project the semantic editor selection so
    // undo/redo and mutations cannot leave a stale highlight behind.
    this.setSelection(this.selection);
    this.validation?.schedule();
    this.updateChrome();
  }

  updateChrome() {
    const { model } = this;
    if (this.externalChange?.ref) {
      this.externalChange = reconcileExternalChange(this.externalChange, {
        loadedRef: model.currentRef, loadedSourceHash: model.sourceHash,
        currentRef: this.externalChange.ref, currentSourceHash: this.externalChange.sourceHash, dirty: this.dirty,
      });
    }
    this.titleLabel.textContent = model.template.name
      ? `${model.template.name} (${model.template.id})`
      : model.template.id || 'untitled';
    // A force retry pins the identity as soon as it adopts an existing CAS
    // head. It stays locked even if the retry fails or re-conflicts: `blank`
    // alone is not enough to decide that the id is still editable.
    const showIDInput = templateIDEditable(this.blank, model.sourceHash);
    const externalPending = externalInteractionPending(this);
    const idEditable = showIDInput && !this.savePending && !externalPending;
    this.idInput.disabled = !idEditable;
    this.identity.replaceChildren(showIDInput ? this.idInput : this.titleLabel);
    this.versionBadge.textContent = model.semanticHash ? `v ${shortHash(model.semanticHash)}` : 'unsaved';
    this.versionBadge.title = model.semanticHash || 'This template has never been saved';
    this.dirtyBadge.hidden = !model.dirty;
    this.undoButton.disabled = externalPending || !model.canUndo;
    this.redoButton.disabled = externalPending || !model.canRedo;
    if (this.settingsButton) this.settingsButton.disabled = externalPending;
    if (this.paramsButton) this.paramsButton.disabled = externalPending || this.savePending;
    if (this.instantiateButton) this.instantiateButton.disabled = externalPending || this.savePending;
    if (this.scribeButton) this.scribeButton.disabled = externalPending || this.savePending;
    if (this.paletteButton) this.paletteButton.disabled = externalPending;
    // A blank editor has not completed a save, even if a force retry adopted
    // an existing CAS head. Keep its retry path armed after a failed or
    // cancelled retry; only a successfully loaded/saved clean editor is done.
    this.saveButton.disabled = this.savePending || externalPending || (!model.dirty && !this.blank);
    this.renderExternalChange?.();
    this.renderInspector();
  }

  renderExternalChange() {
    if (!this.externalBanner) return;
    const visible = this.externalChange.kind === 'clean' || this.externalChange.kind === 'dirty';
    const externalPending = externalInteractionPending(this);
    this.externalBanner.hidden = !visible;
    this.externalBanner.classList.toggle('is-dirty', this.externalChange.kind === 'dirty');
    this.externalKeepButton.hidden = this.externalChange.kind !== 'dirty';
    this.externalReloadButton.disabled = externalPending || this.savePending;
    this.externalKeepButton.disabled = externalPending || this.savePending;
    if (this.body) this.body.inert = externalPending;
    if (this.inspector) this.inspector.inert = externalPending;
    this.root?.classList.toggle('is-reloading', externalPending);
  }

  observeExternalHead({ ref: currentRef, sourceHash: currentSourceHash } = {}) {
    this.externalChange = reconcileExternalChange(this.externalChange, {
      loadedRef: this.model.currentRef, loadedSourceHash: this.model.sourceHash,
      currentRef, currentSourceHash, dirty: this.dirty,
    });
    this.renderExternalChange();
    return this.externalChange;
  }

  keepExternalChange() {
    if (externalInteractionPending(this)) return false;
    this.externalChange = keepExternalChange(this.externalChange);
    this.renderExternalChange();
  }

  retainLiveSelection() {
    if (this.selection?.type === 'template') return;
    this.selection = makeSelection(selectionItems(this.selection).filter((item) => item.type === 'node'
      ? this.model.node(item.id) : this.model.findEdge(item.from, item.outcome)));
  }

  async reloadExternalChange() {
    const targetRef = this.externalChange.ref;
    const targetSourceHash = this.externalChange.sourceHash;
    if (!targetRef || !targetSourceHash || externalInteractionPending(this) || this.savePending) return false;
    const decision = {
      editor: this,
      model: this.model,
      ref: this.model.currentRef,
      sourceHash: this.model.sourceHash,
      rev: this.model.rev,
      modal: this.modalDispose,
      inline: this.inlineCommit,
      targetRef,
      targetSourceHash,
    };
    const decisionCurrent = () => decision.editor === this
      && !this.abort.signal.aborted
      && this.model === decision.model
      && decision.model.currentRef === decision.ref
      && decision.model.sourceHash === decision.sourceHash
      && decision.model.rev === decision.rev
      && this.modalDispose === decision.modal
      && this.inlineCommit === decision.inline
      && this.externalChange.ref === decision.targetRef
      && this.externalChange.sourceHash === decision.targetSourceHash
      && !this.savePending;
    if (this.dirty) {
      this.externalDecisionPending = true;
      this.updateChrome?.();
      let accepted = false;
      try {
        accepted = await (this.options.confirmDiscard?.() ?? false);
      } catch (error) {
        if (!this.abort.signal.aborted) this.status(`Reload confirmation failed: ${error.message}`, true);
      }
      if (!accepted || !decisionCurrent()) {
        this.externalDecisionPending = false;
        if (!this.abort.signal.aborted) this.updateChrome?.();
        return false;
      }
    }
    if (!decisionCurrent()) return false;
    // A dirty node-dialog draft belongs to the old model. The shared discard
    // confirmation above approved its loss, so close it before swapping models.
    decision.modal?.(null);
    if (this.modalDispose === decision.modal) this.modalDispose = null;
    if (decision.inline) this.closeInline?.(false);
    this.pendingMove = null;
    this.removeBand?.();
    const guardedModel = this.model;
    const guardedRev = guardedModel.rev;
    const guardedModal = this.modalDispose;
    const guardedInline = this.inlineCommit;
    const requestSeq = ++this.externalReloadSeq;
    this.externalDecisionPending = false;
    this.externalReloadPending = true;
    this.updateChrome?.();
    try {
      const view = await fetchEditView(guardedModel.template.id);
      if (requestSeq !== this.externalReloadSeq || this.abort.signal.aborted) return false;
      if (this.model !== guardedModel || guardedModel.rev !== guardedRev || this.savePending
          || this.modalDispose !== guardedModal || this.inlineCommit !== guardedInline
          || this.pendingMove || this.band) {
        this.status('Reload cancelled because the editor changed while the new version was loading.');
        return false;
      }
      this.model = new ProcessEditModel(view, this.model.config);
      this.blank = false;
      this.retainLiveSelection();
      // ProcessGraph#setGraph keeps its current pan/zoom when fit is false;
      // refresh replays any still-live semantic selection and focused node.
      this.externalChange = NO_EXTERNAL_CHANGE;
      this.refresh();
      this.validation?.applyDiagnostics(view.diagnostics || []);
      this.status(`Reloaded external version ${shortHash(view.semanticHash)}.`);
      return true;
    } catch (error) {
      if (requestSeq === this.externalReloadSeq && !this.abort.signal.aborted) this.status(`Reload failed: ${error.message}`, true);
      return false;
    } finally {
      if (requestSeq === this.externalReloadSeq) {
        this.externalReloadPending = false;
        this.updateChrome?.();
      }
    }
  }

  status(message, isError = false) {
    this.statusLine.textContent = message || '';
    this.statusLine.classList.toggle('is-error', !!isError);
  }

  // ---- selection + inspector ----------------------------------------------

  // laidEdge resolves an edge in the CORE's layout by its semantic identity.
  // The layout mints its own display ids (an "id:" prefix over the input id),
  // so matching on from/outcome — which the layout spreads through — is the
  // only stable lookup.
  laidEdge(from, outcome) {
    return this.graph.layout.edges.find((edge) => edge.from === from && edge.outcome === outcome);
  }

  setSelection(selection) {
    // Template metadata is editor chrome, not a graph entity. Keep it outside
    // process-selection's node/edge-only normalization while explicitly
    // clearing the canvas highlight. A refresh replays this same branch;
    // every node/edge/canvas gesture calls setSelection with another value and
    // therefore leaves template settings cleanly.
    if (selection?.type === 'template') {
      this.selection = { type: 'template' };
      this.graph.select(null);
      this.renderInspector();
      return;
    }
    this.selection = makeSelection(selectionItems(selection));
    const graphical = selectionItems(this.selection).map((item) => {
      if (item.type === 'node') return { type: 'node', id: item.id };
      const laid = this.laidEdge(item.from, item.outcome);
      return laid ? { type: 'edge', id: laid.id } : null;
    }).filter(Boolean);
    this.graph.select(makeSelection(graphical));
    this.renderInspector();
  }

  renderInspector() {
    const parts = [];
    const sel = this.selection;
    const selected = selectionItems(sel);
    if (sel?.type === 'template') {
      parts.push(h('span', { class: 'process-inspector-kind', text: 'template' }));
      const idInput = h('input', {
        class: 'process-inspector-input process-template-id-locked', type: 'text',
        value: this.model.template.id || '', disabled: '',
        title: 'Template ids are immutable after creation', 'aria-label': 'Template id (immutable)',
      });
      const nameInput = h('input', {
        class: 'process-inspector-input', type: 'text', spellcheck: 'false',
        placeholder: 'display name', 'aria-label': 'Template display name',
      });
      nameInput.value = this.model.template.name || '';
      nameInput.addEventListener('change', () => {
        this.mutate(() => this.model.setTemplateMeta({ name: nameInput.value.trim() }));
      });
      const descriptionInput = h('input', {
        class: 'process-inspector-input process-template-description', type: 'text', spellcheck: 'true',
        placeholder: 'description', 'aria-label': 'Template description',
      });
      descriptionInput.value = this.model.template.description || '';
      descriptionInput.addEventListener('change', () => {
        this.mutate(() => this.model.setTemplateMeta({ description: descriptionInput.value.trim() }));
      });
      const docInput = h('textarea', {
        class: 'process-inspector-input process-template-doc', rows: '2', spellcheck: 'true',
        placeholder: 'documentation', 'aria-label': 'Template documentation',
      });
      docInput.value = this.model.template.doc || '';
      docInput.addEventListener('change', () => {
        this.mutate(() => this.model.setTemplateMeta({ doc: docInput.value.trim() }));
      });
      parts.push(idInput, nameInput, descriptionInput, docInput);
    } else if (selected.length > 1) {
      const nodeCount = selected.filter((item) => item.type === 'node').length;
      const edgeCount = selected.length - nodeCount;
      parts.push(h('span', { class: 'process-inspector-kind', text: 'multiple selection' }));
      parts.push(h('span', { class: 'process-inspector-id', text: `${selected.length} items` }));
      parts.push(h('span', { class: 'process-inspector-hint', text: [
        nodeCount ? `${nodeCount} node${nodeCount === 1 ? '' : 's'}` : '',
        edgeCount ? `${edgeCount} edge${edgeCount === 1 ? '' : 's'}` : '',
      ].filter(Boolean).join(' · ') }));
      parts.push(h('button', {
        class: 'process-action process-action-danger', type: 'button', text: 'delete selection',
        onclick: () => this.deleteSelection(),
      }));
    } else if (sel?.type === 'node' && this.model.node(sel.id)) {
      const node = this.model.node(sel.id);
      parts.push(h('span', { class: 'process-inspector-kind', text: `${node.type || 'task'} node` }));
      parts.push(h('span', { class: 'process-inspector-id', text: sel.id }));
      const labelInput = h('input', {
        class: 'process-inspector-input', type: 'text', spellcheck: 'false',
        placeholder: 'label', 'aria-label': 'Node label',
      });
      labelInput.value = node.name || '';
      labelInput.addEventListener('change', () => {
        this.mutate(() => this.model.renameNode(sel.id, labelInput.value.trim()));
      });
      parts.push(labelInput);
      if (this.model.incomingEdges(sel.id).length > 1) {
        const joinSelect = h('select', { class: 'process-inspector-select', 'aria-label': 'Join semantics' },
          h('option', { value: '', text: 'join: unset' }),
          h('option', { value: 'all', text: 'join: all' }),
          h('option', { value: 'any', text: 'join: any' }));
        joinSelect.value = node.metadata?.join || '';
        joinSelect.addEventListener('change', () => {
          this.mutate(() => this.model.setJoin(sel.id, joinSelect.value || null));
        });
        parts.push(joinSelect);
      }
      if (this.model.template.start !== sel.id && node.type !== 'end') {
        parts.push(h('button', {
          class: 'process-action', type: 'button', text: 'set as start',
          title: 'Make this node the process entry point',
          onclick: () => this.mutate(() => this.model.setStart(sel.id)),
        }));
      }
      parts.push(h('button', {
        class: 'process-action', type: 'button', text: 'node settings…',
        title: 'Open the structured node editor: stages, performers, retry, captures',
        onclick: () => this.openNodeSettings(sel.id),
      }));
      parts.push(h('button', {
        class: 'process-action process-action-danger', type: 'button', text: 'delete node',
        onclick: () => this.deleteSelection(),
      }));
    } else if (sel?.type === 'edge' && this.model.findEdge(sel.from, sel.outcome)) {
      const edge = this.model.findEdge(sel.from, sel.outcome);
      parts.push(h('span', { class: 'process-inspector-kind', text: 'edge' }));
      parts.push(h('span', { class: 'process-inspector-id', text: `${edge.from} → ${edge.to}` }));
      const outcomeInput = h('input', {
        class: 'process-inspector-input', type: 'text', spellcheck: 'false',
        placeholder: 'outcome', 'aria-label': 'Edge outcome label',
      });
      outcomeInput.value = edge.outcome;
      outcomeInput.addEventListener('change', () => {
        this.renameEdgeOutcome(edge.from, edge.outcome, outcomeInput.value.trim());
      });
      parts.push(outcomeInput);
      parts.push(h('button', {
        class: 'process-action process-action-danger', type: 'button', text: 'delete edge',
        onclick: () => this.deleteSelection(),
      }));
    } else {
      parts.push(h('span', { class: 'process-inspector-hint', text: 'Select a node or edge to edit it. Double-click a node to open its stage editor.' }));
    }
    this.inspector.replaceChildren(...parts);
  }

  // ---- graph hooks ---------------------------------------------------------

  onNodeClick({ node, event }) {
    if (!node) return;
    const item = { type: 'node', id: node.id };
    this.setSelection(event?.shiftKey || event?.ctrlKey || event?.metaKey
      ? toggleSelection(this.selection, item) : item);
  }

  // Double-click is the logical-zoom gesture (design §8a): zoom into the
  // node's structured editing surface. In-place rename stays available via
  // the inspector's label input.
  onNodeDblClick({ node }) {
    if (!node) return;
    this.setSelection({ type: 'node', id: node.id });
    this.openNodeSettings(node.id);
  }

  async openParamsSettings() {
    if (externalInteractionPending(this) || this.savePending) return false;
    const current = this.modalDispose;
    if (current) {
      const closed = current.requestClose ? await current.requestClose() : (current(null), true);
      if (!closed || this.abort?.signal.aborted) return false;
    }
    const dispose = openProcessParamsDialog({
      model: this.model,
      onMutated: () => this.refresh(),
      onClosed: () => { if (this.modalDispose === dispose) this.modalDispose = null; },
      confirmDiscard: this.options.confirmDiscard,
    });
    this.modalDispose = dispose;
    return true;
  }

  async requestInstantiate() {
    if (externalInteractionPending(this) || this.savePending) return false;
    if (this.blank || this.dirty || !this.model.currentRef) {
      const choice = await this.choiceModal({
        title: 'Save before instantiating',
        body: 'Runs can only pin a saved, content-addressed template version. Save these changes first; unsaved editor state is never instantiated.',
        choices: [{ key: 'save', label: 'Save first', primary: true }],
      });
      if (choice !== 'save' || this.abort.signal.aborted) return false;
      const saved = await this.save();
      // Edits made while the save was in flight deliberately leave the model
      // dirty; requiring another click is what makes unsaved instantiation
      // impossible by construction.
      if (!saved || this.dirty || !this.model.currentRef) {
        if (saved && this.dirty) this.status('The editor changed while saving. Save the latest changes before instantiating.', true);
        return false;
      }
    }
    if (typeof this.options.onInstantiate !== 'function') return false;
    this.options.onInstantiate({
      id: this.model.template.id,
      ref: this.model.currentRef,
      template: structuredClone(this.model.template),
    });
    return true;
  }

  // openNodeSettings opens the shared node dialog (TCL-298). The TCL-296
  // editability seam decides the mode: a node the view may not edit renders
  // the exact same component read-only — the viewer's detail card.
  async openNodeSettings(nodeId) {
    if (externalInteractionPending(this)) return false;
    if (!this.model.node(nodeId)) return false;
    const current = this.modalDispose;
    if (current) {
      const closed = current.requestClose
        ? await current.requestClose()
        : (current(null), true);
      if (!closed || this.abort?.signal.aborted) return false;
    }
    if (!this.model.node(nodeId)) return false;
    const mode = this.model.config.nodeEditable(nodeId) ? 'edit' : 'view';
    const dispose = openNodeDialog({
      model: this.model,
      nodeId,
      mode,
      onMutated: () => this.refresh(),
      onClosed: () => {
        if (this.modalDispose === dispose) this.modalDispose = null;
      },
      confirmDiscard: this.options.confirmDiscard,
    });
    this.modalDispose = dispose;
    return true;
  }

  onEdgeClick({ edge, event }) {
    if (!edge) return;
    const item = { type: 'edge', from: edge.from, outcome: edge.outcome };
    const already = selectionContains(this.selection, item);
    const additive = event?.shiftKey || event?.ctrlKey || event?.metaKey;
    this.setSelection(additive ? toggleSelection(this.selection, item) : item);
    // Second click on an already-selected edge edits the outcome label in place.
    if (already && !additive) this.openInlineOutcomeEdit(edge.from, edge.outcome);
  }

  onNodeDrag({ nodeId, nodeIds = [nodeId], delta }) {
    if (!this.pendingMove || this.pendingMove.id !== nodeId) {
      const starts = nodeIds.map((id) => this.graph.layout.nodes.find((candidate) => candidate.id === id))
        .filter(Boolean).map((node) => ({ id: node.id, x: node.x, y: node.y }));
      if (!starts.length) return;
      this.pendingMove = { id: nodeId, starts, delta };
    }
    this.pendingMove.delta = delta;
  }

  commitPendingMove() {
    const move = this.pendingMove;
    this.pendingMove = null;
    if (!move || !move.delta) return;
    // The core's own click-vs-drag threshold is 3 CLIENT px; the delta is in
    // graph units, so scale by the zoom before comparing — at high zoom a
    // small visible drag is a tiny graph-unit delta and must still commit.
    if (Math.hypot(move.delta.x, move.delta.y) * this.graph.view.k <= 3) return;
    this.mutate(() => this.model.moveNodes(move.starts.map((start) => ({
      id: start.id, x: start.x + move.delta.x, y: start.y + move.delta.y,
    }))));
  }

  // ---- edge drawing (rubber band) -------------------------------------------

  portPoint(nodeId, port) {
    const laid = this.graph.layout.nodes.find((candidate) => candidate.id === nodeId);
    if (!laid) return { x: 0, y: 0 };
    return { x: laid.x, y: laid.y + (port === 'in' ? -laid.height / 2 : laid.height / 2) };
  }

  onPortDragStart({ nodeId, port, point }) {
    this.removeBand();
    const start = this.portPoint(nodeId, port);
    const band = document.createElementNS(SVG_NS, 'path');
    band.setAttribute('class', 'process-editor-band');
    band.setAttribute('fill', 'none');
    band.setAttribute('d', `M ${start.x} ${start.y} L ${point.x} ${point.y}`);
    // The viewport keeps extra children across the core's layer re-renders,
    // so the band survives mid-drag refreshes and pans with the view.
    this.graph.viewport.append(band);
    this.band = { element: band, start, source: { nodeId, port } };
  }

  onPortDragMove({ point }) {
    if (!this.band) return;
    this.band.element.setAttribute('d', `M ${this.band.start.x} ${this.band.start.y} L ${point.x} ${point.y}`);
  }

  onPortDragEnd({ nodeId, port, targetNodeId, targetPort, cancelled }) {
    const source = this.band?.source || { nodeId, port };
    this.removeBand();
    if (cancelled || !targetNodeId) return;
    // A plain CLICK on a port arrives here too (the core starts a port drag on
    // pointerdown and hit-tests on pointerup): source and target are the same
    // port. Never treat that as an edge gesture — without this, clicking an
    // out port silently minted a pass self-loop. A deliberate out → own-in
    // drop still creates a self-loop edge.
    if (targetNodeId === source.nodeId && targetPort === source.port) return;
    // Direction: an out-port drag connects source → target; an in-port drag
    // dropped on an out port (or a node body) connects target → source.
    let from = source.nodeId;
    let to = targetNodeId;
    if (source.port === 'in') {
      if (targetPort === 'in') {
        this.status('Connect an output to an input: one end must be an out port.', true);
        return;
      }
      from = targetNodeId;
      to = source.nodeId;
    }
    if (from === to) {
      // Released back on the source's own body: a fumbled click, stay silent.
      if (!targetPort) return;
      // v1 processes are acyclic — a hand-drawn self-loop is always a
      // graph_cycle ERROR, and advisory saves would ship it silently. Block
      // the gesture outright (the model refuses too, belt and braces).
      this.status('Self-loop edges are not supported (v1 processes are acyclic).', true);
      return;
    }
    const outcome = this.model.freeOutcome(from, 'pass');
    const created = this.mutate(() => this.model.addEdge(from, outcome, to));
    if (!created) return;
    this.setSelection({ type: 'edge', from, outcome });
    this.openInlineOutcomeEdit(from, outcome);
  }

  removeBand() {
    this.band?.element?.remove();
    this.band = null;
  }

  // ---- palette drop ----------------------------------------------------------

  onCanvasDrop({ point, event }) {
    let raw = event?.dataTransfer?.getData?.(PALETTE_MIME) || '';
    if (!raw && this.paletteDragPayload) raw = this.paletteDragPayload;
    if (!raw) return;
    let payload;
    try { payload = JSON.parse(raw); } catch { return; }
    if (payload.kind === 'primitive') {
      this.addNodeType(payload.type, point);
    } else if (payload.kind === 'snippet') {
      const snippet = PALETTE_SNIPPETS.find((candidate) => candidate.key === payload.key);
      if (!snippet) return;
      const idMap = this.mutate(() => this.model.insertSnippet(snippet, { x: point.x, y: point.y }));
      if (idMap) this.status(`Inserted snippet ${snippet.label} (${idMap.size} nodes).`);
    }
  }

  canvasCenterPoint() {
    const rect = this.graph.svg.getBoundingClientRect();
    return this.graph.clientToGraph(rect.left + rect.width / 2, rect.top + rect.height / 2);
  }

  addNodeType(type, point = this.canvasCenterPoint()) {
    const id = this.mutate(() => this.model.addNode(type, { x: point.x, y: point.y }));
    if (!id) return false;
    this.setSelection({ type: 'node', id });
    this.status(`Added ${type} node ${id}.`);
    return id;
  }

  editSelection() {
    if (this.selection?.type === 'template') {
      this.setSelection({ type: 'template' });
      queueMicrotask(() => this.inspector.querySelector('input:not(:disabled), textarea:not(:disabled)')?.focus());
      return true;
    }
    const items = selectionItems(this.selection);
    if (items.length !== 1) return false;
    const item = items[0];
    if (item.type === 'node') return this.openNodeSettings(item.id);
    const laid = this.laidEdge(item.from, item.outcome);
    const anchor = laid?.label || this.portPoint(item.from, 'out');
    return this.openInline(anchor.x, anchor.y, item.outcome, (value) => {
      this.renameEdgeOutcome(item.from, item.outcome, value);
    });
  }

  duplicateSelection() {
    const items = selectionItems(this.selection);
    if (!items.length || items.some((item) => item.type !== 'node')) return false;
    const positions = Object.fromEntries(items.map((item) => {
      const node = this.graph.layout.nodes.find((candidate) => candidate.id === item.id);
      return [item.id, node ? { x: node.x, y: node.y } : undefined];
    }));
    const idMap = this.mutate(() => this.model.duplicateNodes(items.map((item) => item.id), { positions }));
    if (!idMap?.size) return false;
    this.setSelection(makeSelection([...idMap.values()].map((id) => ({ type: 'node', id }))));
    this.status(`Duplicated ${idMap.size} node${idMap.size === 1 ? '' : 's'}.`);
    return idMap;
  }

  selectAll() {
    const items = [
      ...Object.keys(this.model.template.nodes).map((id) => ({ type: 'node', id })),
      ...this.model.edges.filter((edge) => edge.from).map((edge) => ({ type: 'edge', from: edge.from, outcome: edge.outcome })),
    ];
    this.setSelection(makeSelection(items));
    return items.length > 0;
  }

  clearSelection() {
    if (!this.selection) return false;
    this.setSelection(null);
    return true;
  }

  fitGraph() {
    this.graph.fitToView();
    return true;
  }

  centerSelection() {
    const points = selectionItems(this.selection).map((item) => {
      if (item.type === 'node') return this.graph.layout.nodes.find((node) => node.id === item.id);
      const edge = this.laidEdge(item.from, item.outcome);
      return edge?.label || this.graph.layout.nodes.find((node) => node.id === item.from);
    }).filter(Boolean);
    if (!points.length) return false;
    this.graph.centerOn(
      points.reduce((sum, point) => sum + point.x, 0) / points.length,
      points.reduce((sum, point) => sum + point.y, 0) / points.length,
    );
    return true;
  }

  zoomGraph(factor) {
    return this.graph.zoomBy(factor);
  }

  resetZoom() {
    return this.graph.resetZoom();
  }

  validateNow() {
    // The issues panel owns validation progress/results. A persistent editor
    // status here would outlive both successful and skipped/failed rounds.
    return this.validation?.validateNow() || false;
  }

  focusIssue(delta) {
    return this.validation?.focusIssue(delta) || false;
  }

  commandContext() {
    const pending = externalInteractionPending(this);
    const selected = selectionItems(this.selection).filter((item) => item.type === 'node'
      ? !!this.model.node(item.id) : !!this.model.findEdge(item.from, item.outcome));
    const templateSelected = this.selection?.type === 'template';
    const hasSelection = templateSelected || selected.length > 0;
    const one = selected.length === 1 ? selected[0] : null;
    const oneEditable = one?.type === 'node'
      ? this.model.config.nodeEditable(one.id)
      : one?.type === 'edge' ? this.model.config.edgeEditable(this.model.findEdge(one.from, one.outcome)) : false;
    const selectedNodes = selected.filter((item) => item.type === 'node');
    const selectedNodeIDs = new Set(selectedNodes.map((item) => item.id));
    const selectedEdgeKeys = new Set(selected.filter((item) => item.type === 'edge')
      .map((item) => `${item.from}\u0000${item.outcome}`));
    const affectedEdges = this.model.edges.filter((edge) => selectedNodeIDs.has(edge.from)
      || selectedNodeIDs.has(edge.to) || selectedEdgeKeys.has(`${edge.from}\u0000${edge.outcome}`));
    const deletionEditable = selected.every((item) => item.type === 'node'
      ? this.model.config.nodeEditable(item.id) : this.model.config.edgeEditable(this.model.findEdge(item.from, item.outcome)))
      && affectedEdges.every((edge) => this.model.config.edgeEditable(edge));
    const busyReason = pending ? 'An external template reload is in progress.' : '';
    const issueCount = this.validation?.mapped?.entries?.length || 0;
    const id = (this.model.template.id || '').trim();
    return {
      hasGraph: Object.keys(this.model.template.nodes).length > 0,
      hasSelection,
      hasGraphSelection: selected.length > 0,
      canCreate: !pending && this.model.config.canInsert,
      createReason: busyReason || 'Adding nodes is not allowed in this view.',
      canEdit: !pending && (templateSelected || (!!one && oneEditable)),
      editReason: busyReason || (!hasSelection ? 'Select one item first.' : selected.length > 1 ? 'Edit one item at a time.' : 'The selected item is read-only.'),
      canDuplicate: !pending && this.model.config.canInsert && selectedNodes.length > 0 && selectedNodes.length === selected.length,
      duplicateReason: busyReason || (!hasSelection ? 'Select one or more nodes first.' : 'Only node selections can be duplicated.'),
      canDelete: !pending && selected.length > 0 && deletionEditable,
      deleteReason: busyReason || (!hasSelection ? 'Select graph items first.' : 'The selection includes read-only graph items.'),
      canValidate: !pending && !!this.validation,
      validateReason: busyReason || 'Validation is not available.',
      issueCount,
      canSave: !pending && !this.savePending && !!id && (this.model.dirty || this.blank),
      saveReason: busyReason || (this.savePending ? 'A save is already in progress.' : !id ? 'Enter a template id first.' : 'There are no unsaved changes.'),
      canInstantiate: !pending && !this.savePending && typeof this.options.onInstantiate === 'function',
      instantiateReason: busyReason || (this.savePending ? 'Wait for the save to finish.' : 'Run creation is not available in this context.'),
    };
  }

  // ---- edit ops ----------------------------------------------------------------

  // mutate wraps a model mutation: refresh + chrome on success, status line on
  // rejection (duplicate outcome, read-only node, …). Returns the mutation's
  // result, or undefined when rejected.
  mutate(operation, { fit = false } = {}) {
    if (externalInteractionPending(this)) {
      this.status('Wait for the external reload to finish before editing.');
      return undefined;
    }
    let result;
    try {
      result = operation();
    } catch (error) {
      this.status(error.message, true);
      return undefined;
    }
    this.status('');
    this.refresh({ fit });
    return result === undefined ? true : result;
  }

  applyHistory(direction) {
    if (externalInteractionPending(this)) return false;
    const moved = direction === 'undo' ? this.model.undo() : this.model.redo();
    if (!moved) return;
    // Template settings remain valid across metadata history. Graph selections
    // still need liveness filtering because a restored topology may no longer
    // contain their node/edge.
    if (this.selection?.type !== 'template') {
      this.selection = makeSelection(selectionItems(this.selection).filter((item) => item.type === 'node'
        ? this.model.node(item.id) : this.model.findEdge(item.from, item.outcome)));
    }
    this.refresh();
  }

  renameEdgeOutcome(from, oldOutcome, newOutcome) {
    if (!newOutcome || newOutcome === oldOutcome) return;
    const ok = this.mutate(() => this.model.setEdgeOutcome(from, oldOutcome, newOutcome));
    if (ok) this.setSelection({ type: 'edge', from, outcome: newOutcome });
  }

  async deleteSelection() {
    if (externalInteractionPending(this)) return false;
    const items = selectionItems(this.selection).filter((item) => item.type === 'node'
      ? this.model.node(item.id) : this.model.findEdge(item.from, item.outcome));
    if (!items.length) return;
    const nodes = items.filter((item) => item.type === 'node');
    const midGraph = nodes.filter((item) => this.model.incomingEdges(item.id).length
      && this.model.outgoingEdges(item.id).length);
    const subject = items.length === 1
      ? (items[0].type === 'node' ? `node ${items[0].id}` : 'selected edge')
      : `${items.length} selected items`;
    const choices = midGraph.length ? [
      { key: 'rewire', label: 'Delete + rewire through', primary: true },
      { key: 'drop', label: 'Delete + drop edges', danger: true },
    ] : [{ key: 'drop', label: 'Delete selection', danger: true, initialFocus: true }];
    const choice = await this.choiceModal({
      title: `Delete ${subject}?`,
      body: midGraph.length
        ? `${midGraph.length} selected node${midGraph.length === 1 ? '' : 's'} connect incoming and outgoing edges.`
        : 'This removes the current highlighted selection. You can undo this change afterward.',
      choices,
    });
    if (!choice || externalInteractionPending(this)) return false;
    this.mutate(() => this.model.deleteItems(items, { rewire: choice === 'rewire' }));
    this.setSelection(null);
  }

  onEditorKeyDown(event) {
    const inInput = isProcessEditorFormControl(event.target);
    if ((event.ctrlKey || event.metaKey) && !inInput) {
      const key = event.key.toLowerCase();
      if (key === 'z' && !event.shiftKey) {
        event.preventDefault();
        this.applyHistory('undo');
        return;
      }
      if ((key === 'z' && event.shiftKey) || key === 'y') {
        event.preventDefault();
        this.applyHistory('redo');
        return;
      }
    }
    if ((event.key === 'Delete' || event.key === 'Backspace') && !inInput && this.selection) {
      event.preventDefault();
      this.deleteSelection();
    }
  }

  // ---- inline (in-place) label editing ------------------------------------------

  stagePosition(x, y) {
    const svgRect = this.graph.svg.getBoundingClientRect();
    const stageRect = this.stage.getBoundingClientRect();
    return {
      left: svgRect.left - stageRect.left + this.graph.view.x + x * this.graph.view.k,
      top: svgRect.top - stageRect.top + this.graph.view.y + y * this.graph.view.k,
    };
  }

  openInline(x, y, value, commit) {
    if (externalInteractionPending(this)) return false;
    this.closeInline(false);
    const input = this.inlineInput;
    const position = this.stagePosition(x, y);
    input.style.left = `${Math.round(position.left)}px`;
    input.style.top = `${Math.round(position.top)}px`;
    input.value = value;
    input.hidden = false;
    this.inlineCommit = commit;
    const done = (apply) => this.closeInline(apply);
    this.inlineHandlers = {
      keydown: (event) => {
        if (event.key === 'Enter') { event.preventDefault(); done(true); }
        if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); done(false); }
      },
      blur: () => done(true),
    };
    input.addEventListener('keydown', this.inlineHandlers.keydown);
    input.addEventListener('blur', this.inlineHandlers.blur);
    input.focus();
    input.select();
  }

  closeInline(apply) {
    const input = this.inlineInput;
    if (!input || input.hidden) return;
    const commit = this.inlineCommit;
    if (this.inlineHandlers) {
      input.removeEventListener('keydown', this.inlineHandlers.keydown);
      input.removeEventListener('blur', this.inlineHandlers.blur);
    }
    this.inlineCommit = null;
    this.inlineHandlers = null;
    input.hidden = true;
    if (apply && commit) commit(input.value.trim());
  }

  openInlineOutcomeEdit(from, outcome) {
    const laid = this.laidEdge(from, outcome);
    const anchor = laid?.label || this.portPoint(from, 'out');
    this.openInline(anchor.x, anchor.y, outcome, (value) => {
      this.renameEdgeOutcome(from, outcome, value);
    });
  }

  // ---- save + conflict -----------------------------------------------------------

  async save() {
    const id = (this.model.template.id || '').trim();
    if (!id) {
      this.status('Template id is required before saving.', true);
      return false;
    }
    if (this.savePending || externalInteractionPending(this)) return false;
    const requestSeq = ++this.saveSeq;
    this.savePending = true;
    this.updateChrome();
    try {
      await this.saveRequest(requestSeq);
      return true;
    } catch (error) {
      if (requestSeq === this.saveSeq) this.status(`Save failed: ${error.message}`, true);
      return false;
    } finally {
      if (requestSeq === this.saveSeq) {
        this.savePending = false;
        this.updateChrome();
      }
    }
  }

  async requestScribe() {
    if (!this.options.onScribe || this.savePending || externalInteractionPending(this)) return false;
    const originalBlank = this.blank;
    if (this.dirty) {
      const choice = await this.choiceModal({
        title: 'Resolve unsaved edits before handing off',
        body: 'A process scribe edits canonical state outside this buffer. Save these edits first, discard them explicitly, or cancel the handoff.',
        choices: [
          { key: 'discard', label: 'Discard local edits', danger: true },
          { key: 'save', label: 'Save changes first', primary: true },
        ],
      });
      if (choice === 'save') {
        if (!(await this.save()) || this.dirty) return false;
      } else if (choice === 'discard') {
        let view;
        try {
          const id = (this.model.template.id || '').trim();
          if (originalBlank && !this.model.sourceHash) {
            view = blankEditView(id);
          } else {
            const guardedModel = this.model;
            const guardedRev = guardedModel.rev;
            const guardedModal = this.modalDispose;
            const guardedInline = this.inlineCommit;
            const requestSeq = ++this.externalReloadSeq;
            this.externalReloadPending = true;
            this.updateChrome?.();
            try {
              view = await fetchEditView(id);
              if (requestSeq !== this.externalReloadSeq || this.abort.signal.aborted) return false;
              if (this.model !== guardedModel || guardedModel.rev !== guardedRev || this.savePending
                  || this.modalDispose !== guardedModal || this.inlineCommit !== guardedInline
                  || this.pendingMove || this.band) {
                this.status('Scribe handoff cancelled because the editor changed while canonical state was loading.');
                return false;
              }
            } finally {
              if (requestSeq === this.externalReloadSeq) {
                this.externalReloadPending = false;
                this.updateChrome?.();
              }
            }
          }
          this.model = new ProcessEditModel(view, this.model.config);
          this.blank = originalBlank && !view.sourceHash;
          this.selection = null;
          this.validation?.applyDiagnostics(view.diagnostics || []);
          this.refresh({ fit: true });
          this.status('Discarded local edits before opening the scribe.');
        } catch (error) {
          this.status(`Could not discard local edits safely: ${error.message}`, true);
          return false;
        }
      } else {
        return false;
      }
    }
    const id = (this.model.template.id || '').trim();
    return !!(await this.options.onScribe({
      kind: 'template', id,
      currentRef: this.model.currentRef || '', sourceHash: this.model.sourceHash || '',
      isNew: this.blank && !this.model.sourceHash,
    }));
  }

  async saveRequest(requestSeq) {
    if (requestSeq !== this.saveSeq) return;
    const id = (this.model.template.id || '').trim();
    const savedID = id;
    // The canvas stays interactive during the POST: capture the rev the
    // payload was built at, so edits made in flight keep the model dirty.
    const savedAtRev = this.model.rev;
    const response = await fetch(`/v1/process/templates/${encodeURIComponent(id)}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(this.model.saveBody()),
    });
    const body = await response.json().catch(() => ({}));
    // A newer editor request/lifecycle generation owns the model now. Never
    // let this completion overwrite its identity, CAS base, or status.
    if (requestSeq !== this.saveSeq) return;
    if (response.status === 409 && body.code === 'process_template_conflict') {
      await this.resolveConflict(body, requestSeq);
      return;
    }
    if (!response.ok) {
      this.status(body.message || body.error || `${response.status} ${response.statusText}`, true);
      return;
    }
    // The POST path is the creation-time identity. Discard any draft id
    // change made while the request was in flight before locking the model;
    // history restoration also preserves this pinned id.
    this.model.template.id = savedID;
    this.idInput.value = savedID;
    this.model.markSaved(body, savedAtRev);
    // Sync the validation controller with the save verdict: a failed
    // debounced round deliberately keeps prior diagnostics, so without this
    // the badges/panel stay stale until the next mutation. The follow-up
    // schedule() re-validates the live draft in case edits landed while the
    // POST was in flight (its seq guard drops any out-of-order result).
    this.validation?.applyDiagnostics(body.diagnostics || []);
    this.validation?.schedule();
    this.blank = false;
    const diagCount = (body.diagnostics || []).length;
    this.status(`Saved version ${shortHash(body.semanticHash)}${diagCount ? ` · ${diagCount} advisory finding${diagCount === 1 ? '' : 's'}` : ''}.`);
    this.updateChrome();
    this.options.onSaved?.(body);
  }

  // resolveConflict is the explicit 409 dialog (never a silent overwrite):
  // reload their head version (discarding local edits), or save as a new
  // version on top of theirs (rebasing this draft's CAS base).
  async resolveConflict(conflict, requestSeq = this.saveSeq) {
    const firstSave = !this.model.sourceHash;
    const choice = await this.choiceModal({
      title: firstSave ? 'Template id already exists' : 'Template changed while you were editing',
      body: firstSave
        ? `A template named ${this.model.template.id} already exists (head ${shortHash(conflict.currentSourceHash)}). Saving anyway stacks a new version on top of it.`
        : `${conflict.error || 'The template head moved.'} Their head is now ${shortHash(conflict.currentSourceHash)}.`,
      choices: [
        { key: 'reload', label: 'Reload their version (discard mine)' },
        { key: 'force', label: 'Save as new version anyway', primary: true },
      ],
    });
    if (requestSeq !== this.saveSeq) return;
    if (choice === 'force') {
      this.model.sourceHash = conflict.currentSourceHash || '';
      await this.saveRequest(requestSeq);
    } else if (choice === 'reload') {
      try {
        const view = await fetchEditView(this.model.template.id);
        if (requestSeq !== this.saveSeq) return;
        this.model = new ProcessEditModel(view, this.model.config);
        this.blank = false;
        this.selection = null;
        this.refresh({ fit: true });
        // The model swap replaced this.model.diagnostics; without an explicit
        // sync the validation controller keeps the OLD model's set until a
        // network round happens to succeed.
        this.validation?.applyDiagnostics(view.diagnostics || []);
        this.status(`Reloaded their version ${shortHash(view.semanticHash)}.`);
      } catch (error) {
        if (requestSeq !== this.saveSeq) return;
        this.status(`Reload failed: ${error.message}`, true);
      }
    }
  }

  // choiceModal: a promise-based dialog on the shared .modal-overlay styling,
  // owned per-editor (the global #confirm-modal singleton only offers two
  // fixed buttons). Escape / backdrop resolve null.
  async choiceModal({ title, body, choices }) {
    const current = this.modalDispose;
    if (current) {
      const closed = current.requestClose
        ? await current.requestClose()
        : (current(null), true);
      if (!closed || this.abort?.signal.aborted) return null;
    }
    return new Promise((resolve) => {
      // Fully dispose any previous dialog (resolving its promise null) so its
      // capture-phase document keydown listener never outlives its overlay —
      // the confirm-modal singleton double-listener disease, avoided by
      // construction.
      const buttons = choices.map((choice) => h('button', {
        class: `${choice.primary ? 'primary ' : ''}${choice.danger ? 'confirm-danger ' : ''}process-editor-modal-btn`,
        type: 'button', text: choice.label,
      }));
      const cancel = h('button', { type: 'button', text: 'Cancel', class: 'process-editor-modal-btn' });
      const overlay = h('div', { class: 'modal-overlay show process-editor-modal' },
        h('div', { class: 'modal', role: 'dialog', 'aria-modal': 'true' },
          h('h3', { text: title }),
          h('p', { text: body }),
          h('div', { class: 'modal-buttons' }, cancel, ...buttons)));
      const done = (result) => {
        overlay.remove();
        document.removeEventListener('keydown', onKey, true);
        if (this.modalDispose === done) this.modalDispose = null;
        resolve(result);
      };
      const onKey = (event) => {
        if (event.key !== 'Escape') return;
        event.preventDefault();
        event.stopImmediatePropagation();
        done(null);
      };
      buttons.forEach((button, index) => button.addEventListener('click', () => done(choices[index].key)));
      cancel.addEventListener('click', () => done(null));
      overlay.addEventListener('click', (event) => { if (event.target === overlay) done(null); });
      document.addEventListener('keydown', onKey, true);
      this.modalDispose = done;
      document.body.append(overlay);
      (buttons.find((_, index) => choices[index].initialFocus || choices[index].primary) || cancel).focus();
    });
  }
}

// openTemplateEditor mounts the editor into `mount` for a template id (or a
// blank scaffold). Returns the editor instance; throws on fetch errors so the
// caller can render its own error state.
export async function openTemplateEditor(mount, { id, blank = false, version, config = {} } = {}) {
  // Destroy the previous instance BEFORE fetching: on a fetch failure the
  // caller renders an error into the mount, and a live ghost editor handle
  // must not keep gating navigation with its stale dirty state.
  mount.__processEditor?.destroy?.();
  const view = blank ? blankEditView(id) : await fetchEditView(id, version);
  return new ProcessTemplateEditor(mount, view, { ...config, blank });
}
