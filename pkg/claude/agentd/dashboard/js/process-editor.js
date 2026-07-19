// process-editor.js -- the template editor surface for the Processes tab
// (TCL-296): full-canvas ProcessGraph + palette dock + edit ops + save/CAS.
//
// Split of responsibilities:
//   - process-graph.js owns presentation + pointer mechanics (hooks only).
//   - process-edit-model.js owns the pure edit model + undo/redo (node-tested).
//   - process-editor-island.js owns the Preact chrome and form DOM.
//   - This controller translates semantic events into model mutations and REST.
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
// Template content is untrusted at render time. Preact renders it as text; no
// editor value is accepted as HTML.

import { createProcessGraphAdapter } from './process-graph-adapter.js';
import {
  ProcessEditModel, blankEditView,
  PALETTE_SNIPPETS,
} from './process-edit-model.js';
import { edgePinTitle } from './process-edge-hint.js';
import { defaultPinned, edgeLabelVisible } from './process-outcome-vocabulary.js';
import { processEdgePortAvailability } from './process-port-availability.js';
import {
  ProcessClipboardError, createProcessSelectionPayload,
  isProcessSelectionClipboardText, parseProcessSelection,
  processSelectionFingerprint, serializeProcessSelection,
  validateProcessSelectionPayload,
} from './process-editor-clipboard.js';
import { LiveValidation } from './process-validation.js';
import {
  NO_EXTERNAL_CHANGE, attachExternalReview, keepExternalChange, reconcileExternalChange,
  sameTemplateGeneration, templateHeadFromEditView,
} from './process-external-change.js';
import {
  makeSelection, selectionContains, selectionItems, toggleSelection,
} from './process-selection.js';
import { requestCommandPalette } from './command-registry.js';
import { openProcessNodeTypeChooser } from './process-node-chooser.js';
import {
  prepareProcessConnectionFeedback, resolveProcessConnectionFeedback,
} from './process-connection-feedback.js';
import { PROCESS_NODE_TYPES } from './process-node-types.js';
import {
  processScribeContextPreview, processScribeEditorContext,
  processScribeHandoff, processScribePrompt,
} from './process-scribe.js';
import {
  createProcessSnippet, deleteProcessSnippet, loadProcessSnippets, renameProcessSnippet,
} from './process-snippet-library.js';

// Custom drag payload MIME (dock-dnd idiom): withholding text/plain keeps
// every other document-level DnD feature out of a palette drag.
const PALETTE_MIME = 'application/x-tclaude-process-palette';
const EXTERNAL_REVIEW_TIMEOUT_MS = 15_000;
// 1/1024 graph unit is at most 0.0035 CSS px at the supported maximum zoom.
// Treating targets inside this epsilon as equal absorbs harmless subpixel
// SVGRect/view arithmetic without creating a perceptible cursor snap.
export const PROCESS_PASTE_TARGET_EPSILON = 1 / 1024;

export function resolveProcessPastePlacement(fingerprint, target, previous = {}) {
  const anchor = previous.anchor;
  const sameTarget = Number.isFinite(target?.x) && Number.isFinite(target?.y)
    && Number.isFinite(anchor?.x) && Number.isFinite(anchor?.y)
    && Math.abs(target.x - anchor.x) <= PROCESS_PASTE_TARGET_EPSILON
    && Math.abs(target.y - anchor.y) <= PROCESS_PASTE_TARGET_EPSILON;
  const repeated = fingerprint === previous.fingerprint && sameTarget;
  return {
    center: repeated ? { x: anchor.x, y: anchor.y } : { x: target.x, y: target.y },
    repeat: repeated ? (Number(previous.repeat) || 0) + 1 : 0,
  };
}

export function isProcessEditorFormControl(target) {
  const element = target?.nodeType === 1 ? target : target?.parentElement || target;
  const tag = String(element?.tagName || '').toUpperCase();
  if (tag === 'INPUT' || tag === 'SELECT' || tag === 'TEXTAREA') return true;
  if (element?.isContentEditable) return true;
  return !!element?.closest?.('[contenteditable]:not([contenteditable="false"]), [role="textbox"], .cm-editor, .monaco-editor');
}

export function hasNonCollapsedDOMSelection(event) {
  try {
    const selection = event?.view?.getSelection?.() || globalThis.window?.getSelection?.();
    return selection?.isCollapsed === false;
  } catch {
    // If the host selection cannot be inspected, preserve native copy rather
    // than risk replacing user-highlighted text with a graph payload.
    return true;
  }
}

function externalInteractionPending(editor) {
  return !!(editor.externalDecisionPending || editor.externalReloadPending);
}

function graphInteraction(editor) {
  return editor.graph?.interactionSnapshot?.() || { generation: 0, active: false };
}

function cancelExternalReview(editor) {
  const request = editor.externalReviewRequest;
  if (!request) return false;
  editor.externalReviewRequest = null;
  editor.externalReviewPending = false;
  editor.externalReviewSeq += 1;
  request.controller.abort();
  return true;
}

function scribeSelectionIdentity(selection) {
  return JSON.stringify(selectionItems(selection).map((item) => item?.type === 'node'
    ? ['node', String(item.id || '')]
    : ['edge', String(item?.from || ''), String(item?.outcome || '')]).sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b))));
}

function shortHash(hash) {
  return hash ? hash.slice(0, 8) : '';
}

async function fetchEditView(id, version, { signal } = {}) {
  const query = new URLSearchParams({ authorship: 'omit' });
  if (version) query.set('version', version);
  const response = await fetch(`/v1/process/templates/${encodeURIComponent(id)}?${query}`, { signal });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
  return body;
}

export class ProcessTemplateEditor {
  constructor(mount, view, options = {}) {
    this.mount = mount;
    this.options = options;
    // Public command methods must be harmless after destroy(): they may not
    // mutate editor/model state, touch the disposed graph/UI, start network
    // or modal work, or invoke outward callbacks. Guards check this flag.
    this.destroyed = false;
    this.model = new ProcessEditModel(view, {
      mode: options.mode || 'template',
      nodeEditable: options.nodeEditable,
      edgeEditable: options.edgeEditable,
      canInsert: options.canInsert,
    });
    this.loadedView = structuredClone(view);
    this.blank = !!options.blank;
    this.selection = null;
    this.band = null;
    this.nodeChooserDispose = null;
    this.savePending = false;
    this.saveSeq = 0;
    this.externalReloadPending = false;
    this.externalDecisionPending = false;
    this.externalReloadSeq = 0;
    this.externalReviewSeq = 0;
    this.externalReviewPending = false;
    this.externalReviewRequest = null;
    this.externalChange = NO_EXTERNAL_CHANGE;
    this.externalReviewOpen = false;
    this.paletteHidden = false;
    this.customSnippets = [];
    this.snippetLibrary = { loading: true, error: '', generation: 0, pendingID: '', creating: false };
    this.snippetLoadSeq = 0;
    this.statusState = { message: '', error: false };
    this.inlineState = { open: false, token: 0, left: 0, top: 0, value: '' };
    this.inspectorFocusRequest = 0;
    this.modalState = null;
    this.modalGeneration = 0;
    this.modalHandle = null;
    // Only the bounded fingerprint + repeat count survive a paste event. Raw
    // clipboard bytes are never stored on the editor, logged, or published.
    this.pasteFingerprint = '';
    this.pasteRepeat = 0;
    this.pasteAnchor = null;
    // Ephemeral observation only: never published, persisted, or included in
    // model/clipboard state. The live graph bounds revalidate it at paste.
    this.canvasPointer = null;
    this.abort = new AbortController();
    this.graph = null;
    if (!options.ui?.createPublisher || !options.ui?.mount) {
      throw new Error('process editor requires its Preact UI boundary');
    }
    this.snapshotSignal = options.ui.createPublisher(this.snapshot());
    this.mount.classList.add('process-editor-mount');
    this.uiCleanup = options.ui.mount(this.mount, this);
    if (!this.graph) throw new Error('process editor graph host did not mount');
    // Live validation (TCL-299): debounced POST /v1/process/validate on every
    // model mutation, inline badges + issues panel. Constructed after the
    // graph so its initial diagnostics paint can decorate it.
    this.validation = new LiveValidation(this, options.validation || {});
    this.updateChrome();
    void this.loadCustomSnippets();
    // Test/automation handle (dashsnap drives states through this; not an API).
    this.mount.__processEditor = this;
  }

  attachGraphHost(host) {
    if (this.destroyed) return;
    if (!host) {
      this.canvasPointer = null;
      this.graph?.dispose?.();
      this.graph = null;
      return;
    }
    if (this.graph?.host === host) return;
    this.canvasPointer = null;
    this.graph?.dispose?.();
    this.graph = createProcessGraphAdapter(host, {
      graph: this.model.graph(),
      ariaLabel: `Process template editor: ${this.model.template.id}`,
      interactionLayering: true,
      connectionFeedback: (request, prepared) => resolveProcessConnectionFeedback(this.model, request, prepared),
      connectionFeedbackPreparation: () => prepareProcessConnectionFeedback(this.model),
      events: {
        nodeClick: (event) => this.onNodeClick(event),
        nodeDoubleClick: (event) => this.onNodeDblClick(event),
        edgeClick: (event) => this.onEdgeClick(event),
        canvasClick: () => this.setSelection(null),
        // The pin toggle is an HTML overlay anchored in host pixels, so it has
        // to be re-resolved whenever the viewport moves under it.
        viewportChange: () => { if (this.edgePinOpen()) this.publish(); },
        marqueeSelection: (event) => this.setSelection(event.selection),
        nodeDragStart: (event) => this.setSelection(event.selection),
        nodeDragEnd: (event) => this.commitNodeDrag(event),
        nodeDragCancel: () => {},
        portDragStart: (event) => this.onPortDragStart(event),
        portDragEnd: (event) => this.onPortDragEnd(event),
        canvasDrop: (event) => this.onCanvasDrop(event),
        canvasPointerMove: (event) => this.onCanvasPointerMove(event),
        canvasPointerLeave: () => this.onCanvasPointerLeave(),
      },
    });
  }

  snapshot() {
    const model = this.model;
    const { review: exactExternalReview, ...externalChange } = this.externalChange;
    const selectedNode = this.selection?.type === 'node' ? model.node(this.selection.id) : null;
    const selectedEdge = this.selection?.type === 'edge'
      ? model.findEdge(this.selection.from, this.selection.outcome) : null;
    const actorDescription = this.options.describeActor?.(this.externalChange.actor) || null;
    return {
      blank: this.blank,
      selection: structuredClone(this.selection),
      selectedNode: structuredClone(selectedNode || null),
      selectedEdge: structuredClone(selectedEdge || null),
      selectedNodeIncoming: selectedNode ? model.incomingEdges(this.selection.id).length : 0,
      model: {
        id: model.template.id || '', name: model.template.name || '',
        description: model.template.description || '', doc: model.template.doc || '',
        start: model.template.start || '', sourceHash: model.sourceHash || '',
        semanticHash: model.semanticHash || '', currentRef: model.currentRef || '',
        dirty: model.dirty, canUndo: model.canUndo, canRedo: model.canRedo,
      },
      pending: {
        save: this.savePending,
        externalDecision: this.externalDecisionPending,
        externalReload: this.externalReloadPending,
      },
      status: { ...this.statusState },
      paletteHidden: this.paletteHidden,
      snippets: {
        ...this.snippetLibrary,
        items: this.customSnippets.map(({ payload, ...metadata }) => ({ ...metadata })),
        canInsert: this.model.config.canInsert && !externalInteractionPending(this),
        canSaveSelection: this.canSaveSelectionAsSnippet(),
      },
      inline: { ...this.inlineState },
      edgePin: this.edgePinView(),
      inspectorFocusRequest: this.inspectorFocusRequest,
      external: {
        ...structuredClone(externalChange),
        // The exact view remains private controller state for Apply Update.
        // Only its bounded summary crosses into the Preact/DOM snapshot, which
        // avoids cloning a near-limit node ID on every unrelated render.
        ...(exactExternalReview ? { review: { summary: structuredClone(exactExternalReview.summary) } } : {}),
        actorDescription,
        reviewOpen: this.externalReviewOpen,
        reviewPending: this.externalReviewPending,
      },
      issues: this.validation?.panelSnapshot?.() || {
        open: false, hidden: true, summary: 'Issues · none', entries: [],
        issueCursor: -1, focusRequest: 0,
      },
      modal: this.modalState ? { ...this.modalState } : null,
    };
  }

  publish() {
    if (this.destroyed || !this.snapshotSignal) return;
    this.snapshotSignal.value = this.snapshot();
  }

  openModal(descriptor, resolve = null) {
    if (this.destroyed) {
      // The island can no longer render this modal; resolve immediately so
      // awaiting callers observe a cancellation instead of hanging forever.
      resolve?.(null);
      const dispose = () => false;
      dispose.isDirty = () => false;
      dispose.requestClose = async () => true;
      return dispose;
    }
    const generation = ++this.modalGeneration;
    this.modalState = { ...descriptor, generation };
    this.modalHandle = null;
    const dispose = (result = null) => this.finishModal(generation, result);
    dispose.isDirty = () => !!(this.modalState?.generation === generation && this.modalHandle?.isDirty?.());
    dispose.requestClose = async () => {
      if (this.modalState?.generation !== generation) return true;
      if (this.modalHandle?.requestClose) return this.modalHandle.requestClose();
      dispose(null);
      return true;
    };
    dispose.resolve = resolve;
    this.modalDispose = dispose;
    this.publish();
    return dispose;
  }

  registerModalHandle(generation, handle) {
    if (this.modalState?.generation !== generation) return () => {};
    this.modalHandle = handle;
    return () => { if (this.modalHandle === handle) this.modalHandle = null; };
  }

  finishModal(generation, result = null) {
    if (this.modalState?.generation !== generation) return false;
    const dispose = this.modalDispose;
    this.modalState = null;
    this.modalHandle = null;
    this.modalDispose = null;
    this.publish();
    dispose?.resolve?.(result);
    return true;
  }

  destroy() {
    if (this.destroyed) return;
    this.destroyed = true;
    // Invalidate delayed completions and cancel the bounded review request
    // before tearing down the editor's DOM and callbacks.
    this.saveSeq += 1;
    this.externalReloadSeq += 1;
    this.externalReviewSeq += 1;
    this.savePending = false;
    this.externalReloadPending = false;
    this.externalDecisionPending = false;
    this.externalReviewPending = false;
    cancelExternalReview(this);
    this.abort.abort();
    this.nodeChooserDispose?.();
    this.nodeChooserDispose = null;
    this.closeInline(false);
    this.validation?.destroy();
    this.validation = null;
    this.graph?.dispose?.();
    this.graph = null;
    this.canvasPointer = null;
    // Parent teardown follows an already-approved navigation/unmount. It is
    // the one forced-close path; user-driven modal replacement goes through
    // requestClose below so a dirty node draft cannot disappear silently.
    this.modalDispose?.(null);
    delete this.mount.__processEditor;
    this.uiCleanup?.();
    this.uiCleanup = null;
    this.mount.classList.remove('process-editor-mount');
  }

  get dirty() {
    return this.model.dirty || !!this.modalDispose?.isDirty?.();
  }

  // ---- chrome ------------------------------------------------------------

  setTemplateID(value) {
    if (this.destroyed) return false;
    if (this.savePending || externalInteractionPending(this)) return false;
    if (!this.model.setTemplateID(String(value || '').trim())) {
      this.status('Template id is fixed once an existing version is selected.', true);
      return false;
    }
    this.updateChrome();
    return true;
  }

  setTemplateMeta(fields) {
    const clean = Object.fromEntries(Object.entries(fields || {})
      .map(([key, value]) => [key, String(value || '').trim()]));
    return this.mutate(() => this.model.setTemplateMeta(clean));
  }

  renameNode(id, name) { return this.mutate(() => this.model.renameNode(id, String(name || '').trim())); }
  setJoin(id, join) { return this.mutate(() => this.model.setJoin(id, join)); }
  setStart(id) { return this.mutate(() => this.model.setStart(id)); }
  togglePalette() { if (this.destroyed) return; this.paletteHidden = !this.paletteHidden; this.publish?.(); }
  openCommands() { if (this.destroyed) return; requestCommandPalette(); }
  openExternalActor() { if (this.destroyed) return; this.options.onOpenActor?.(this.externalChange.actor); }
  setIssuesOpen(open) { this.validation?.setPanelOpen(open); }
  focusIssueAt(index, focusButton = false) { return this.validation?.focusIssueAt(index, { focusButton }) || false; }

  paletteDragStart(event) {
    const card = event.target.closest?.('.process-palette-card');
    if (!card || card.getAttribute('draggable') !== 'true') {
      event.preventDefault?.();
      return;
    }
    event.dataTransfer.setData(PALETTE_MIME, card.getAttribute('data-palette-item'));
    event.dataTransfer.effectAllowed = 'copy';
    this.paletteDragPayload = card.getAttribute('data-palette-item');
    card.classList.add('is-dragging');
  }

  paletteDragEnd(event) {
    this.paletteDragPayload = null;
    event.target.closest?.('.process-palette-card')?.classList.remove('is-dragging');
  }

  canSaveSelectionAsSnippet() {
    if (!this.model.config.canInsert || this.snippetLibrary.loading || externalInteractionPending(this)) return false;
    return selectionItems(this.selection).some((item) => item.type === 'node' && this.model.node(item.id));
  }

  async loadCustomSnippets() {
    if (this.abort.signal.aborted) return false;
    const requestSeq = ++this.snippetLoadSeq;
    this.snippetLibrary = { ...this.snippetLibrary, loading: true, error: '' };
    this.publish?.();
    try {
      const library = await loadProcessSnippets({ signal: this.abort.signal });
      if (this.abort.signal.aborted || requestSeq !== this.snippetLoadSeq) return false;
      this.customSnippets = library.snippets;
      this.snippetLibrary = { ...this.snippetLibrary, loading: false, error: '', generation: library.generation };
      this.publish?.();
      return true;
    } catch (error) {
      if (this.abort.signal.aborted || requestSeq !== this.snippetLoadSeq) return false;
      this.snippetLibrary = { ...this.snippetLibrary, loading: false, error: error.message || 'Custom snippets could not be loaded.' };
      this.publish?.();
      return false;
    }
  }

  customSnippet(id) { return this.customSnippets.find((snippet) => snippet.id === id) || null; }

  async reconcileCustomSnippetMutation(result, expectedGeneration, patch) {
    if (Number.isSafeInteger(result?.generation) && result.generation === expectedGeneration) {
      this.snippetLoadSeq += 1;
      patch();
      this.snippetLibrary = {
        ...this.snippetLibrary, loading: false, error: '', generation: result.generation,
      };
      return true;
    }
    const loaded = await this.loadCustomSnippets();
    if (!loaded && !this.abort.signal.aborted) {
      // The mutation committed even though the authoritative reconciliation
      // failed. Preserve that known item change in the last-good collection;
      // the visible load error keeps Retry available for missing generations.
      patch();
      this.snippetLibrary = {
        ...this.snippetLibrary, loading: false,
        generation: Number.isSafeInteger(result?.generation)
          ? Math.max(this.snippetLibrary.generation, result.generation) : this.snippetLibrary.generation,
      };
      this.publish?.();
    }
    return loaded;
  }

  async saveSelectionAsSnippet() {
    if (this.destroyed || !this.canSaveSelectionAsSnippet() || this.snippetLibrary.creating) return false;
    const layout = this.graph?.layoutSnapshot?.();
    let envelope;
    try {
      envelope = createProcessSelectionPayload(this.model, this.selection, layout?.nodes || []);
      if (!envelope) throw new ProcessClipboardError('empty', 'Select one or more nodes first.');
    } catch (error) {
      this.status(error.message || 'The selection cannot be saved as a custom snippet.', true);
      return false;
    }
    const name = await this.nameSnippetModal({ title: 'Save selection as custom snippet', submitLabel: 'Save snippet' });
    if (!name || this.abort.signal.aborted) return false;
    const expectedGeneration = this.snippetLibrary.generation + 1;
    this.snippetLibrary = { ...this.snippetLibrary, creating: true };
    this.publish?.();
    try {
      const result = await createProcessSnippet(name, envelope, { signal: this.abort.signal });
      if (!result.snippet || this.abort.signal.aborted) throw new Error('The saved snippet response was invalid.');
      await this.reconcileCustomSnippetMutation(result, expectedGeneration, () => {
        this.customSnippets = [...this.customSnippets.filter((item) => item.id !== result.snippet.id), result.snippet]
          .sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id));
      });
      if (this.abort.signal.aborted) return false;
      this.status(`Saved custom snippet ${result.snippet.name}.`);
      return true;
    } catch (error) {
      if (!this.abort.signal.aborted) {
        this.status(`Custom snippet save failed: ${error.message}`, true);
        void this.loadCustomSnippets();
      }
      return false;
    } finally {
      if (!this.abort.signal.aborted) {
        this.snippetLibrary = { ...this.snippetLibrary, creating: false };
        this.publish?.();
      }
    }
  }

  async renameCustomSnippet(id) {
    if (this.destroyed) return false;
    const snippet = this.customSnippet(id);
    if (!snippet || this.snippetLibrary.pendingID) return false;
    const name = await this.nameSnippetModal({
      title: 'Rename custom snippet', submitLabel: 'Rename snippet', initialName: snippet.name,
    });
    if (!name || name === snippet.name || this.abort.signal.aborted) return false;
    const expectedGeneration = this.snippetLibrary.generation + 1;
    this.snippetLibrary = { ...this.snippetLibrary, pendingID: id };
    this.publish?.();
    try {
      const result = await renameProcessSnippet(snippet, name, { signal: this.abort.signal });
      if (!result.snippet || this.abort.signal.aborted) throw new Error('The renamed snippet response was invalid.');
      await this.reconcileCustomSnippetMutation(result, expectedGeneration, () => {
        this.customSnippets = this.customSnippets.map((item) => item.id === id ? result.snippet : item)
          .sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id));
      });
      if (this.abort.signal.aborted) return false;
      this.status(`Renamed custom snippet to ${result.snippet.name}.`);
      return true;
    } catch (error) {
      if (!this.abort.signal.aborted) {
        this.status(`Custom snippet rename failed: ${error.message}`, true);
        void this.loadCustomSnippets();
      }
      return false;
    } finally {
      if (!this.abort.signal.aborted) {
        this.snippetLibrary = { ...this.snippetLibrary, pendingID: '' };
        this.publish?.();
      }
    }
  }

  async deleteCustomSnippet(id) {
    if (this.destroyed) return false;
    const snippet = this.customSnippet(id);
    if (!snippet || this.snippetLibrary.pendingID) return false;
    const choice = await this.choiceModal({
      title: `Delete custom snippet ${snippet.name}?`,
      body: 'This removes the reusable snippet from the local library. Process templates that used it are unchanged.',
      choices: [{ key: 'delete', label: 'Delete snippet', danger: true, initialFocus: true }],
    });
    if (choice !== 'delete' || this.abort.signal.aborted) return false;
    const expectedGeneration = this.snippetLibrary.generation + 1;
    this.snippetLibrary = { ...this.snippetLibrary, pendingID: id };
    this.publish?.();
    try {
      const result = await deleteProcessSnippet(snippet, { signal: this.abort.signal });
      if (this.abort.signal.aborted) return false;
      await this.reconcileCustomSnippetMutation(result, expectedGeneration, () => {
        this.customSnippets = this.customSnippets.filter((item) => item.id !== id);
      });
      if (this.abort.signal.aborted) return false;
      this.status(`Deleted custom snippet ${snippet.name}.`);
      return true;
    } catch (error) {
      if (!this.abort.signal.aborted) {
        this.status(`Custom snippet delete failed: ${error.message}`, true);
        void this.loadCustomSnippets();
      }
      return false;
    } finally {
      if (!this.abort.signal.aborted) {
        this.snippetLibrary = { ...this.snippetLibrary, pendingID: '' };
        this.publish?.();
      }
    }
  }

  insertCustomSnippet(id, point = this.canvasCenterPoint()) {
    if (this.destroyed) return false;
    const snippet = this.customSnippet(id);
    if (!snippet || !snippet.available || !snippet.payload) {
      this.status('This custom snippet is unavailable and was not inserted.', true);
      return false;
    }
    if (!this.model.config.canInsert || externalInteractionPending(this)) {
      this.status('Inserting snippets is not allowed in this read-only view.', true);
      return false;
    }
    let payload;
    let idMap;
    try {
      payload = validateProcessSelectionPayload(snippet.payload);
      idMap = this.model.insertClipboardSelection(payload,
        { center: point, offset: { x: 0, y: 0 }, operation: 'snippet' });
    } catch (error) {
      this.status(error.message || 'The custom snippet could not be inserted.', true);
      return false;
    }
    this.status('');
    this.refresh();
    const ids = [...idMap.values()];
    this.setSelection(makeSelection(ids.map((nodeID) => ({ type: 'node', id: nodeID }))));
    this.status(`Inserted custom snippet ${snippet.name} (${ids.length} node${ids.length === 1 ? '' : 's'}).`);
    queueMicrotask(() => this.graph?.focusNode?.(ids[0]));
    return idMap;
  }

  insertPaletteItem(payload, point = this.canvasCenterPoint()) {
    if (payload.kind === 'primitive') return this.addNodeType(payload.type, point);
    if (payload.kind === 'custom-snippet') return this.insertCustomSnippet(payload.id, point);
    if (payload.kind !== 'snippet') return false;
    const snippet = PALETTE_SNIPPETS.find((candidate) => candidate.key === payload.key);
    if (!snippet) return false;
    const idMap = this.mutate(() => this.model.insertSnippet(snippet, { x: point.x, y: point.y }));
    if (idMap) this.status(`Inserted snippet ${snippet.label} (${idMap.size} nodes).`);
    return idMap;
  }

  refresh({ fit = false } = {}) {
    if (this.destroyed) return;
    // decorate() re-anchors the last known diagnostics on the fresh graph
    // (badges for deleted targets drop immediately); schedule() debounces the
    // next validation round for the mutated draft.
    const graph = this.validation ? this.validation.decorate(this.model.graph()) : this.model.graph();
    this.graph?.setGraph(graph, { fit });
    // setGraph re-renders the SVG; re-project the semantic editor selection so
    // undo/redo and mutations cannot leave a stale highlight behind.
    this.setSelection(this.selection);
    this.validation?.schedule();
    this.updateChrome();
  }

  updateChrome() {
    if (this.destroyed) return;
    const { model } = this;
    if (this.externalChange?.ref) {
      this.externalChange = reconcileExternalChange(this.externalChange, {
        loadedRef: model.currentRef, loadedSourceHash: model.sourceHash,
        currentRef: this.externalChange.ref, currentSourceHash: this.externalChange.sourceHash,
        actor: this.externalChange.actor, authoredAt: this.externalChange.authoredAt, dirty: this.dirty,
      });
    }
    this.publish?.();
  }

  renderExternalChange() {
    this.publish?.();
  }

  observeExternalHead({ ref: currentRef, sourceHash: currentSourceHash, actor, authoredAt } = {}) {
    if (this.destroyed) return this.externalChange;
    const previous = this.externalChange;
    const observedKey = currentRef && currentSourceHash ? `${currentRef}\n${currentSourceHash}` : '';
    if (this.externalReviewRequest && this.externalReviewRequest.key !== observedKey) cancelExternalReview(this);
    this.externalChange = reconcileExternalChange(this.externalChange, {
      loadedRef: this.model.currentRef, loadedSourceHash: this.model.sourceHash,
      currentRef, currentSourceHash, actor, authoredAt, dirty: this.dirty,
    });
    this.renderExternalChange();
    if ((this.externalChange.kind === 'clean' || this.externalChange.kind === 'dirty')
        && (!sameTemplateGeneration(previous, this.externalChange) || !this.externalChange.review)) {
      void this.loadExternalReview();
    }
    return this.externalChange;
  }

  toggleExternalReview() {
    if (this.destroyed || this.externalReviewPending || externalInteractionPending(this)) return false;
    this.externalReviewOpen = !this.externalReviewOpen;
    if (this.externalReviewOpen && !this.externalChange.review) void this.loadExternalReview();
    this.renderExternalChange();
    return true;
  }

  loadExternalReview() {
    const target = { ref: this.externalChange.ref, sourceHash: this.externalChange.sourceHash };
    if (!target.ref || !target.sourceHash || this.abort.signal.aborted) return Promise.resolve(false);
    const key = `${target.ref}\n${target.sourceHash}`;
    const active = this.externalReviewRequest;
    if (active?.key === key && !active.controller.signal.aborted) return active.promise;
    if (active) cancelExternalReview(this);

    const model = this.model; const loadedRef = model.currentRef; const loadedSourceHash = model.sourceHash;
    const requestSeq = ++this.externalReviewSeq;
    const controller = new AbortController();
    const cancelForTeardown = () => controller.abort();
    this.abort.signal.addEventListener('abort', cancelForTeardown, { once: true });
    const configuredTimeout = Number(this.options.externalReviewTimeoutMs);
    const timeoutMs = Number.isFinite(configuredTimeout) && configuredTimeout > 0
      ? Math.min(configuredTimeout, EXTERNAL_REVIEW_TIMEOUT_MS) : EXTERNAL_REVIEW_TIMEOUT_MS;
    const request = { key, controller, promise: null };
    this.externalReviewRequest = request;
    const timeout = setTimeout(() => {
      if (request === this.externalReviewRequest) {
        cancelExternalReview(this);
        this.renderExternalChange();
      } else {
        controller.abort();
      }
    }, timeoutMs);
    this.externalReviewPending = true;
    this.renderExternalChange();
    request.promise = (async () => {
      try {
        const view = await fetchEditView(model.template.id, undefined, { signal: controller.signal });
        if (request !== this.externalReviewRequest || requestSeq !== this.externalReviewSeq
            || controller.signal.aborted || this.abort.signal.aborted || this.model !== model
            || model.currentRef !== loadedRef || model.sourceHash !== loadedSourceHash
            || !sameTemplateGeneration(target, this.externalChange)) return false;
        // The head poll owns ordering. A GET for an older/newer generation may
        // finish after another save; never let that response replace or clear
        // the polled target. The next bounded poll will coalesce to the latest.
        if (!sameTemplateGeneration(target, view)) return false;
        const head = templateHeadFromEditView(view);
        this.externalChange = reconcileExternalChange(this.externalChange, {
          loadedRef, loadedSourceHash, currentRef: head.ref, currentSourceHash: head.sourceHash,
          actor: head.actor, authoredAt: head.authoredAt, dirty: this.dirty,
        });
        if (this.externalChange.kind !== 'clean' && this.externalChange.kind !== 'dirty') return false;
        this.externalChange = attachExternalReview(this.externalChange, view, this.loadedView);
        return !!this.externalChange.review;
      } catch (error) {
        if (request === this.externalReviewRequest && requestSeq === this.externalReviewSeq
            && !controller.signal.aborted && !this.abort.signal.aborted) {
          this.status(`Change review failed: ${error.message}`, true);
        }
        return false;
      } finally {
        clearTimeout(timeout);
        this.abort.signal.removeEventListener('abort', cancelForTeardown);
        if (request === this.externalReviewRequest && requestSeq === this.externalReviewSeq) {
          this.externalReviewRequest = null;
          this.externalReviewPending = false;
          this.renderExternalChange();
        }
      }
    })();
    return request.promise;
  }

  keepExternalChange() {
    if (this.destroyed || externalInteractionPending(this)) return false;
    this.externalChange = keepExternalChange(this.externalChange);
    this.renderExternalChange();
  }

  retainLiveSelection() {
    if (this.selection?.type === 'template') return;
    this.selection = makeSelection(selectionItems(this.selection).filter((item) => item.type === 'node'
      ? this.model.node(item.id) : this.model.findEdge(item.from, item.outcome)));
  }

  async reloadExternalChange() {
    if (this.destroyed) return false;
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
      interaction: graphInteraction(this),
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
      && !graphInteraction(this).active
      && graphInteraction(this).generation === decision.interaction.generation
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
    const guardedModel = this.model;
    const guardedRev = guardedModel.rev;
    const requestSeq = ++this.externalReloadSeq;
    this.externalDecisionPending = false;
    this.externalReloadPending = true;
    this.updateChrome?.();
    try {
      const reviewed = this.externalChange.review?.view;
      const view = sameTemplateGeneration(this.externalChange, reviewed)
        ? reviewed : await fetchEditView(guardedModel.template.id);
      if (requestSeq !== this.externalReloadSeq || this.abort.signal.aborted) return false;
      if (this.model !== guardedModel || guardedModel.rev !== guardedRev || this.savePending
          || this.modalDispose !== decision.modal || this.inlineCommit !== decision.inline
          || graphInteraction(this).active
          || graphInteraction(this).generation !== decision.interaction.generation) {
        this.status('Reload cancelled because the editor changed while the new version was loading.');
        return false;
      }
      if (!sameTemplateGeneration({ ref: targetRef, sourceHash: targetSourceHash }, this.externalChange)) {
        this.status('Reload cancelled because a newer external version is now available.');
        return false;
      }
      if (!sameTemplateGeneration({ ref: targetRef, sourceHash: targetSourceHash }, view)) {
        this.status('The fetched version no longer matches the polled head. Waiting for the next refresh before applying.');
        return false;
      }
      // A dirty node-dialog draft belongs to the old model. The confirmation
      // approved its loss, but close it only after an exact target view is in
      // hand so a stale/newer response cannot discard local UI state by itself.
      decision.modal?.(null);
      if (this.modalDispose === decision.modal) this.modalDispose = null;
      if (decision.inline) this.closeInline?.(false);
      this.model = new ProcessEditModel(view, this.model.config);
      this.graph?.resetInteractionLayering?.();
      this.loadedView = structuredClone(view);
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
    if (this.destroyed) return;
    this.statusState = { message: message || '', error: !!isError };
    this.publish?.();
  }

  // ---- selection + inspector ----------------------------------------------

  // laidEdge resolves an edge in the CORE's layout by its semantic identity.
  // The layout mints its own display ids (an "id:" prefix over the input id),
  // so matching on from/outcome — which the layout spreads through — is the
  // only stable lookup.
  laidEdge(from, outcome) {
    return this.graph?.layoutSnapshot().edges.find((edge) => edge.from === from && edge.outcome === outcome);
  }

  setSelection(selection) {
    if (this.destroyed) return;
    // Template metadata is editor chrome, not a graph entity. Keep it outside
    // process-selection's node/edge-only normalization while explicitly
    // clearing the canvas highlight. A refresh replays this same branch;
    // every node/edge/canvas gesture calls setSelection with another value and
    // therefore leaves template settings cleanly.
    if (selection?.type === 'template') {
      this.selection = { type: 'template' };
      this.graph?.setSelection?.(null);
      this.publish?.();
      return;
    }
    this.selection = makeSelection(selectionItems(selection));
    const graphical = selectionItems(this.selection).map((item) => {
      if (item.type === 'node') return { type: 'node', id: item.id };
      const laid = this.laidEdge(item.from, item.outcome);
      return laid ? { type: 'edge', id: laid.id } : null;
    }).filter(Boolean);
    this.graph?.setSelection?.(makeSelection(graphical));
    this.publish?.();
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
    if (this.destroyed || externalInteractionPending(this) || this.savePending) return false;
    const current = this.modalDispose;
    if (current) {
      const closed = current.requestClose ? await current.requestClose() : (current(null), true);
      if (!closed || this.abort?.signal.aborted) return false;
    }
    if (!this.snapshotSignal) {
      const { openProcessParamsDialog } = await import('./process-params-dialog.js');
      const dispose = openProcessParamsDialog({
        model: this.model,
        onMutated: () => this.refresh?.(),
        onClosed: () => { if (this.modalDispose === dispose) this.modalDispose = null; },
        confirmDiscard: this.options.confirmDiscard,
      });
      this.modalDispose = dispose;
    } else {
      this.openModal({ kind: 'params' });
    }
    return true;
  }

  async requestInstantiate() {
    if (this.destroyed || externalInteractionPending(this) || this.savePending) return false;
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
      if (!saved || this.destroyed || this.dirty || !this.model.currentRef) {
        if (saved && this.dirty) this.status('The editor changed while saving. Save the latest changes before instantiating.', true);
        return false;
      }
    }
    // The save's onSaved hook may navigate away and destroy this editor;
    // never hand a destroyed editor's identity to the instantiate flow.
    if (this.destroyed || typeof this.options.onInstantiate !== 'function') return false;
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
    if (this.destroyed || externalInteractionPending(this)) return false;
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
    if (!this.snapshotSignal) {
      const { openNodeDialog } = await import('./process-node-dialog.js');
      const dispose = openNodeDialog({
        model: this.model, nodeId, mode,
        onMutated: () => this.refresh?.(),
        onClosed: () => { if (this.modalDispose === dispose) this.modalDispose = null; },
        confirmDiscard: this.options.confirmDiscard,
      });
      this.modalDispose = dispose;
    } else {
      this.openModal({ kind: 'node', nodeId, mode });
    }
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

  commitNodeDrag({ starts, delta, moved = true }) {
    if (this.destroyed || !this.graph) return;
    if (!moved || !starts?.length || !delta) return;
    // The core's own click-vs-drag threshold is 3 CLIENT px; the delta is in
    // graph units, so scale by the zoom before comparing — at high zoom a
    // small visible drag is a tiny graph-unit delta and must still commit.
    if (Math.hypot(delta.x, delta.y) * this.graph.viewSnapshot().k <= 3) return;
    this.mutate(() => this.model.moveNodes(starts.map((start) => ({
      id: start.id, x: start.x + delta.x, y: start.y + delta.y,
    }))));
  }

  // ---- edge drawing (rubber band) -------------------------------------------

  portPoint(nodeId, port) {
    const laid = this.graph?.layoutSnapshot().nodes.find((candidate) => candidate.id === nodeId);
    if (!laid) return { x: 0, y: 0 };
    return { x: laid.x, y: laid.y + (port === 'in' ? -laid.height / 2 : laid.height / 2) };
  }

  onPortDragStart({ nodeId, port, point }) {
    if (this.destroyed) return;
    this.nodeChooserDispose?.();
    this.nodeChooserDispose = null;
    this.band = { source: { nodeId, port } };
  }

  onPortDragEnd({ nodeId, port, point, targetNodeId, targetPort, emptyCanvas, cancelled, event }) {
    if (this.destroyed) return;
    const source = this.band?.source || { nodeId, port };
    this.removeBand();
    if (cancelled) return;
    const feedback = resolveProcessConnectionFeedback(this.model, {
      phase: 'target', source, candidate: { nodeId: targetNodeId, port: targetPort, emptyCanvas },
    });
    if (!targetNodeId) {
      if (emptyCanvas && feedback.state === 'valid') this.openConnectedNodeChooser(source, point, event);
      else if (feedback.state === 'invalid' || feedback.state === 'disabled') this.status(feedback.message, true);
      return;
    }
    // A plain CLICK on a port arrives here too (the core starts a port drag on
    // pointerdown and hit-tests on pointerup): source and target are the same
    // port. Never treat that as an edge gesture — without this, clicking an
    // out port silently minted a pass self-loop. The resolver keeps a
    // deliberate out → own-in drop distinct so it receives the self-loop
    // reason without committing.
    if (feedback.state === 'source') return;
    if (feedback.state !== 'valid') {
      if (feedback.message) this.status(feedback.message, true);
      return;
    }
    const { from, to } = feedback;
    const outcome = this.model.freeOutcome(from, 'pass');
    const created = this.mutate(() => this.model.addEdge(from, outcome, to));
    if (!created) return;
    this.setSelection({ type: 'edge', from, outcome });
    // Open the inline editor only if the new connector's label is actually
    // drawn; otherwise the author would be asked to type into an invisible
    // anchor. Routed through edgeLabelVisible rather than re-deriving the rule,
    // so this cannot drift from what the renderer decided.
    const drawn = edgeLabelVisible({
      outcome,
      siblingCount: this.model.outgoingEdges(from).length,
      nodeType: this.model.node(from)?.type,
      pinned: this.model.edgePinned(from, outcome),
    });
    if (drawn) this.openInlineOutcomeEdit(from, outcome);
  }

  openConnectedNodeChooser(source, point, event) {
    // Only the destroyed guard here: the semantic feedback rejection below is
    // valid without a live graph; the adapter is needed only past that point,
    // to anchor the chooser.
    if (this.destroyed) return false;
    const feedback = resolveProcessConnectionFeedback(this.model, {
      phase: 'target', source, candidate: { emptyCanvas: true },
    });
    if (feedback.state !== 'valid') {
      this.status(feedback.message || 'This connector is not available.', true);
      return false;
    }

    this.nodeChooserDispose?.();
    const anchor = Number.isFinite(event?.clientX) && Number.isFinite(event?.clientY)
      ? this.graph.clientPointToHost({ clientX: event.clientX, clientY: event.clientY }, this.stage)
      : this.graph.graphPointToHost(point, this.stage);
    const dropPoint = { x: point.x, y: point.y };
    // The source connector is this anchored dialog's visible invoker. Give it
    // ownership before opening, then restore that exact element on dismissal.
    // If it disappears while the chooser is open, do not invent a fallback.
    const restoreInvoker = this.graph.capturePortFocus(source.nodeId, source.port);
    const dispose = openProcessNodeTypeChooser({
      host: this.stage,
      anchor: { x: anchor.left, y: anchor.top },
      restoreFocus: restoreInvoker,
      availability: (type) => {
        const candidate = { type };
        const endpoints = source.port === 'in'
          ? [candidate, this.model.node(source.nodeId)]
          : [this.model.node(source.nodeId), candidate];
        const availability = processEdgePortAvailability(...endpoints);
        return availability.enabled ? null : {
          enabled: false, disabledReason: availability.message,
        };
      },
      onChoose: (type) => this.addConnectedNodeType(type, source, dropPoint),
      onClose: () => {
        if (this.nodeChooserDispose === dispose) this.nodeChooserDispose = null;
      },
    });
    this.nodeChooserDispose = dispose;
    return true;
  }

  addConnectedNodeType(type, source, point) {
    if (this.destroyed) return false;
    const connection = source.port === 'in'
      ? { connectTo: source.nodeId } : { connectFrom: source.nodeId };
    const created = this.mutate(() => this.model.addConnectedNode(type, {
      x: point.x, y: point.y, ...connection,
    }));
    if (!created) {
      queueMicrotask(() => {
        if (this.graph && !this.graph.focusPort(source.nodeId, source.port)) {
          this.graph.focusKeyboardTarget();
        }
      });
      return false;
    }
    this.setSelection({ type: 'node', id: created.id });
    this.status(`Added ${type} node ${created.id} and connected it from ${created.edge.from}.`);
    const nodeType = PROCESS_NODE_TYPES.find((candidate) => candidate.type === type);
    if (nodeType?.requiresConfiguration) {
      void this.openNodeSettings(created.id);
    } else {
      queueMicrotask(() => this.graph?.focusNode(created.id));
    }
    return created.id;
  }

  removeBand() {
    this.graph?.endConnectionBand();
    this.band = null;
  }

  // ---- palette drop ----------------------------------------------------------

  onCanvasDrop({ point, event }) {
    let raw = event?.dataTransfer?.getData?.(PALETTE_MIME) || '';
    if (!raw && this.paletteDragPayload) raw = this.paletteDragPayload;
    if (!raw) return;
    let payload;
    try { payload = JSON.parse(raw); } catch { return; }
    ProcessTemplateEditor.prototype.insertPaletteItem.call(this, payload, point);
  }

  canvasCenterPoint() {
    return this.graph ? this.graph.canvasCenter() : { x: 0, y: 0 };
  }

  onCanvasPointerMove({ clientX, clientY, pointerType = '', event } = {}) {
    if (event?.isTrusted === false) return false;
    if ((pointerType && pointerType !== 'mouse' && pointerType !== 'pen')
        || !this.graph?.containsClientPoint?.(clientX, clientY)) {
      this.canvasPointer = null;
      return false;
    }
    this.canvasPointer = { clientX, clientY };
    return true;
  }

  onCanvasPointerLeave() {
    this.canvasPointer = null;
  }

  pasteTargetPoint() {
    const pointer = this.canvasPointer;
    if (pointer) {
      if (this.graph?.containsClientPoint?.(pointer.clientX, pointer.clientY)) {
        const point = this.graph.clientToGraph(pointer.clientX, pointer.clientY);
        if (Number.isFinite(point?.x) && Number.isFinite(point?.y)) return point;
      }
      this.canvasPointer = null;
    }
    return this.canvasCenterPoint();
  }

  addNodeType(type, point = this.canvasCenterPoint()) {
    if (this.destroyed) return false;
    const id = this.mutate(() => this.model.addNode(type, { x: point.x, y: point.y }));
    if (!id) return false;
    this.setSelection({ type: 'node', id });
    this.status(`Added ${type} node ${id}.`);
    return id;
  }

  editSelection() {
    if (this.destroyed) return false;
    if (this.selection?.type === 'template') {
      this.setSelection({ type: 'template' });
      this.inspectorFocusRequest += 1;
      this.publish?.();
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
    if (this.destroyed || !this.graph) return false;
    const items = selectionItems(this.selection);
    if (!items.length || items.some((item) => item.type !== 'node')) return false;
    const layout = this.graph.layoutSnapshot();
    const positions = Object.fromEntries(items.map((item) => {
      const node = layout.nodes.find((candidate) => candidate.id === item.id);
      return [item.id, node ? { x: node.x, y: node.y } : undefined];
    }));
    const idMap = this.mutate(() => this.model.duplicateNodes(items.map((item) => item.id), { positions }));
    if (!idMap?.size) return false;
    this.setSelection(makeSelection([...idMap.values()].map((id) => ({ type: 'node', id }))));
    this.status(`Duplicated ${idMap.size} node${idMap.size === 1 ? '' : 's'}.`);
    return idMap;
  }

  selectAll() {
    if (this.destroyed) return false;
    const items = [
      ...Object.keys(this.model.template.nodes).map((id) => ({ type: 'node', id })),
      ...this.model.edges.filter((edge) => edge.from).map((edge) => ({ type: 'edge', from: edge.from, outcome: edge.outcome })),
    ];
    this.setSelection(makeSelection(items));
    return items.length > 0;
  }

  clearSelection() {
    if (this.destroyed || !this.selection) return false;
    this.setSelection(null);
    return true;
  }

  fitGraph() {
    if (this.destroyed || !this.graph) return false;
    this.graph.fit();
    return true;
  }

  centerSelection() {
    if (this.destroyed || !this.graph) return false;
    const layout = this.graph.layoutSnapshot();
    const points = selectionItems(this.selection).map((item) => {
      if (item.type === 'node') return layout.nodes.find((node) => node.id === item.id);
      const edge = this.laidEdge(item.from, item.outcome);
      return edge?.label || layout.nodes.find((node) => node.id === item.from);
    }).filter(Boolean);
    if (!points.length) return false;
    this.graph.centerOn(
      points.reduce((sum, point) => sum + point.x, 0) / points.length,
      points.reduce((sum, point) => sum + point.y, 0) / points.length,
    );
    return true;
  }

  zoomGraph(factor) {
    if (this.destroyed || !this.graph) return false;
    return this.graph.zoomBy(factor);
  }

  resetZoom() {
    if (this.destroyed || !this.graph) return false;
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
    if (this.destroyed) {
      // A stale provider snapshot may still ask a destroyed editor for its
      // context; every affordance reads as unavailable rather than live.
      const closedReason = 'The process editor is closed.';
      return {
        hasGraph: false, hasSelection: false, hasGraphSelection: false,
        canCreate: false, createReason: closedReason,
        canEdit: false, editReason: closedReason,
        canDuplicate: false, duplicateReason: closedReason,
        canDelete: false, deleteReason: closedReason,
        canValidate: false, validateReason: closedReason,
        issueCount: 0, hasCurrentIssue: false,
        canSave: false, saveReason: closedReason,
        canInstantiate: false, instantiateReason: closedReason,
      };
    }
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
    const hasCurrentIssue = !!this.validation?.currentIssue?.();
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
      hasCurrentIssue,
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
    if (this.destroyed) return undefined;
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
    if (this.destroyed || externalInteractionPending(this)) return false;
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
    if (newOutcome === oldOutcome) return;
    // An emptied field used to be indistinguishable from "unchanged", so the
    // label silently snapped back and the author had no idea why. Outcomes are
    // the keys of the node's `next` map — a blank key is rejected by
    // model.validate and cannot exist in YAML — so say that instead of
    // pretending nothing was typed.
    if (!newOutcome) {
      this.status(this.model.outgoingEdges(from).length > 1
        ? 'An outcome label is required while this node has more than one outgoing connector: it selects which one the run takes.'
        : 'An outcome label is required.', true);
      return;
    }
    const ok = this.mutate(() => this.model.setEdgeOutcome(from, oldOutcome, newOutcome));
    if (ok) this.setSelection({ type: 'edge', from, outcome: newOutcome });
  }

  async deleteSelection() {
    if (this.destroyed || externalInteractionPending(this)) return false;
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
    // A rejected delete must keep the selection: the rewire rejection tells the
    // operator to retry with "Delete + drop edges", and clearing here would
    // force them to find and reselect the nodes first. mutate() returns
    // undefined only on rejection.
    const applied = this.mutate(() => this.model.deleteItems(items, { rewire: choice === 'rewire' }));
    if (applied === undefined) return false;
    this.setSelection(null);
    return true;
  }

  nameSnippetModal({ title, submitLabel, initialName = '' }) {
    return new Promise((resolve) => this.openModal({
      kind: 'snippet-name', title, submitLabel, initialName,
    }, resolve));
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

  onEditorCopy(event) {
    if (this.destroyed || event?.isTrusted === false || isProcessEditorFormControl(event.target)
        || this.modalDispose || !event.clipboardData?.setData) return false;
    if (hasNonCollapsedDOMSelection(event)) return false;
    const layout = this.graph?.layoutSnapshot?.();
    let payload;
    let text;
    try {
      payload = createProcessSelectionPayload(this.model, this.selection, layout?.nodes || []);
      if (!payload) return false;
      text = serializeProcessSelection(payload);
      event.clipboardData.setData('text/plain', text);
    } catch (error) {
      this.status(error instanceof ProcessClipboardError
        ? error.message : 'The selected nodes could not be serialized for the clipboard.', true);
      return false;
    }
    // ClipboardEvent data is committed only when the native copy is claimed.
    // Do this after successful serialization + setData so native selection copy
    // remains untouched on every failure path.
    event.preventDefault();
    this.pasteFingerprint = '';
    this.pasteRepeat = 0;
    this.pasteAnchor = null;
    this.status(`Copied ${payload.nodes.length} node${payload.nodes.length === 1 ? '' : 's'}.`);
    return true;
  }

  onEditorPaste(event) {
    if (this.destroyed || event?.isTrusted === false || isProcessEditorFormControl(event.target)
        || this.modalDispose || !event.clipboardData?.getData) return false;
    let text;
    try {
      text = event.clipboardData.getData('text/plain');
    } catch {
      return false;
    }
    // Unrelated clipboard text is never claimed, inspected further, or copied
    // into editor state. Native browser ownership remains intact.
    if (!isProcessSelectionClipboardText(text)) return false;
    event.preventDefault();
    let payload;
    try {
      payload = parseProcessSelection(text);
    } catch (error) {
      this.status(error instanceof ProcessClipboardError
        ? error.message : 'Clipboard selection is invalid or unsupported.', true);
      return true;
    }
    if (externalInteractionPending(this)) {
      this.status('Wait for the external reload to finish before pasting.', true);
      return true;
    }
    if (!this.model.config.canInsert) {
      this.status('Pasting nodes is not allowed in this read-only view.', true);
      return true;
    }

    const fingerprint = processSelectionFingerprint(text);
    let placement;
    let idMap;
    try {
      placement = resolveProcessPastePlacement(fingerprint, this.pasteTargetPoint(), {
        fingerprint: this.pasteFingerprint,
        repeat: this.pasteRepeat,
        anchor: this.pasteAnchor,
      });
      idMap = this.model.insertClipboardSelection(payload, {
        center: placement.center,
        offset: { x: placement.repeat * 36, y: placement.repeat * 36 },
      });
    } catch (error) {
      this.status(error instanceof ProcessClipboardError
        ? error.message : 'Clipboard selection could not be pasted.', true);
      return true;
    }

    this.pasteFingerprint = fingerprint;
    this.pasteRepeat = placement.repeat;
    this.pasteAnchor = placement.center;
    this.status('');
    this.refresh();
    const ids = [...idMap.values()];
    this.setSelection(makeSelection(ids.map((id) => ({ type: 'node', id }))));
    this.status(`Pasted ${ids.length} node${ids.length === 1 ? '' : 's'}.`);
    queueMicrotask(() => this.graph?.focusNode?.(ids[0]));
    return true;
  }

  // ---- connector label pinning ---------------------------------------------------

  // edgePinView anchors the pin toggle to the selected connector's label. The
  // button exists only while exactly one edge is selected: a multi-selection has
  // no single label to talk about, and an unselected connector deliberately
  // offers no affordance -- its label is either pinned on or decluttered away,
  // and selecting it is how you get the control back.
  edgePinView() {
    const items = selectionItems(this.selection);
    const item = items.length === 1 && items[0].type === 'edge' ? items[0] : null;
    const edge = item && this.model.findEdge(item.from, item.outcome);
    if (!edge) return { open: false };
    // Pinning is layout metadata, but it still writes through the model's undo
    // gate and save body, so a view that cannot edit this edge must not
    // advertise a working toggle via aria-pressed and then throw on click.
    if (this.model.config.edgeEditable && !this.model.config.edgeEditable(edge)) return { open: false };
    const laid = this.laidEdge(item.from, item.outcome);
    if (!laid?.label) return { open: false };
    const pinned = this.edgePinnedEffective(item.from, item.outcome);
    const position = this.stagePosition(laid.label.x, laid.label.y);
    return {
      open: true,
      from: item.from,
      outcome: item.outcome,
      left: position.left,
      top: position.top,
      pinned,
      title: edgePinTitle(item.outcome, pinned),
    };
  }

  // edgePinOpen is the cheap predicate the viewport hook uses: republishing on
  // every pan frame is only worth it while a pin is actually on screen.
  edgePinOpen() {
    const items = selectionItems(this.selection);
    return items.length === 1 && items[0].type === 'edge';
  }

  // edgePinnedEffective resolves the tri-state into the boolean the button
  // renders and toggles against, so an author with no stored opinion toggles
  // away from what they can currently see rather than from an invisible default.
  edgePinnedEffective(from, outcome) {
    const stored = this.model.edgePinned(from, outcome);
    if (stored !== undefined) return !!stored;
    const siblings = this.model.outgoingEdges(from).length;
    return defaultPinned(outcome, siblings, this.model.node(from)?.type);
  }

  toggleEdgePin(from, outcome) {
    const next = !this.edgePinnedEffective(from, outcome);
    this.mutate(() => this.model.setEdgePinned(from, outcome, next));
  }

  // ---- inline (in-place) label editing ------------------------------------------

  stagePosition(x, y) {
    return this.graph ? this.graph.graphPointToHost({ x, y }, this.stage) : { left: 0, top: 0 };
  }

  openInline(x, y, value, commit) {
    if (this.destroyed || externalInteractionPending(this)) return false;
    this.closeInline(false);
    const position = this.stagePosition(x, y);
    this.inlineCommit = commit;
    this.inlineState = {
      open: true, token: this.inlineState.token + 1,
      left: position.left, top: position.top, value: String(value || ''),
    };
    this.publish();
    return true;
  }

  closeInline(apply, value = this.inlineState.value) {
    if (!this.inlineState.open) return;
    const commit = this.inlineCommit;
    this.inlineCommit = null;
    this.inlineState = { ...this.inlineState, open: false };
    this.publish();
    if (apply && commit) commit(String(value || '').trim());
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
    if (this.destroyed) return false;
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
      // destroy() (or a newer lifecycle generation) invalidates the sequence
      // mid-flight; a discarded completion must not report success to callers
      // like requestInstantiate that act on it.
      return requestSeq === this.saveSeq;
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

  async requestScribe(kind = 'template') {
    if (this.destroyed || !this.options.onScribe || this.savePending || externalInteractionPending(this)) return false;
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
            const guardedInteraction = graphInteraction(this);
            const requestSeq = ++this.externalReloadSeq;
            this.externalReloadPending = true;
            this.updateChrome?.();
            try {
              view = await fetchEditView(id);
              if (requestSeq !== this.externalReloadSeq || this.abort.signal.aborted) return false;
              if (this.model !== guardedModel || guardedModel.rev !== guardedRev || this.savePending
                  || this.modalDispose !== guardedModal || this.inlineCommit !== guardedInline
                  || graphInteraction(this).active
                  || graphInteraction(this).generation !== guardedInteraction.generation) {
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
          this.graph?.resetInteractionLayering?.();
          this.loadedView = structuredClone(view);
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
    const anchor = {
      kind: 'template', id,
      currentRef: this.model.currentRef || '', sourceHash: this.model.sourceHash || '',
      isNew: this.blank && !this.model.sourceHash,
    };
    let context;
    let handoff;
    const focusedDiagnostic = kind === 'diagnostic' ? this.validation?.currentIssue?.() || null : null;
    const guardedModel = this.model;
    const guardedRev = guardedModel.rev;
    const guardedSelection = kind === 'selection' ? scribeSelectionIdentity(this.selection) : '';
    try {
      handoff = processScribeHandoff(anchor);
      context = processScribeEditorContext({
        kind, handoff, template: this.model.template, edges: this.model.edges,
        selection: selectionItems(this.selection), diagnostic: focusedDiagnostic,
      });
    } catch (error) {
      this.status(error.message, true);
      return false;
    }
    const freshnessGuard = () => {
      const currentID = (this.model?.template?.id || '').trim();
      const modelFresh = !this.abort?.signal?.aborted
        && this.model === guardedModel && this.model.rev === guardedRev
        && currentID === anchor.id
        && (this.model.currentRef || '') === anchor.currentRef
        && (this.model.sourceHash || '') === anchor.sourceHash
        && (this.blank && !this.model.sourceHash) === anchor.isNew
        && !this.savePending && !externalInteractionPending(this);
      const selectionFresh = kind !== 'selection' || scribeSelectionIdentity(this.selection) === guardedSelection;
      let diagnosticFresh = kind !== 'diagnostic';
      if (kind === 'diagnostic') {
        const currentDiagnostic = this.validation?.currentIssue?.() || null;
        if (focusedDiagnostic && currentDiagnostic) {
          try {
            const currentContext = processScribeEditorContext({
              kind, handoff, template: this.model.template, edges: this.model.edges,
              selection: [], diagnostic: currentDiagnostic,
            });
            diagnosticFresh = JSON.stringify(currentContext.diagnostic) === JSON.stringify(context.diagnostic);
          } catch {
            diagnosticFresh = false;
          }
        }
      }
      if (modelFresh && selectionFresh && diagnosticFresh) return true;
      this.status('Scribe handoff cancelled because the editor context changed while the request was open.');
      this.graph?.focus?.();
      return false;
    };
    const prompt = await this.scribePreviewModal({
      kind, prompt: processScribePrompt(kind), context: processScribeContextPreview(context),
      truncated: !!context.truncation,
    });
    if (prompt == null || this.abort?.signal.aborted) {
      return false;
    }
    if (!freshnessGuard()) return false;
    return !!(await this.options.onScribe(anchor, { context, prompt, freshnessGuard }));
  }

  async saveRequest(requestSeq) {
    if (requestSeq !== this.saveSeq) return;
    const id = (this.model.template.id || '').trim();
    const savedID = id;
    // The canvas stays interactive during the POST: capture the rev the
    // payload was built at, so edits made in flight keep the model dirty.
    const savedAtRev = this.model.rev;
    const savedView = this.model.saveBody();
    const response = await fetch(`/v1/process/templates/${encodeURIComponent(id)}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(savedView),
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
    this.model.markSaved(body, savedAtRev);
    this.loadedView = {
      ...savedView, currentRef: body.ref || '', sourceHash: body.sourceHash || '',
      semanticHash: body.semanticHash || '', diagnostics: body.diagnostics || [],
    };
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
        this.graph?.resetInteractionLayering?.();
        this.loadedView = structuredClone(view);
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
    if (!this.snapshotSignal) {
      const { openStandaloneChoiceDialog } = await import('./process-editor-island.js');
      return openStandaloneChoiceDialog({ title, body, choices });
    }
    return new Promise((resolve) => this.openModal({ kind: 'choice', title, body, choices }, resolve));
  }

  // The human edits only their request. Stable context stays read-only and
  // visibly delimited so it cannot quietly become an alternate template body.
  async scribePreviewModal({ kind, prompt, context, truncated = false }) {
    const current = this.modalDispose;
    if (current) {
      const closed = current.requestClose ? await current.requestClose() : (current(null), true);
      if (!closed || this.abort?.signal.aborted) return null;
    }
    if (!this.snapshotSignal) {
      const { openStandaloneScribeDialog } = await import('./process-editor-island.js');
      return openStandaloneScribeDialog({ kind, prompt, context, truncated });
    }
    return new Promise((resolve) => this.openModal({
      kind: 'scribe', scribeKind: kind, prompt, context, truncated,
    }, resolve));
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
  const ui = await import('./process-editor-island.js');
  return new ProcessTemplateEditor(mount, view, {
    ...config, blank,
    ui: { createPublisher: ui.createProcessEditorPublisher, mount: ui.mountProcessEditorIsland },
  });
}
