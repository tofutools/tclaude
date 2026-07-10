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
  ProcessEditModel, blankEditView, graphEdgeID,
  PALETTE_PRIMITIVES, PALETTE_SNIPPETS,
} from './process-edit-model.js';

const SVG_NS = 'http://www.w3.org/2000/svg';
// Custom drag payload MIME (dock-dnd idiom): withholding text/plain keeps
// every other document-level DnD feature out of a palette drag.
const PALETTE_MIME = 'application/x-tclaude-process-palette';

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
    this.abort = new AbortController();
    this.buildDOM();
    this.graph = new ProcessGraph(this.stageHost, this.model.graph(), {
      ariaLabel: `Process template editor: ${this.model.template.id}`,
      colorScheme: 'dark',
      onNodeClick: (e) => this.onNodeClick(e),
      onNodeDblClick: (e) => this.onNodeDblClick(e),
      onEdgeClick: (e) => this.onEdgeClick(e),
      onNodeDrag: (e) => this.onNodeDrag(e),
      onPortDragStart: (e) => this.onPortDragStart(e),
      onPortDragMove: (e) => this.onPortDragMove(e),
      onPortDragEnd: (e) => this.onPortDragEnd(e),
      onCanvasDrop: (e) => this.onCanvasDrop(e),
    });
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

    this.undoButton = h('button', { class: 'process-action', type: 'button', title: 'Undo (Ctrl+Z)', text: '↶ undo' });
    this.redoButton = h('button', { class: 'process-action', type: 'button', title: 'Redo (Ctrl+Shift+Z)', text: '↷ redo' });
    this.paletteButton = h('button', { class: 'process-action', type: 'button', title: 'Toggle the node palette', text: '⬒ palette' });
    this.saveButton = h('button', { class: 'process-action primary', type: 'button', title: 'Save a new version', text: 'Save' });

    const header = h('div', { class: 'process-editor-header' },
      this.blank ? this.idInput : this.titleLabel,
      this.versionBadge,
      this.dirtyBadge,
      this.statusLine,
      h('span', { class: 'spacer' }),
      this.undoButton, this.redoButton, this.paletteButton, this.saveButton,
    );

    this.palette = this.buildPalette();
    this.stageHost = h('div', { class: 'process-editor-canvas-host' });
    this.inlineInput = h('input', {
      class: 'process-editor-inline-input', type: 'text', spellcheck: 'false', hidden: '',
    });
    this.stage = h('div', { class: 'process-editor-stage' }, this.stageHost, this.inlineInput);
    const body = h('div', { class: 'process-editor-body' }, this.palette, this.stage);

    this.inspector = h('div', { class: 'process-editor-inspector' });
    this.root = h('div', { class: 'process-editor' }, header, body, this.inspector);
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
    this.undoButton.addEventListener('click', () => this.applyHistory('undo'), { signal });
    this.redoButton.addEventListener('click', () => this.applyHistory('redo'), { signal });
    this.paletteButton.addEventListener('click', () => {
      this.palette.hidden = !this.palette.hidden;
    }, { signal });
    if (this.blank) {
      this.idInput.addEventListener('change', () => {
        this.model.setTemplateMeta({ id: this.idInput.value.trim() });
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
    this.abort.abort();
    this.closeInline(false);
    this.graph.destroy();
    this.modalOverlay?.remove();
    delete this.mount.__processEditor;
    this.mount.classList.remove('process-editor-mount');
    this.mount.replaceChildren();
  }

  // ---- chrome ------------------------------------------------------------

  refresh({ fit = false } = {}) {
    this.graph.setGraph(this.model.graph(), { fit });
    this.updateChrome();
  }

  updateChrome() {
    const { model } = this;
    this.titleLabel.textContent = model.template.name
      ? `${model.template.name} (${model.template.id})`
      : model.template.id || 'untitled';
    this.versionBadge.textContent = model.semanticHash ? `v ${shortHash(model.semanticHash)}` : 'unsaved';
    this.versionBadge.title = model.semanticHash || 'This template has never been saved';
    this.dirtyBadge.hidden = !model.dirty;
    // The id is only editable until the first version exists; after that the
    // id is the store key and renaming means a different template.
    this.idInput.disabled = !!model.sourceHash;
    this.undoButton.disabled = !model.canUndo;
    this.redoButton.disabled = !model.canRedo;
    this.saveButton.disabled = !model.dirty && !!model.sourceHash;
    this.renderInspector();
  }

  status(message, isError = false) {
    this.statusLine.textContent = message || '';
    this.statusLine.classList.toggle('is-error', !!isError);
  }

  // ---- selection + inspector ----------------------------------------------

  setSelection(selection) {
    this.selection = selection;
    if (selection?.type === 'node') this.graph.select({ type: 'node', id: selection.id });
    else if (selection?.type === 'edge') this.graph.select({ type: 'edge', id: graphEdgeID(selection.from, selection.outcome) });
    else this.graph.select(null);
    this.renderInspector();
  }

  renderInspector() {
    const parts = [];
    const sel = this.selection;
    if (sel?.type === 'node' && this.model.node(sel.id)) {
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
      parts.push(h('button', {
        class: 'process-action', type: 'button', text: 'node settings…',
        title: 'Node edit dialogs land in a later ticket (TCL-298)', disabled: '',
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
      parts.push(h('span', { class: 'process-inspector-hint', text: 'Select a node or edge to edit it. Double-click a node to rename in place.' }));
    }
    this.inspector.replaceChildren(...parts);
  }

  // ---- graph hooks ---------------------------------------------------------

  onNodeClick({ node }) {
    if (!node) return;
    this.setSelection({ type: 'node', id: node.id });
  }

  onNodeDblClick({ node }) {
    if (!node) return;
    this.setSelection({ type: 'node', id: node.id });
    this.openInlineNodeRename(node.id);
  }

  onEdgeClick({ edge }) {
    if (!edge) return;
    const already = this.selection?.type === 'edge'
      && this.selection.from === edge.from && this.selection.outcome === edge.outcome;
    this.setSelection({ type: 'edge', from: edge.from, outcome: edge.outcome });
    // Second click on an already-selected edge edits the outcome label in place.
    if (already) this.openInlineOutcomeEdit(edge.from, edge.outcome);
  }

  onNodeDrag({ nodeId, delta }) {
    if (!this.pendingMove || this.pendingMove.id !== nodeId) {
      const laid = this.graph.layout.nodes.find((candidate) => candidate.id === nodeId);
      if (!laid) return;
      this.pendingMove = { id: nodeId, startX: laid.x, startY: laid.y, delta };
    }
    this.pendingMove.delta = delta;
  }

  commitPendingMove() {
    const move = this.pendingMove;
    this.pendingMove = null;
    if (!move || !move.delta) return;
    // The core's own click-vs-drag threshold is 3px; mirror it so a plain
    // click never pins an auto-laid node.
    if (Math.hypot(move.delta.x, move.delta.y) <= 3) return;
    this.mutate(() => this.model.moveNode(move.id, move.startX + move.delta.x, move.startY + move.delta.y));
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
    } else if (targetPort === 'out' && targetNodeId !== source.nodeId) {
      // out → out is ambiguous; treat the drop node as the target anyway.
    }
    if (from === to && source.nodeId === targetNodeId && !targetPort) return; // released on own body
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
      const id = this.mutate(() => this.model.addNode(payload.type, { x: point.x, y: point.y }));
      if (id) {
        this.setSelection({ type: 'node', id });
        this.status(`Added ${payload.type} node ${id}.`);
      }
    } else if (payload.kind === 'snippet') {
      const snippet = PALETTE_SNIPPETS.find((candidate) => candidate.key === payload.key);
      if (!snippet) return;
      const idMap = this.mutate(() => this.model.insertSnippet(snippet, { x: point.x, y: point.y }));
      if (idMap) this.status(`Inserted snippet ${snippet.label} (${idMap.size} nodes).`);
    }
  }

  // ---- edit ops ----------------------------------------------------------------

  // mutate wraps a model mutation: refresh + chrome on success, status line on
  // rejection (duplicate outcome, read-only node, …). Returns the mutation's
  // result, or undefined when rejected.
  mutate(operation, { fit = false } = {}) {
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
    const moved = direction === 'undo' ? this.model.undo() : this.model.redo();
    if (!moved) return;
    // A restored state may no longer contain the selected node/edge.
    if (this.selection?.type === 'node' && !this.model.node(this.selection.id)) this.selection = null;
    if (this.selection?.type === 'edge' && !this.model.findEdge(this.selection.from, this.selection.outcome)) this.selection = null;
    this.refresh();
  }

  renameEdgeOutcome(from, oldOutcome, newOutcome) {
    if (!newOutcome || newOutcome === oldOutcome) return;
    const ok = this.mutate(() => this.model.setEdgeOutcome(from, oldOutcome, newOutcome));
    if (ok) this.setSelection({ type: 'edge', from, outcome: newOutcome });
  }

  async deleteSelection() {
    const sel = this.selection;
    if (!sel) return;
    if (sel.type === 'edge') {
      this.mutate(() => this.model.deleteEdge(sel.from, sel.outcome));
      this.setSelection(null);
      return;
    }
    if (sel.type !== 'node' || !this.model.node(sel.id)) return;
    const incoming = this.model.incomingEdges(sel.id);
    const outgoing = this.model.outgoingEdges(sel.id);
    let rewire = false;
    if (incoming.length && outgoing.length) {
      // Deleting a mid-graph node orphans its neighbours' edges; offer the
      // rewire affordance instead of silently dropping the connections.
      const choice = await this.choiceModal({
        title: `Delete node ${sel.id}?`,
        body: `${incoming.length} incoming and ${outgoing.length} outgoing edge${outgoing.length === 1 ? '' : 's'} connect through this node.`,
        choices: [
          { key: 'rewire', label: 'Delete + rewire through', primary: true },
          { key: 'drop', label: 'Delete + drop edges', danger: true },
        ],
      });
      if (!choice) return;
      rewire = choice === 'rewire';
    }
    this.mutate(() => this.model.deleteNode(sel.id, { rewire }));
    this.setSelection(null);
  }

  onEditorKeyDown(event) {
    const inInput = event.target instanceof HTMLInputElement
      || event.target instanceof HTMLSelectElement
      || event.target instanceof HTMLTextAreaElement;
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

  openInlineNodeRename(nodeId) {
    const laid = this.graph.layout.nodes.find((candidate) => candidate.id === nodeId);
    if (!laid) return;
    const node = this.model.node(nodeId);
    this.openInline(laid.x, laid.y, node?.name || '', (value) => {
      this.mutate(() => this.model.renameNode(nodeId, value));
    });
  }

  openInlineOutcomeEdit(from, outcome) {
    const laid = this.graph.layout.edges.find((candidate) => candidate.id === graphEdgeID(from, outcome));
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
      return;
    }
    this.saveButton.disabled = true;
    try {
      const response = await fetch(`/v1/process/templates/${encodeURIComponent(id)}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(this.model.saveBody()),
      });
      const body = await response.json().catch(() => ({}));
      if (response.status === 409 && body.code === 'process_template_conflict') {
        await this.resolveConflict(body);
        return;
      }
      if (!response.ok) {
        this.status(body.message || body.error || `${response.status} ${response.statusText}`, true);
        return;
      }
      this.model.markSaved(body);
      this.blank = false;
      const diagCount = (body.diagnostics || []).length;
      this.status(`Saved version ${shortHash(body.semanticHash)}${diagCount ? ` · ${diagCount} advisory finding${diagCount === 1 ? '' : 's'}` : ''}.`);
      this.updateChrome();
      this.options.onSaved?.(body);
    } catch (error) {
      this.status(`Save failed: ${error.message}`, true);
    } finally {
      this.saveButton.disabled = false;
      this.updateChrome();
    }
  }

  // resolveConflict is the explicit 409 dialog (never a silent overwrite):
  // reload their head version (discarding local edits), or save as a new
  // version on top of theirs (rebasing this draft's CAS base).
  async resolveConflict(conflict) {
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
    if (choice === 'force') {
      this.model.sourceHash = conflict.currentSourceHash || '';
      await this.save();
    } else if (choice === 'reload') {
      try {
        const view = await fetchEditView(this.model.template.id);
        this.model = new ProcessEditModel(view, this.model.config);
        this.blank = false;
        this.selection = null;
        this.refresh({ fit: true });
        this.status(`Reloaded their version ${shortHash(view.semanticHash)}.`);
      } catch (error) {
        this.status(`Reload failed: ${error.message}`, true);
      }
    }
  }

  // choiceModal: a promise-based dialog on the shared .modal-overlay styling,
  // owned per-editor (the global #confirm-modal singleton only offers two
  // fixed buttons). Escape / backdrop resolve null.
  choiceModal({ title, body, choices }) {
    return new Promise((resolve) => {
      this.modalOverlay?.remove();
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
        if (this.modalOverlay === overlay) this.modalOverlay = null;
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
      this.modalOverlay = overlay;
      document.body.append(overlay);
      (buttons.find((_, index) => choices[index].primary) || cancel).focus();
    });
  }
}

// openTemplateEditor mounts the editor into `mount` for a template id (or a
// blank scaffold). Returns the editor instance; throws on fetch errors so the
// caller can render its own error state.
export async function openTemplateEditor(mount, { id, blank = false, version, config = {} } = {}) {
  const view = blank ? blankEditView(id) : await fetchEditView(id, version);
  mount.__processEditor?.destroy?.();
  return new ProcessTemplateEditor(mount, view, { ...config, blank });
}
