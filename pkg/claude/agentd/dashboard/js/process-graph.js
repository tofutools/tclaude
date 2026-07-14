// process-graph.js -- shared SVG process graph renderer + interaction shell.
// It owns presentation and pointer mechanics only. Editor/viewer semantics live
// behind hooks; this module never fetches, validates, mutates templates, or
// computes run state.

import { layoutProcessGraph, rerouteProcessLayout } from './process-layout.js';
import {
  makeSelection, nodesInMarquee, normalizeMarquee, selectionContains, selectionItems,
} from './process-selection.js';

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

export function isGraphTypingTarget(target) {
  if (!target || typeof target.closest !== 'function') return false;
  return !!target.closest('input, textarea, select, button, summary, a[href], [role="button"], [contenteditable]:not([contenteditable="false"])');
}

export function normalizeWheelDelta(deltaY, deltaMode = 0, pagePixels = 800) {
  if (!Number.isFinite(deltaY)) return 0;
  if (deltaMode === 1) return deltaY * 24; // DOM_DELTA_LINE (Firefox wheels)
  if (deltaMode === 2) return deltaY * Math.max(1, pagePixels); // DOM_DELTA_PAGE
  return deltaY; // DOM_DELTA_PIXEL
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
  if (overlay.severity) bits.push(`has ${overlay.severity}`);
  if (Array.isArray(overlay.issues) && overlay.issues.length) bits.push(overlay.issues.join('; '));
  return bits.join(', ');
}

function tooltipLines(issues, maxChars = 48) {
  const lines = [];
  for (const issue of issues || []) {
    const words = String(issue).trim().split(/\s+/).filter(Boolean);
    let line = '';
    for (const word of words) {
      if (!line) line = word;
      else if (`${line} ${word}`.length <= maxChars) line += ` ${word}`;
      else {
        lines.push(line);
        line = word;
      }
    }
    if (line) lines.push(line);
  }
  return lines;
}

function issueTexts(issues) {
  return Array.isArray(issues) ? issues.filter(Boolean).map(String) : [];
}

function renderIssueTooltip(parent, issues) {
  const lines = tooltipLines(issues);
  if (!lines.length) return;
  const width = 330;
  const height = 18 + lines.length * 15;
  const tooltip = svgElement('g', {
    class: 'process-overlay-tooltip',
    transform: `translate(${-width - 16} 16)`,
  });
  tooltip.append(svgElement('rect', { x: 0, y: 0, width, height, rx: 6 }));
  const text = svgElement('text', { x: 9, y: 16 });
  lines.forEach((line, index) => {
    const tspan = svgElement('tspan', { x: 9, y: 16 + index * 15 });
    tspan.textContent = line;
    text.append(tspan);
  });
  tooltip.append(text);
  parent.append(tooltip);
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
    class: `process-overlay-anchor${overlay ? ' has-overlay' : ''}${overlay?.severity ? ` overlay-${overlay.severity}` : ''}`,
    transform: `translate(${x} ${y})`,
    'aria-hidden': 'true',
  });
  const issues = issueTexts(overlay?.issues);
  renderIssueTooltip(group, issues);
  group.append(svgElement('circle', { class: 'process-overlay-ring', cx: 0, cy: 0, r: 11 }));
  if (overlay?.glyph) {
    const glyph = svgElement('text', { class: 'process-overlay-glyph', x: 0, y: 4, 'text-anchor': 'middle' });
    glyph.textContent = String(overlay.glyph);
    group.append(glyph);
  }
  const status = overlay?.label || overlay?.status;
  if (status) {
    const statusLabel = svgElement('text', { class: 'process-overlay-status', x: -16, y: 4, 'text-anchor': 'end' });
    statusLabel.textContent = String(status);
    group.append(statusLabel);
  }
  const progress = overlay?.progress;
  const progressText = typeof progress === 'string'
    ? progress
    : progress ? `${progress.current ?? 0}/${progress.total ?? 0}` : '';
  const attempt = overlay?.attempt ?? overlay?.attempts;
  const retry = overlay?.retry ?? overlay?.retries;
  const detail = [progressText, attempt != null ? `#${attempt}` : '', retry != null ? `↻${retry}` : ''].filter(Boolean).join(' ');
  if (detail) {
    const text = svgElement('text', { class: 'process-overlay-detail', x: -16, y: node.height - 20, 'text-anchor': 'end' });
    text.textContent = detail;
    group.append(text);
  }
  if (overlay?.badge) {
    const badge = svgElement('text', { class: 'process-overlay-badge', x: -16, y: status ? 17 : 4, 'text-anchor': 'end' });
    badge.textContent = String(overlay.badge);
    group.append(badge);
  }
  parent.append(group);
}

function renderPorts(parent, node) {
  const input = svgElement('circle', {
    class: 'process-port process-port-in', cx: 0, cy: -node.height / 2, r: 6,
    'data-port': 'in', role: 'button', tabindex: '0', 'aria-pressed': 'false',
    'aria-label': `Input port for ${node.label || node.id}`,
  });
  const output = svgElement('circle', {
    class: 'process-port process-port-out', cx: 0, cy: node.height / 2, r: 6,
    'data-port': 'out', role: 'button', tabindex: '0', 'aria-pressed': 'false',
    'aria-label': `Output port for ${node.label || node.id}`,
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
    this.graph = graph || { nodes: [], edges: [] };
    // Validate/layout before touching the host. A bad draft must leave the
    // existing graph intact and repairable.
    this.layout = layoutProcessGraph(this.graph, this.options.layout || {});
    this.instanceID = nextGraphID++;
    this.markerID = `process-arrow-${this.instanceID}`;
    this.backMarkerID = `process-back-arrow-${this.instanceID}`;
    this.view = { x: 0, y: 0, k: 1 };
    this.selected = null;
    this.pointer = null;
    this.pendingClickTarget = null;
    this.lastClickTarget = null;
    this.transientLayout = null;
    this.dragMoved = false;
    this.suppressClick = false;
    this.keyboardPort = null;
    this.spaceHeld = false;
    this.destroyed = false;
    this.abort = new AbortController();

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
    this.portLayer = svgElement('g', { class: 'process-port-layer', 'data-key': 'ports' });
    this.viewport.append(this.edgeLayer, this.nodeLayer, this.portLayer);
    this.svg.append(this.defs, this.viewport);
    this.controls = htmlElement('div', { class: 'process-graph-controls', 'aria-label': 'Graph view controls' });
    this.fitButton = htmlElement('button', { class: 'process-fit-button', type: 'button', text: 'Fit', title: 'Fit graph to view' });
    this.controls.append(this.fitButton);
    this.root.append(this.svg, this.controls);
    this.bindEvents();
    this.render();
    container.replaceChildren(this.root);
    if (options.fitOnRender !== false) requestAnimationFrame(() => this.fitToView());
  }

  setGraph(graph, { fit = false } = {}) {
    const nextGraph = graph || { nodes: [], edges: [] };
    const nextLayout = layoutProcessGraph(nextGraph, this.options.layout || {});
    this.graph = nextGraph;
    this.layout = nextLayout;
    this.render();
    if (fit) requestAnimationFrame(() => this.fitToView());
    return this.layout;
  }

  setOptions(options = {}) {
    const nextOptions = { ...this.options, ...options };
    const nextLayout = layoutProcessGraph(this.graph, nextOptions.layout || {});
    this.options = nextOptions;
    this.layout = nextLayout;
    this.root.dataset.colorScheme = this.options.colorScheme || 'dark';
    this.render();
    return this.layout;
  }

  render() {
    const focused = this.captureFocus();
    const title = this.svg.querySelector('title');
    title.textContent = this.options.ariaLabel || `Process graph with ${this.layout.nodes.length} nodes`;
    this.renderEdges(this.layout.edges);
    this.nodeLayer.replaceChildren();
    this.portLayer.replaceChildren();
    for (const node of this.layout.nodes) {
      this.nodeLayer.append(this.renderNode(node));
      this.portLayer.append(this.renderPortNode(node));
    }
    this.applyView();
    this.applySelection();
    this.applyKeyboardPort();
    this.restoreFocus(focused);
  }

  renderEdges(edges) {
    this.edgeLayer.replaceChildren(...(edges || []).map((edge) => this.renderEdge(edge)));
  }

  renderEdge(edge) {
    const key = `edge:${edge.id}`;
    const issues = issueTexts(edge.issues);
    const group = svgElement('g', {
      class: `process-edge${edge.back ? ' process-edge-back' : ''}`,
      'data-key': key,
      'data-edge-id': edge.id,
      'data-edge-index': edge.inputIndex,
      'data-from': edge.from,
      'data-to': edge.to,
      role: 'button',
      tabindex: '0',
      'aria-pressed': 'false',
      'aria-label': `${edge.back ? 'Return edge' : 'Edge'} from ${edge.from} to ${edge.to}${edgeLabel(edge) ? `: ${edgeLabel(edge)}` : ''}${issues.length ? `, ${issues.join('; ')}` : ''}`,
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
    // Optional badge glyph at the label anchor (validation and future state
    // decorations). Severity is glyph-coded AND class-coded — never color-only.
    if (edge.badge) {
      const marker = svgElement('g', {
        class: 'process-edge-issue-marker',
        transform: `translate(${edge.label.x} ${edge.label.y - 13})`,
        'aria-hidden': 'true',
      });
      const badge = svgElement('text', {
        class: `process-edge-badge process-edge-badge-${edge.badgeSeverity || 'error'}`,
        x: 0, y: 0, 'text-anchor': edge.back ? 'end' : 'middle',
      });
      badge.textContent = String(edge.badge);
      marker.append(badge);
      renderIssueTooltip(marker, issues);
      group.append(marker);
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
      'aria-pressed': 'false',
      'aria-label': `${node.label || node.id}, ${node.compound?.collapsed ? 'collapsed compound' : node.type || 'task'}${overlayText(overlay) ? `, ${overlayText(overlay)}` : ''}`,
    });
    renderShape(group, node);
    if (node.type !== 'wait' && node.type !== 'start' && node.type !== 'end') renderText(group, node);
    else renderPeripheralLabel(group, node);
    renderOverlay(group, node);
    return group;
  }

  renderPortNode(node) {
    // Ports are siblings of the focusable node button, not descendants. Nested
    // button roles are inconsistently exposed by accessibility trees.
    const group = svgElement('g', {
      class: 'process-node-ports',
      transform: `translate(${node.x} ${node.y})`,
      'data-key': `ports:${node.id}`,
      'data-node-id': node.id,
    });
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
    this.svg.addEventListener('pointercancel', (event) => this.onPointerCancel(event), { signal });
    this.svg.addEventListener('pointerleave', () => this.updatePortHover(null), { signal });
    this.svg.addEventListener('click', (event) => this.onClick(event), { signal });
    this.svg.addEventListener('dblclick', (event) => this.onDoubleClick(event), { signal });
    this.svg.addEventListener('keydown', (event) => this.onKeyDown(event), { signal });
    document.addEventListener('keydown', (event) => this.onSpaceKey(event), { signal });
    document.addEventListener('keyup', (event) => this.onSpaceKey(event), { signal });
    window.addEventListener('blur', () => this.setSpaceHeld(false), { signal });
    this.root.addEventListener('dragover', (event) => event.preventDefault(), { signal });
    this.root.addEventListener('drop', (event) => {
      event.preventDefault();
      hook(this.options, 'onCanvasDrop')({ point: this.clientToGraph(event.clientX, event.clientY), event });
    }, { signal });
  }

  eventTarget(event) {
    let node = event.target?.closest?.('[data-node-id]');
    let edge = event.target?.closest?.('[data-edge-index]');
    let port = event.target?.closest?.('[data-port]');
    // Multiple editor/viewer graphs may share a page. A pointer captured by
    // this SVG must never accept a node or port belonging to another instance.
    if (node && !this.nodeLayer.contains(node) && !this.portLayer.contains(node)) node = null;
    if (edge && !this.edgeLayer.contains(edge)) edge = null;
    if (port && !this.portLayer.contains(port)) port = null;
    return { node, edge, port };
  }

  updatePortHover(event) {
    const target = event ? this.eventTarget(event) : { node: null };
    const nodeID = target.node?.dataset.nodeId || null;
    this.portLayer.querySelectorAll('.process-node-ports').forEach((ports) => {
      ports.classList.toggle('is-node-hover', ports.dataset.nodeId === nodeID);
    });
  }

  onPointerDown(event) {
    if (this.pointer) return;
    const middle = event.button === 1;
    if (event.button !== 0 && !middle) return;
    // Resolve the target before focus: focusing the graph blurs an inspector
    // input, whose synchronous change handler may refresh and replace every
    // SVG layer child. The detached original still carries the stable ids we
    // need to classify this gesture.
    const target = this.eventTarget(event);
    // Empty SVG space is not natively focusable. Explicitly focus the graph so
    // editor shortcuts bubble through its root after a canvas click instead of
    // acting on whichever palette/control happened to be focused previously.
    this.root.focus({ preventScroll: true });
    event.preventDefault();
    const point = this.clientToGraph(event.clientX, event.clientY);
    // Touch/pen have no middle button. Preserve their empty-canvas navigation
    // while still letting a primary pointer drag nodes and ports normally.
    const directPan = middle || (this.spaceHeld && event.button === 0) || (!target.node && !target.port
      && (event.pointerType === 'touch' || event.pointerType === 'pen'));
    const mode = directPan ? 'pan'
      : target.port ? 'port'
        : target.node ? 'node'
          : target.edge ? 'edge'
          : this.options.marqueeSelect ? 'marquee' : 'pan';
    const nodeID = target.node?.dataset.nodeId;
    const selectedNodes = selectionItems(this.selected)
      .filter((item) => item.type === 'node')
      .map((item) => item.id);
    const nodeIDs = mode === 'node' && selectionContains(this.selected, { type: 'node', id: nodeID })
      ? selectedNodes : nodeID ? [nodeID] : [];
    this.pointer = {
      id: event.pointerId, mode, startClientX: event.clientX, startClientY: event.clientY,
      startPoint: point, startView: { ...this.view }, nodeID, nodeIDs,
      edgeID: target.edge?.dataset.edgeId, port: target.port?.dataset.port,
      selectionStarted: false,
    };
    this.dragMoved = false;
    this.svg.setPointerCapture?.(event.pointerId);
    if (mode === 'port') {
      event.stopPropagation();
      this.clearKeyboardPort();
      hook(this.options, 'onPortDragStart')({ nodeId: this.pointer.nodeID, port: this.pointer.port, point, event });
    } else if (mode === 'marquee') {
      this.marquee = svgElement('rect', { class: 'process-marquee', x: point.x, y: point.y, width: 0, height: 0 });
      this.viewport.append(this.marquee);
    }
  }

  onPointerMove(event) {
    this.updatePortHover(event);
    if (!this.pointer || this.pointer.id !== event.pointerId) return;
    const dx = event.clientX - this.pointer.startClientX;
    const dy = event.clientY - this.pointer.startClientY;
    if (Math.hypot(dx, dy) > 3 && !this.dragMoved) {
      this.dragMoved = true;
      if (this.pointer.mode === 'node'
        && !selectionContains(this.selected, { type: 'node', id: this.pointer.nodeID })) {
        const selection = { type: 'node', id: this.pointer.nodeID };
        this.pointer.selectionStarted = true;
        this.select(selection);
        hook(this.options, 'onNodeDragStart')({ nodeId: this.pointer.nodeID, selection, event });
      }
    }
    const point = this.clientToGraph(event.clientX, event.clientY);
    if (this.pointer.mode === 'pan') {
      this.view.x = this.pointer.startView.x + dx;
      this.view.y = this.pointer.startView.y + dy;
      this.applyView();
    } else if (this.pointer.mode === 'node') {
      for (const nodeID of this.pointer.nodeIDs) {
        const node = this.nodeLayer.querySelector(`[data-node-id="${CSS.escape(nodeID)}"]`);
        const ports = this.portLayer.querySelector(`[data-node-id="${CSS.escape(nodeID)}"]`);
        const laid = this.layout.nodes.find((candidate) => candidate.id === nodeID);
        if (node && laid) {
          const transform = `translate(${laid.x + (point.x - this.pointer.startPoint.x)} ${laid.y + (point.y - this.pointer.startPoint.y)})`;
          node.setAttribute('transform', transform);
          ports?.setAttribute('transform', transform);
        }
      }
      hook(this.options, 'onNodeDrag')({
        nodeId: this.pointer.nodeID, nodeIds: [...this.pointer.nodeIDs], point,
        delta: { x: point.x - this.pointer.startPoint.x, y: point.y - this.pointer.startPoint.y },
        event,
      });
      this.renderTransientEdges(this.pointer.nodeIDs, {
        x: point.x - this.pointer.startPoint.x,
        y: point.y - this.pointer.startPoint.y,
      });
    } else if (this.pointer.mode === 'port') {
      hook(this.options, 'onPortDragMove')({ nodeId: this.pointer.nodeID, port: this.pointer.port, point, event });
    } else if (this.pointer.mode === 'marquee') {
      const box = normalizeMarquee(this.pointer.startPoint, point);
      this.marquee?.setAttribute('x', box.left);
      this.marquee?.setAttribute('y', box.top);
      this.marquee?.setAttribute('width', box.right - box.left);
      this.marquee?.setAttribute('height', box.bottom - box.top);
    }
  }

  onPointerUp(event) {
    if (!this.pointer || this.pointer.id !== event.pointerId) return;
    const pointer = this.pointer;
    const point = this.clientToGraph(event.clientX, event.clientY);
    if (pointer.mode === 'port') {
      // Pointer capture retargets pointerup to the SVG, so event.target cannot
      // identify the node/port actually under the cursor. Hit-test the document
      // at release time before notifying the editor consumer.
      const hit = document.elementFromPoint(event.clientX, event.clientY);
      const target = this.eventTarget({ target: hit });
      hook(this.options, 'onPortDragEnd')({
        nodeId: pointer.nodeID, port: pointer.port, point,
        targetNodeId: target.node?.dataset.nodeId || null,
        targetPort: target.port?.dataset.port || null,
        event,
      });
    } else if (pointer.mode === 'node') {
      // Position ownership stays outside the core. Snap the transient drag back
      // unless the hook's caller supplied a new pinned graph through setGraph.
      this.snapNodesHome(pointer.nodeIDs || [pointer.nodeID]);
      this.restoreTransientEdges();
    } else if (pointer.mode === 'marquee') {
      this.marquee?.remove();
      this.marquee = null;
      if (this.dragMoved) {
        const items = nodesInMarquee(this.layout.nodes, pointer.startPoint, point)
          .map((node) => ({ type: 'node', id: node.id }));
        const selection = makeSelection(items);
        this.select(selection);
        hook(this.options, 'onMarqueeSelect')({ selection, items, event });
      }
    }
    // A pan gesture over an item is navigation even if the pointer did not
    // move. Do not let its synthetic click select the item underneath it, but
    // preserve ordinary empty-canvas clicks so the viewer can clear selection.
    const panOverItem = pointer.mode === 'pan'
      && !!(pointer.nodeID || pointer.edgeID || pointer.port);
    this.suppressClick = this.dragMoved || panOverItem;
    this.pendingClickTarget = this.suppressClick ? null : {
      mode: pointer.mode, nodeID: pointer.nodeID || null,
      edgeID: pointer.edgeID || null, port: pointer.port || null,
    };
    this.svg.releasePointerCapture?.(event.pointerId);
    this.pointer = null;
    // The synthetic click follows pointerup in the same task. Clear on the next
    // task so a completed drag never also selects/activates the dragged node.
    setTimeout(() => {
      this.dragMoved = false;
      this.suppressClick = false;
      this.pendingClickTarget = null;
    }, 0);
  }

  // onPointerCancel: the browser aborted the gesture (touch scroll takeover,
  // pen leaving range, pointer grabbed away). Nothing may commit: a port drag
  // ends with cancelled: true and NO hit-testing — a cancelled drag whose last
  // position happens to sit over another node must never read as a deliberate
  // drop — a node drag snaps home, and a pan simply stops where it is.
  onPointerCancel(event) {
    if (!this.pointer || this.pointer.id !== event.pointerId) return;
    const pointer = this.pointer;
    this.pointer = null;
    this.svg.releasePointerCapture?.(event.pointerId);
    if (pointer.mode === 'port') {
      hook(this.options, 'onPortDragEnd')({
        nodeId: pointer.nodeID, port: pointer.port,
        point: this.clientToGraph(event.clientX, event.clientY),
        targetNodeId: null, targetPort: null, cancelled: true, event,
      });
    } else if (pointer.mode === 'node') {
      this.snapNodesHome(pointer.nodeIDs || [pointer.nodeID]);
      this.restoreTransientEdges();
    } else if (pointer.mode === 'marquee') {
      this.marquee?.remove();
      this.marquee = null;
    }
    this.suppressClick = this.dragMoved;
    this.pendingClickTarget = null;
    setTimeout(() => {
      this.dragMoved = false;
      this.suppressClick = false;
    }, 0);
  }

  snapNodeHome(nodeID) {
    const node = this.nodeLayer.querySelector(`[data-node-id="${CSS.escape(nodeID)}"]`);
    const ports = this.portLayer.querySelector(`[data-node-id="${CSS.escape(nodeID)}"]`);
    const laid = this.layout.nodes.find((candidate) => candidate.id === nodeID);
    if (node && laid) {
      const transform = `translate(${laid.x} ${laid.y})`;
      node.setAttribute('transform', transform);
      ports?.setAttribute('transform', transform);
    }
  }

  snapNodesHome(nodeIDs) {
    for (const nodeID of nodeIDs || []) this.snapNodeHome(nodeID);
  }

  renderTransientEdges(nodeIDs, delta) {
    const positions = new Map();
    for (const nodeID of nodeIDs || []) {
      const laid = this.layout.nodes.find((node) => node.id === nodeID);
      if (laid) positions.set(nodeID, { x: laid.x + delta.x, y: laid.y + delta.y });
    }
    this.transientLayout = rerouteProcessLayout(this.layout, positions, this.options.layout || {});
    this.renderEdges(this.transientLayout.edges);
    this.applySelection();
  }

  restoreTransientEdges() {
    if (!this.transientLayout) return;
    this.transientLayout = null;
    this.renderEdges(this.layout.edges);
    this.applySelection();
  }

  onClick(event) {
    if (this.dragMoved || this.suppressClick) return;
    const pending = this.pendingClickTarget;
    this.pendingClickTarget = null;
    const target = pending ? {
      nodeID: pending.nodeID, edgeID: pending.edgeID, port: pending.port,
    } : (() => {
      const hit = this.eventTarget(event);
      return {
        nodeID: hit.node?.dataset.nodeId || null,
        edgeID: hit.edge?.dataset.edgeId || null,
        port: hit.port?.dataset.port || null,
      };
    })();
    this.lastClickTarget = target;
    if (target.port) return;
    if (target.nodeID) {
      const node = this.layout.nodes.find((candidate) => candidate.id === target.nodeID);
      if (!node) return;
      this.select({ type: 'node', id: node.id });
      hook(this.options, 'onNodeClick')({ node, event });
    } else if (target.edgeID) {
      const edge = this.layout.edges.find((candidate) => candidate.id === target.edgeID);
      if (!edge) return;
      this.select({ type: 'edge', id: edge.id });
      hook(this.options, 'onEdgeClick')({ edge, event });
    } else {
      this.select(null);
      hook(this.options, 'onCanvasClick')({ event });
    }
  }

  onDoubleClick(event) {
    const hit = this.eventTarget(event);
    const nodeID = hit.node?.dataset.nodeId || this.lastClickTarget?.nodeID;
    if (!nodeID) return;
    const node = this.layout.nodes.find((candidate) => candidate.id === nodeID);
    if (!node) return;
    hook(this.options, 'onNodeDblClick')({ node, event });
  }

  onKeyDown(event) {
    const target = this.eventTarget(event);
    if (event.key === 'Escape' && this.keyboardPort) {
      event.preventDefault();
      const source = this.keyboardPort;
      this.clearKeyboardPort();
      hook(this.options, 'onPortDragEnd')({
        nodeId: source.nodeId, port: source.port, point: source.point,
        targetNodeId: null, targetPort: null, keyboard: true, cancelled: true, event,
      });
      return;
    }
    if (event.key !== 'Enter' && event.key !== ' ') return;
    if (target.port && target.node) {
      event.preventDefault();
      const nodeId = target.node.dataset.nodeId;
      const port = target.port.dataset.port;
      const node = this.layout.nodes.find((candidate) => candidate.id === nodeId);
      const point = { x: node.x, y: node.y + (port === 'out' ? node.height / 2 : -node.height / 2) };
      if (!this.keyboardPort) {
        this.keyboardPort = { nodeId, port, point };
        this.applyKeyboardPort();
        hook(this.options, 'onPortDragStart')({ nodeId, port, point, keyboard: true, event });
      } else {
        const source = this.keyboardPort;
        this.clearKeyboardPort();
        hook(this.options, 'onPortDragEnd')({
          nodeId: source.nodeId, port: source.port, point,
          targetNodeId: nodeId, targetPort: port, keyboard: true, event,
        });
      }
      return;
    }
    if (!target.node && !target.edge) return;
    event.preventDefault();
    this.onClick(event);
  }

  onSpaceKey(event) {
    if (event.key !== ' ' && event.code !== 'Space') return;
    if (event.type === 'keyup') {
      this.setSpaceHeld(false);
      return;
    }
    if (event.defaultPrevented || event.repeat || this.pointer || isGraphTypingTarget(event.target)) return;
    const ownsKey = this.root.contains(event.target) || this.root.matches(':hover');
    if (!ownsKey) return;
    event.preventDefault();
    this.setSpaceHeld(true);
  }

  setSpaceHeld(held) {
    this.spaceHeld = !!held;
    this.root.classList.toggle('is-space-pan', this.spaceHeld);
  }

  onWheel(event) {
    event.preventDefault();
    const rect = this.svg.getBoundingClientRect();
    if (this.options.wheelPan && !event.ctrlKey) {
      const deltaX = normalizeWheelDelta(event.deltaX, event.deltaMode, rect.width);
      const deltaY = normalizeWheelDelta(event.deltaY, event.deltaMode, rect.height);
      if (event.shiftKey && !deltaX) this.view.x -= deltaY;
      else {
        this.view.x -= deltaX;
        this.view.y -= deltaY;
      }
      this.applyView();
      return;
    }
    const cursorX = event.clientX - rect.left;
    const cursorY = event.clientY - rect.top;
    const oldZoom = this.view.k;
    const delta = normalizeWheelDelta(event.deltaY, event.deltaMode, rect.height);
    const nextZoom = clamp(oldZoom * Math.exp(-delta * 0.0015), MIN_ZOOM, MAX_ZOOM);
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

  // centerOn pans the view so graph point (x, y) sits at the viewport center,
  // keeping the current zoom. Used by consumers that jump to a node/edge
  // (e.g. the editor's issues panel).
  centerOn(x, y) {
    const rect = this.svg.getBoundingClientRect();
    if (rect.width <= 0 || rect.height <= 0) return;
    this.view.x = rect.width / 2 - x * this.view.k;
    this.view.y = rect.height / 2 - y * this.view.k;
    this.applyView();
  }

  select(selection) {
    this.selected = makeSelection(selectionItems(selection));
    this.applySelection();
  }

  applySelection() {
    this.root.querySelectorAll('.process-node, .process-edge').forEach((element) => {
      element.classList.remove('is-selected');
      element.setAttribute('aria-pressed', 'false');
    });
    for (const item of selectionItems(this.selected)) {
      let selected = null;
      if (item.type === 'node') {
        selected = this.nodeLayer.querySelector(`[data-node-id="${CSS.escape(String(item.id))}"]`);
      } else if (item.type === 'edge') {
        selected = this.edgeLayer.querySelector(`[data-edge-id="${CSS.escape(String(item.id))}"]`);
      }
      selected?.classList.add('is-selected');
      selected?.setAttribute('aria-pressed', 'true');
    }
  }

  applyKeyboardPort() {
    this.portLayer.querySelectorAll('.process-port').forEach((port) => {
      port.classList.remove('is-keyboard-source');
      port.setAttribute('aria-pressed', 'false');
    });
    if (!this.keyboardPort) return;
    const source = this.portLayer.querySelector(
      `[data-node-id="${CSS.escape(this.keyboardPort.nodeId)}"] [data-port="${this.keyboardPort.port}"]`,
    );
    if (!source) {
      this.keyboardPort = null;
      return;
    }
    source.classList.add('is-keyboard-source');
    source.setAttribute('aria-pressed', 'true');
  }

  clearKeyboardPort() {
    this.keyboardPort = null;
    this.applyKeyboardPort();
  }

  captureFocus() {
    const active = document.activeElement;
    if (!active || !this.root.contains(active)) return null;
    const port = active.closest?.('[data-port]');
    const node = active.closest?.('[data-node-id]');
    const edge = active.closest?.('[data-edge-index]');
    if (port && node) return { type: 'port', nodeId: node.dataset.nodeId, port: port.dataset.port };
    if (node) return { type: 'node', nodeId: node.dataset.nodeId };
    if (edge) return { type: 'edge', edgeId: edge.dataset.edgeId };
    return null;
  }

  restoreFocus(focused) {
    if (!focused) return;
    let target;
    if (focused.type === 'port') {
      target = this.portLayer.querySelector(
        `[data-node-id="${CSS.escape(focused.nodeId)}"] [data-port="${focused.port}"]`,
      );
    } else if (focused.type === 'node') {
      target = this.nodeLayer.querySelector(`[data-node-id="${CSS.escape(focused.nodeId)}"]`);
    } else {
      target = this.edgeLayer.querySelector(`[data-edge-id="${CSS.escape(focused.edgeId)}"]`);
    }
    target?.focus({ preventScroll: true });
  }

  destroy() {
    if (this.destroyed) return;
    this.destroyed = true;
    this.abort.abort();
    this.container.replaceChildren();
  }
}

export function createProcessGraph(container, graph, options) {
  return new ProcessGraph(container, graph, options);
}
