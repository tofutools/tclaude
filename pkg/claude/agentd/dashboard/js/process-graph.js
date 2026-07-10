// process-graph.js -- shared SVG process graph renderer + interaction shell.
// It owns presentation and pointer mechanics only. Editor/viewer semantics live
// behind hooks; this module never fetches, validates, mutates templates, or
// computes run state.

import { layoutProcessGraph } from './process-layout.js';

const SVG_NS = 'http://www.w3.org/2000/svg';
const MIN_ZOOM = 0.18;
const MAX_ZOOM = 3.5;
let nextGraphID = 1;

function svgElement(name, attrs = {}) {
  const element = document.createElementNS(SVG_NS, name);
  for (const [key, value] of Object.entries(attrs)) {
    if (value !== undefined && value !== null) element.setAttribute(key, String(value));
  }
  return element;
}

function htmlElement(name, attrs = {}) {
  const element = document.createElement(name);
  for (const [key, value] of Object.entries(attrs)) {
    if (key === 'class') element.className = value;
    else if (key === 'text') element.textContent = value;
    else element.setAttribute(key, String(value));
  }
  return element;
}

function clamp(value, min, max) {
  return Math.max(min, Math.min(max, value));
}

function hook(options, name) {
  return typeof options[name] === 'function' ? options[name] : () => {};
}

function edgeLabel(edge) {
  const bits = [];
  if (edge.outcome) bits.push(String(edge.outcome));
  if (edge.joinOnTarget) bits.push(`join: ${edge.joinOnTarget}`);
  if (!bits.length && edge.back) bits.push('return');
  return bits.join(' · ');
}

function overlayText(overlay) {
  if (!overlay) return '';
  const bits = [];
  if (overlay.glyph) bits.push(String(overlay.glyph));
  if (overlay.label || overlay.status) bits.push(String(overlay.label || overlay.status));
  if (overlay.progress) {
    if (typeof overlay.progress === 'string') bits.push(overlay.progress);
    else bits.push(`${overlay.progress.current ?? 0}/${overlay.progress.total ?? 0}`);
  }
  if (overlay.attempt != null || overlay.attempts != null) bits.push(`attempt ${overlay.attempt ?? overlay.attempts}`);
  if (overlay.retry != null || overlay.retries != null) bits.push(`retry ${overlay.retry ?? overlay.retries}`);
  if (overlay.badge) bits.push(String(overlay.badge));
  return bits.join(', ');
}

function wrapLabel(label, maxChars) {
  const words = String(label || '').trim().split(/\s+/).filter(Boolean);
  if (!words.length) return [''];
  const lines = [];
  let line = '';
  for (const word of words) {
    if (!line) line = word;
    else if (`${line} ${word}`.length <= maxChars) line += ` ${word}`;
    else {
      lines.push(line);
      line = word;
    }
  }
  lines.push(line);
  return lines.slice(0, 3);
}

function renderText(parent, node) {
  const maxChars = node.compound?.collapsed ? 22 : node.type === 'decision' ? 14 : 20;
  const lines = wrapLabel(node.label || node.id, maxChars);
  const text = svgElement('text', { class: 'process-node-label', 'text-anchor': 'middle', 'aria-hidden': 'true' });
  const lineHeight = 16;
  const startY = -(lines.length - 1) * lineHeight / 2;
  lines.forEach((line, index) => {
    const tspan = svgElement('tspan', { x: 0, y: startY + index * lineHeight });
    tspan.textContent = line;
    text.append(tspan);
  });
  parent.append(text);
}

function renderPeripheralLabel(parent, node) {
  if (!node.label) return;
  const text = svgElement('text', {
    class: 'process-node-label process-node-label-peripheral',
    x: 0, y: node.height / 2 + 20, 'text-anchor': 'middle', 'aria-hidden': 'true',
  });
  text.textContent = String(node.label);
  parent.append(text);
}

function renderClock(parent) {
  parent.append(
    svgElement('circle', { class: 'process-clock-face', cx: 0, cy: 0, r: 12 }),
    svgElement('path', { class: 'process-clock-hand', d: 'M 0 -7 L 0 0 L 7 4' }),
  );
}

function renderCompoundAffordance(parent, node) {
  const right = node.width / 2;
  const bottom = node.height / 2;
  parent.append(
    svgElement('path', { class: 'process-stage-stack', d: `M ${-right + 13} ${bottom - 18} h 19 M ${-right + 10} ${bottom - 13} h 19 M ${-right + 7} ${bottom - 8} h 19` }),
    svgElement('circle', { class: 'process-expand-ring', cx: right - 16, cy: bottom - 16, r: 9 }),
    svgElement('path', { class: 'process-expand-plus', d: `M ${right - 20} ${bottom - 16} h 8 M ${right - 16} ${bottom - 20} v 8` }),
  );
  const count = Array.isArray(node.compound?.stages) ? node.compound.stages.length : node.compound?.stages;
  if (count) {
    const stageCount = svgElement('text', { class: 'process-stage-count', x: -right + 34, y: bottom - 8 });
    stageCount.textContent = `${count} stages`;
    parent.append(stageCount);
  }
}

function renderShape(parent, node) {
  if (node.compound?.collapsed) {
    parent.append(svgElement('rect', {
      class: 'process-node-shape process-shape-compound',
      x: -node.width / 2, y: -node.height / 2, width: node.width, height: node.height, rx: 14,
    }));
    renderCompoundAffordance(parent, node);
    return;
  }
  switch (node.type) {
    case 'decision':
      parent.append(svgElement('polygon', {
        class: 'process-node-shape process-shape-decision',
        points: `0,${-node.height / 2} ${node.width / 2},0 0,${node.height / 2} ${-node.width / 2},0`,
      }));
      break;
    case 'wait':
      parent.append(svgElement('circle', { class: 'process-node-shape process-shape-wait', cx: 0, cy: 0, r: node.width / 2 }));
      renderClock(parent);
      break;
    case 'start':
      parent.append(svgElement('circle', { class: 'process-node-shape process-shape-start', cx: 0, cy: 0, r: node.width / 2 }));
      break;
    case 'end':
      parent.append(
        svgElement('circle', { class: 'process-node-shape process-shape-end', cx: 0, cy: 0, r: node.width / 2 }),
        svgElement('circle', { class: 'process-end-inner', cx: 0, cy: 0, r: node.width / 2 - 7 }),
      );
      break;
    default:
      parent.append(svgElement('rect', {
        class: 'process-node-shape process-shape-task',
        x: -node.width / 2, y: -node.height / 2, width: node.width, height: node.height, rx: 12,
      }));
  }
}

function renderOverlay(parent, node) {
  const overlay = node.overlay || node.stateOverlay;
  const x = node.width / 2 - 9;
  const y = -node.height / 2 + 9;
  const group = svgElement('g', {
    class: `process-overlay-anchor${overlay ? ' has-overlay' : ''}`,
    transform: `translate(${x} ${y})`,
    'aria-hidden': 'true',
  });
  group.append(svgElement('circle', { class: 'process-overlay-ring', cx: 0, cy: 0, r: 11 }));
  if (overlay?.glyph) {
    const glyph = svgElement('text', { class: 'process-overlay-glyph', x: 0, y: 4, 'text-anchor': 'middle' });
    glyph.textContent = String(overlay.glyph);
    group.append(glyph);
  }
  const progress = overlay?.progress;
  const progressText = typeof progress === 'string'
    ? progress
    : progress ? `${progress.current ?? 0}/${progress.total ?? 0}` : '';
  const attempt = overlay?.attempt ?? overlay?.attempts;
  const retry = overlay?.retry ?? overlay?.retries;
  const detail = [progressText, attempt != null ? `#${attempt}` : '', retry != null ? `↻${retry}` : ''].filter(Boolean).join(' ');
  if (detail) {
    const text = svgElement('text', { class: 'process-overlay-detail', x: -16, y: node.height - 8, 'text-anchor': 'end' });
    text.textContent = detail;
    group.append(text);
  }
  if (overlay?.badge) {
    const badge = svgElement('text', { class: 'process-overlay-badge', x: -16, y: 4, 'text-anchor': 'end' });
    badge.textContent = String(overlay.badge);
    group.append(badge);
  }
  parent.append(group);
}

function renderPorts(parent, node) {
  const input = svgElement('circle', {
    class: 'process-port process-port-in', cx: 0, cy: -node.height / 2, r: 6,
    'data-port': 'in', 'aria-hidden': 'true',
  });
  const output = svgElement('circle', {
    class: 'process-port process-port-out', cx: 0, cy: node.height / 2, r: 6,
    'data-port': 'out', 'aria-hidden': 'true',
  });
  parent.append(input, output);
}

function renderMarkers(defs, markerID, backMarkerID) {
  const marker = svgElement('marker', {
    id: markerID, viewBox: '0 0 10 10', refX: 9, refY: 5,
    markerWidth: 7, markerHeight: 7, orient: 'auto-start-reverse',
  });
  marker.append(svgElement('path', { class: 'process-arrowhead', d: 'M 0 0 L 10 5 L 0 10 z' }));
  const backMarker = svgElement('marker', {
    id: backMarkerID, viewBox: '0 0 10 10', refX: 9, refY: 5,
    markerWidth: 7, markerHeight: 7, orient: 'auto-start-reverse',
  });
  backMarker.append(svgElement('path', { class: 'process-arrowhead process-arrowhead-back', d: 'M 0 0 L 10 5 L 0 10 z' }));
  defs.append(marker, backMarker);
}

export class ProcessGraph {
  constructor(container, graph = { nodes: [], edges: [] }, options = {}) {
    if (!(container instanceof Element)) throw new TypeError('ProcessGraph container must be a DOM Element');
    this.container = container;
    this.options = { ...options };
    this.graph = graph;
    this.instanceID = nextGraphID++;
    this.markerID = `process-arrow-${this.instanceID}`;
    this.backMarkerID = `process-back-arrow-${this.instanceID}`;
    this.view = { x: 0, y: 0, k: 1 };
    this.selected = null;
    this.pointer = null;
    this.dragMoved = false;
    this.suppressClick = false;
    this.destroyed = false;
    this.abort = new AbortController();

    // morph.js treats this element as live-owned. The graph's stable imperative
    // subtree is therefore exempt from polling reconciliation, preserving view
    // transform/selection and preventing a fresh empty host from deleting SVG.
    container.setAttribute('data-morph-owned', 'process-graph');
    container.replaceChildren();
    this.root = htmlElement('div', {
      class: 'process-graph',
      tabindex: '0',
      role: 'application',
      'aria-label': options.ariaLabel || 'Process graph',
      'data-color-scheme': options.colorScheme || 'dark',
    });
    this.svg = svgElement('svg', { class: 'process-graph-svg', role: 'img' });
    this.svg.append(svgElement('title'));
    this.defs = svgElement('defs');
    renderMarkers(this.defs, this.markerID, this.backMarkerID);
    this.viewport = svgElement('g', { class: 'process-graph-viewport' });
    this.edgeLayer = svgElement('g', { class: 'process-edge-layer', 'data-key': 'edges' });
    this.nodeLayer = svgElement('g', { class: 'process-node-layer', 'data-key': 'nodes' });
    this.viewport.append(this.edgeLayer, this.nodeLayer);
    this.svg.append(this.defs, this.viewport);
    this.controls = htmlElement('div', { class: 'process-graph-controls', 'aria-label': 'Graph view controls' });
    this.fitButton = htmlElement('button', { class: 'process-fit-button', type: 'button', text: 'Fit', title: 'Fit graph to view' });
    this.controls.append(this.fitButton);
    this.root.append(this.svg, this.controls);
    container.append(this.root);
    this.bindEvents();
    this.setGraph(graph, { fit: options.fitOnRender !== false });
  }

  setGraph(graph, { fit = false } = {}) {
    this.graph = graph || { nodes: [], edges: [] };
    this.layout = layoutProcessGraph(this.graph, this.options.layout || {});
    this.render();
    if (fit) requestAnimationFrame(() => this.fitToView());
    return this.layout;
  }

  setOptions(options = {}) {
    this.options = { ...this.options, ...options };
    this.root.dataset.colorScheme = this.options.colorScheme || 'dark';
    return this.setGraph(this.graph);
  }

  render() {
    const title = this.svg.querySelector('title');
    title.textContent = this.options.ariaLabel || `Process graph with ${this.layout.nodes.length} nodes`;
    this.edgeLayer.replaceChildren();
    this.nodeLayer.replaceChildren();
    for (const edge of this.layout.edges) this.edgeLayer.append(this.renderEdge(edge));
    for (const node of this.layout.nodes) this.nodeLayer.append(this.renderNode(node));
    this.applyView();
    this.applySelection();
  }

  renderEdge(edge) {
    const key = `edge:${edge.inputIndex}:${edge.from}:${edge.to}`;
    const group = svgElement('g', {
      class: `process-edge${edge.back ? ' process-edge-back' : ''}`,
      'data-key': key,
      'data-edge-index': edge.inputIndex,
      'data-from': edge.from,
      'data-to': edge.to,
      role: 'button',
      tabindex: '0',
      'aria-label': `${edge.back ? 'Return edge' : 'Edge'} from ${edge.from} to ${edge.to}${edgeLabel(edge) ? `: ${edgeLabel(edge)}` : ''}`,
    });
    const visible = svgElement('path', {
      class: 'process-edge-path', d: edge.path, fill: 'none',
      'marker-end': `url(#${edge.back ? this.backMarkerID : this.markerID})`,
    });
    const hit = svgElement('path', { class: 'process-edge-hit', d: edge.path, fill: 'none' });
    group.append(visible, hit);
    const label = edgeLabel(edge);
    if (label) {
      const labelGroup = svgElement('g', { class: 'process-edge-label', transform: `translate(${edge.label.x} ${edge.label.y})`, 'aria-hidden': 'true' });
      const text = svgElement('text', { 'text-anchor': edge.back ? 'end' : 'middle' });
      text.textContent = label;
      labelGroup.append(text);
      group.append(labelGroup);
    }
    return group;
  }

  renderNode(node) {
    const overlay = node.overlay || node.stateOverlay;
    const group = svgElement('g', {
      class: `process-node process-node-${node.compound?.collapsed ? 'compound' : node.type || 'task'}${node.pinned ? ' is-pinned' : ''}`,
      transform: `translate(${node.x} ${node.y})`,
      'data-key': `node:${node.id}`,
      'data-node-id': node.id,
      role: 'button',
      tabindex: '0',
      'aria-label': `${node.label || node.id}, ${node.compound?.collapsed ? 'collapsed compound' : node.type || 'task'}${overlayText(overlay) ? `, ${overlayText(overlay)}` : ''}`,
    });
    renderShape(group, node);
    if (node.type !== 'wait' && node.type !== 'start' && node.type !== 'end') renderText(group, node);
    else renderPeripheralLabel(group, node);
    renderOverlay(group, node);
    renderPorts(group, node);
    return group;
  }

  bindEvents() {
    const signal = this.abort.signal;
    this.fitButton.addEventListener('click', () => this.fitToView(), { signal });
    this.svg.addEventListener('wheel', (event) => this.onWheel(event), { passive: false, signal });
    this.svg.addEventListener('pointerdown', (event) => this.onPointerDown(event), { signal });
    this.svg.addEventListener('pointermove', (event) => this.onPointerMove(event), { signal });
    this.svg.addEventListener('pointerup', (event) => this.onPointerUp(event), { signal });
    this.svg.addEventListener('pointercancel', (event) => this.onPointerUp(event), { signal });
    this.svg.addEventListener('click', (event) => this.onClick(event), { signal });
    this.svg.addEventListener('dblclick', (event) => this.onDoubleClick(event), { signal });
    this.svg.addEventListener('keydown', (event) => this.onKeyDown(event), { signal });
    this.root.addEventListener('dragover', (event) => event.preventDefault(), { signal });
    this.root.addEventListener('drop', (event) => {
      event.preventDefault();
      hook(this.options, 'onCanvasDrop')({ point: this.clientToGraph(event.clientX, event.clientY), event });
    }, { signal });
  }

  eventTarget(event) {
    const node = event.target.closest?.('[data-node-id]');
    const edge = event.target.closest?.('[data-edge-index]');
    const port = event.target.closest?.('[data-port]');
    return { node, edge, port };
  }

  onPointerDown(event) {
    if (event.button !== 0) return;
    const target = this.eventTarget(event);
    const point = this.clientToGraph(event.clientX, event.clientY);
    const mode = target.port ? 'port' : target.node ? 'node' : 'pan';
    this.pointer = {
      id: event.pointerId, mode, startClientX: event.clientX, startClientY: event.clientY,
      startPoint: point, startView: { ...this.view }, nodeID: target.node?.dataset.nodeId,
      port: target.port?.dataset.port,
    };
    this.dragMoved = false;
    this.svg.setPointerCapture?.(event.pointerId);
    if (mode === 'port') {
      event.stopPropagation();
      hook(this.options, 'onPortDragStart')({ nodeId: this.pointer.nodeID, port: this.pointer.port, point, event });
    }
  }

  onPointerMove(event) {
    if (!this.pointer || this.pointer.id !== event.pointerId) return;
    const dx = event.clientX - this.pointer.startClientX;
    const dy = event.clientY - this.pointer.startClientY;
    if (Math.hypot(dx, dy) > 3) this.dragMoved = true;
    const point = this.clientToGraph(event.clientX, event.clientY);
    if (this.pointer.mode === 'pan') {
      this.view.x = this.pointer.startView.x + dx;
      this.view.y = this.pointer.startView.y + dy;
      this.applyView();
    } else if (this.pointer.mode === 'node') {
      const node = this.nodeLayer.querySelector(`[data-node-id="${CSS.escape(this.pointer.nodeID)}"]`);
      const laid = this.layout.nodes.find((candidate) => candidate.id === this.pointer.nodeID);
      if (node && laid) node.setAttribute('transform', `translate(${laid.x + (point.x - this.pointer.startPoint.x)} ${laid.y + (point.y - this.pointer.startPoint.y)})`);
      hook(this.options, 'onNodeDrag')({
        nodeId: this.pointer.nodeID, point,
        delta: { x: point.x - this.pointer.startPoint.x, y: point.y - this.pointer.startPoint.y },
        event,
      });
    } else if (this.pointer.mode === 'port') {
      hook(this.options, 'onPortDragMove')({ nodeId: this.pointer.nodeID, port: this.pointer.port, point, event });
    }
  }

  onPointerUp(event) {
    if (!this.pointer || this.pointer.id !== event.pointerId) return;
    const pointer = this.pointer;
    const point = this.clientToGraph(event.clientX, event.clientY);
    if (pointer.mode === 'port') {
      const target = this.eventTarget(event);
      hook(this.options, 'onPortDragEnd')({
        nodeId: pointer.nodeID, port: pointer.port, point,
        targetNodeId: target.node?.dataset.nodeId || null,
        targetPort: target.port?.dataset.port || null,
        event,
      });
    } else if (pointer.mode === 'node') {
      // Position ownership stays outside the core. Snap the transient drag back
      // unless the hook's caller supplied a new pinned graph through setGraph.
      const node = this.nodeLayer.querySelector(`[data-node-id="${CSS.escape(pointer.nodeID)}"]`);
      const laid = this.layout.nodes.find((candidate) => candidate.id === pointer.nodeID);
      if (node && laid) node.setAttribute('transform', `translate(${laid.x} ${laid.y})`);
    }
    this.suppressClick = this.dragMoved;
    this.svg.releasePointerCapture?.(event.pointerId);
    this.pointer = null;
    // The synthetic click follows pointerup in the same task. Clear on the next
    // task so a completed drag never also selects/activates the dragged node.
    setTimeout(() => {
      this.dragMoved = false;
      this.suppressClick = false;
    }, 0);
  }

  onClick(event) {
    if (this.dragMoved || this.suppressClick) return;
    const target = this.eventTarget(event);
    if (target.port) return;
    if (target.node) {
      const node = this.layout.nodes.find((candidate) => candidate.id === target.node.dataset.nodeId);
      this.select({ type: 'node', id: node.id });
      hook(this.options, 'onNodeClick')({ node, event });
    } else if (target.edge) {
      const edge = this.layout.edges.find((candidate) => candidate.inputIndex === Number(target.edge.dataset.edgeIndex));
      this.select({ type: 'edge', id: edge.inputIndex });
      hook(this.options, 'onEdgeClick')({ edge, event });
    } else {
      this.select(null);
    }
  }

  onDoubleClick(event) {
    const target = this.eventTarget(event);
    if (!target.node) return;
    const node = this.layout.nodes.find((candidate) => candidate.id === target.node.dataset.nodeId);
    hook(this.options, 'onNodeDblClick')({ node, event });
  }

  onKeyDown(event) {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    const target = this.eventTarget(event);
    if (!target.node && !target.edge) return;
    event.preventDefault();
    this.onClick(event);
  }

  onWheel(event) {
    event.preventDefault();
    const rect = this.svg.getBoundingClientRect();
    const cursorX = event.clientX - rect.left;
    const cursorY = event.clientY - rect.top;
    const oldZoom = this.view.k;
    const nextZoom = clamp(oldZoom * Math.exp(-event.deltaY * 0.0015), MIN_ZOOM, MAX_ZOOM);
    const graphX = (cursorX - this.view.x) / oldZoom;
    const graphY = (cursorY - this.view.y) / oldZoom;
    this.view.k = nextZoom;
    this.view.x = cursorX - graphX * nextZoom;
    this.view.y = cursorY - graphY * nextZoom;
    this.applyView();
  }

  clientToGraph(clientX, clientY) {
    const rect = this.svg.getBoundingClientRect();
    return {
      x: (clientX - rect.left - this.view.x) / this.view.k,
      y: (clientY - rect.top - this.view.y) / this.view.k,
    };
  }

  applyView() {
    this.viewport.setAttribute('transform', `translate(${this.view.x} ${this.view.y}) scale(${this.view.k})`);
  }

  fitToView(padding = 44) {
    const rect = this.svg.getBoundingClientRect();
    const bounds = this.layout?.bounds;
    if (!bounds || rect.width <= 0 || rect.height <= 0 || bounds.width <= 0 || bounds.height <= 0) return;
    const zoom = clamp(Math.min((rect.width - padding * 2) / bounds.width, (rect.height - padding * 2) / bounds.height), MIN_ZOOM, MAX_ZOOM);
    this.view.k = zoom;
    this.view.x = (rect.width - bounds.width * zoom) / 2 - bounds.x * zoom;
    this.view.y = (rect.height - bounds.height * zoom) / 2 - bounds.y * zoom;
    this.applyView();
  }

  select(selection) {
    this.selected = selection;
    this.applySelection();
  }

  applySelection() {
    this.root.querySelectorAll('.is-selected').forEach((element) => element.classList.remove('is-selected'));
    if (!this.selected) return;
    if (this.selected.type === 'node') {
      this.nodeLayer.querySelector(`[data-node-id="${CSS.escape(String(this.selected.id))}"]`)?.classList.add('is-selected');
    } else if (this.selected.type === 'edge') {
      this.edgeLayer.querySelector(`[data-edge-index="${Number(this.selected.id)}"]`)?.classList.add('is-selected');
    }
  }

  destroy() {
    if (this.destroyed) return;
    this.destroyed = true;
    this.abort.abort();
    this.container.removeAttribute('data-morph-owned');
    this.container.replaceChildren();
  }
}

export function createProcessGraph(container, graph, options) {
  return new ProcessGraph(container, graph, options);
}
