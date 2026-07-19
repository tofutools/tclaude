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

export function interactionNode(nodes, candidateID) {
  if (candidateID == null) return null;
  const id = String(candidateID);
  return (nodes || []).find((node) => String(node.id) === id) || null;
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

function overlayText(overlay, issues = []) {
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
  if (issues.length) bits.push(issues.join('; '));
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

function overlayPresentation(overlay) {
  const issues = issueTexts(overlay?.issues);
  const description = overlayText(overlay, issues);
  if (!description) return null;
  // One presentation object gates the marker and supplies both disclosure
  // channels, so tooltip content and the node's accessible name stay aligned.
  return { overlay, description, issues };
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

function labelUnits(value) {
  const text = String(value || '');
  if (typeof Intl?.Segmenter === 'function') {
    return Array.from(new Intl.Segmenter(undefined, { granularity: 'grapheme' }).segment(text), ({ segment }) => segment);
  }
  return Array.from(text);
}

function labelUnitWidth(unit) {
  // Conservative em-relative buckets keep wrapping deterministic before the
  // disconnected SVG text can be measured. Wide Latin glyphs are not allowed
  // to consume the ordinary half-em budget; CJK, emoji, combining clusters,
  // and the ellipsis receive a full-em allowance. The clip rectangle remains
  // the final hard geometry boundary, while Chrome dashsnap verifies that each
  // emitted line fits without relying on that clip.
  if (Array.from(unit).some((character) => character.codePointAt(0) > 0xff)) return 1.1;
  if (/^[WM@#%&QOGmw]$/u.test(unit)) return 1.15;
  if (/^[A-Z]$/u.test(unit)) return 0.8;
  if (/^[ilI1.,'`:;|!]$/u.test(unit)) return 0.4;
  if (/^\s$/u.test(unit)) return 0.35;
  return 0.65;
}

function labelWidth(units) {
  return units.reduce((total, unit) => total + labelUnitWidth(unit), 0);
}

function takeLabelPrefix(units, budget) {
  let width = 0;
  let count = 0;
  while (count < units.length) {
    const next = labelUnitWidth(units[count]);
    if (count > 0 && width + next > budget) break;
    width += next;
    count += 1;
  }
  return units.splice(0, count || 1);
}

function wrapLabel(label, maxUnits, maxLines) {
  const words = String(label || '').trim().split(/\s+/u).filter(Boolean).map(labelUnits);
  if (!words.length) return [''];
  const lines = [];
  let line = [];
  let truncated = false;
  while (words.length) {
    if (lines.length >= maxLines) {
      truncated = true;
      break;
    }
    const word = words[0];
    if (!line.length) {
      if (labelWidth(word) <= maxUnits) {
        line = word.splice(0);
        words.shift();
      } else {
        line = takeLabelPrefix(word, maxUnits);
        if (!word.length) words.shift();
        lines.push(line.join(''));
        line = [];
      }
      continue;
    }
    if (labelWidth(line) + 1 + labelWidth(word) <= maxUnits) {
      line.push(' ', ...word);
      words.shift();
    } else {
      lines.push(line.join(''));
      line = [];
    }
  }
  if (line.length && lines.length < maxLines) {
    lines.push(line.join(''));
    line = [];
  }
  if (words.length || line.length) truncated = true;
  if (truncated && lines.length) {
    const last = labelUnits(lines.at(-1));
    const ellipsisWidth = labelUnitWidth('…');
    while (last.length && labelWidth(last) + ellipsisWidth > maxUnits) last.pop();
    lines[lines.length - 1] = `${last.join('').trimEnd()}…`;
  }
  return lines.slice(0, maxLines);
}

function insideLabelLayout(node) {
  if (node.compound?.collapsed) {
    return { x: -78, y: -31, width: 156, height: 42, centerY: -10, maxUnits: 12, maxLines: 2, lineHeight: 15, className: 'process-node-label-compound' };
  }
  switch (node.type) {
    case 'decision':
      return { x: -32, y: -18, width: 64, height: 36, centerY: 0, maxUnits: 5.7, maxLines: 2, lineHeight: 14, className: 'process-node-label-compact' };
    case 'parallel':
      return { x: -29, y: 5, width: 58, height: 24, centerY: 17, maxUnits: 5.2, maxLines: 1, lineHeight: 12, className: 'process-node-label-compact' };
    case 'wait':
      return { x: -28, y: 5, width: 56, height: 25, centerY: 17, maxUnits: 5, maxLines: 1, lineHeight: 12, className: 'process-node-label-compact' };
    case 'start':
      return { x: -20, y: -15, width: 40, height: 30, centerY: 0, maxUnits: 4, maxLines: 2, lineHeight: 11, className: 'process-node-label-small' };
    case 'end':
      return { x: -21, y: -15, width: 42, height: 30, centerY: 0, maxUnits: 4.2, maxLines: 2, lineHeight: 11, className: 'process-node-label-small' };
    default:
      return { x: -70, y: -25, width: 140, height: 50, centerY: 0, maxUnits: 10.2, maxLines: 3, lineHeight: 16, className: '' };
  }
}

function renderText(parent, node, clipID) {
  const layout = insideLabelLayout(node);
  const lines = wrapLabel(node.label || node.id, layout.maxUnits, layout.maxLines);
  const clip = svgElement('clipPath', {
    id: clipID, class: 'process-node-label-clip', clipPathUnits: 'userSpaceOnUse', 'aria-hidden': 'true',
  });
  clip.append(svgElement('rect', {
    x: layout.x, y: layout.y, width: layout.width, height: layout.height,
  }));
  const text = svgElement('text', {
    class: `process-node-label process-node-label-inside${layout.className ? ` ${layout.className}` : ''}`,
    'text-anchor': 'middle', 'aria-hidden': 'true', 'clip-path': `url(#${clipID})`,
    'data-label-max-lines': layout.maxLines,
  });
  const startY = layout.centerY + 4 - (lines.length - 1) * layout.lineHeight / 2;
  lines.forEach((line, index) => {
    const tspan = svgElement('tspan', { x: 0, y: startY + index * layout.lineHeight });
    tspan.textContent = line;
    text.append(tspan);
  });
  parent.append(clip, text);
}

function renderClock(parent, y = 0) {
  const clock = svgElement('g', { class: 'process-clock', transform: `translate(0 ${y})`, 'aria-hidden': 'true' });
  clock.append(
    svgElement('circle', { class: 'process-clock-face', cx: 0, cy: 0, r: 12 }),
    svgElement('path', { class: 'process-clock-hand', d: 'M 0 -7 L 0 0 L 7 4' }),
  );
  parent.append(clock);
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
    case 'parallel':
      parent.append(svgElement('polygon', {
        class: 'process-node-shape process-shape-parallel',
        points: `0,${-node.height / 2} ${node.width / 2},0 0,${node.height / 2} ${-node.width / 2},0`,
      }));
      parent.append(
        svgElement('path', { class: 'process-parallel-mark', d: 'M -10 -13 H 10 M 0 -23 V -3' }),
      );
      break;
    case 'wait':
      parent.append(svgElement('circle', { class: 'process-node-shape process-shape-wait', cx: 0, cy: 0, r: node.width / 2 }));
      renderClock(parent, -13);
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

function renderOverlay(parent, node, presentation) {
  if (!presentation) return;
  const { overlay, issues } = presentation;
  const x = node.width / 2 - 9;
  const y = -node.height / 2 + 9;
  const group = svgElement('g', {
    class: `process-overlay-anchor has-overlay${overlay.severity ? ` overlay-${overlay.severity}` : ''}`,
    transform: `translate(${x} ${y})`,
    'aria-hidden': 'true',
  });
  renderIssueTooltip(group, issues);
  group.append(svgElement('circle', { class: 'process-overlay-ring', cx: 0, cy: 0, r: 11 }));
  if (overlay.glyph) {
    const glyph = svgElement('text', { class: 'process-overlay-glyph', x: 0, y: 4, 'text-anchor': 'middle' });
    glyph.textContent = String(overlay.glyph);
    group.append(glyph);
  }
  const status = overlay.label || overlay.status;
  if (status) {
    const statusLabel = svgElement('text', { class: 'process-overlay-status', x: -16, y: 4, 'text-anchor': 'end' });
    statusLabel.textContent = String(status);
    group.append(statusLabel);
  }
  const progress = overlay.progress;
  const progressText = typeof progress === 'string'
    ? progress
    : progress ? `${progress.current ?? 0}/${progress.total ?? 0}` : '';
  const attempt = overlay.attempt ?? overlay.attempts;
  const retry = overlay.retry ?? overlay.retries;
  const detail = [progressText, attempt != null ? `#${attempt}` : '', retry != null ? `↻${retry}` : ''].filter(Boolean).join(' ');
  if (detail) {
    const text = svgElement('text', { class: 'process-overlay-detail', x: -16, y: node.height - 20, 'text-anchor': 'end' });
    text.textContent = detail;
    group.append(text);
  }
  if (overlay.badge) {
    const badge = svgElement('text', { class: 'process-overlay-badge', x: -16, y: status ? 17 : 4, 'text-anchor': 'end' });
    badge.textContent = String(overlay.badge);
    group.append(badge);
  }
  parent.append(group);
}

function renderPorts(parent, node, feedbackFor) {
  const portAttributes = (port) => {
    const label = `${port === 'in' ? 'Input' : 'Output'} port for ${node.label || node.id}`;
    const feedback = feedbackFor?.(port);
    if (!feedback) return { class: `process-port process-port-${port}`, label };
    return {
      class: `process-port process-port-${port}${feedback.enabled === false ? ' is-action-disabled' : ''}`,
      label: `${label}. ${feedback.message}`, baseLabel: label,
      state: feedback.state,
      enabled: feedback.enabled !== false,
      message: feedback.message,
    };
  };
  if (node.portAvailability?.in !== false) {
    const inputFeedback = portAttributes('in');
    parent.append(svgElement('circle', {
      class: inputFeedback.class, cx: 0, cy: -node.height / 2, r: 6,
      'data-port': 'in', role: 'button', tabindex: '0', 'aria-pressed': 'false',
      'aria-label': inputFeedback.label, 'data-base-aria-label': inputFeedback.baseLabel || inputFeedback.label,
      'data-source-aria-label': inputFeedback.label,
      'aria-disabled': inputFeedback.enabled === undefined ? undefined : String(!inputFeedback.enabled),
      'data-source-state': inputFeedback.state, 'data-feedback-message': inputFeedback.message,
    }));
  }
  if (node.portAvailability?.out !== false) {
    const outputFeedback = portAttributes('out');
    parent.append(svgElement('circle', {
      class: outputFeedback.class, cx: 0, cy: node.height / 2, r: 6,
      'data-port': 'out', role: 'button', tabindex: '0', 'aria-pressed': 'false',
      'aria-label': outputFeedback.label, 'data-base-aria-label': outputFeedback.baseLabel || outputFeedback.label,
      'data-source-aria-label': outputFeedback.label,
      'aria-disabled': outputFeedback.enabled === undefined ? undefined : String(!outputFeedback.enabled),
      'data-source-state': outputFeedback.state, 'data-feedback-message': outputFeedback.message,
    }));
  }
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
    this.connectionSource = null;
    this.feedbackTarget = null;
    this.feedbackTimer = null;
    this.feedbackFrame = null;
    this.pendingFeedbackPointer = null;
    this.feedbackEnabled = typeof options.connectionFeedback === 'function';
    this.connectionEvaluation = null;
    this.keyboardCancellationFocus = null;
    this.keyboardFocusOwned = false;
    this.keyboardFocusModality = false;
    // Interaction layering is presentation-only. The canonical node/port DOM
    // stays in deterministic semantic order for native keyboard traversal;
    // one aria-hidden paint/hit copy supplies the visual front without an
    // accumulating z-index or model metadata.
    this.frontNodeID = null;
    this.destroyed = false;
    this.abort = new AbortController();

    this.root = htmlElement('div', {
      class: 'process-graph',
      tabindex: options.rootTabIndex ?? '0',
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
    this.frontNodeLayer = svgElement('g', {
      class: 'process-front-node-layer', 'data-key': 'front-node', 'aria-hidden': 'true',
    });
    this.portLayer = svgElement('g', { class: 'process-port-layer', 'data-key': 'ports' });
    this.frontPortLayer = svgElement('g', {
      class: 'process-front-port-layer', 'data-key': 'front-ports', 'aria-hidden': 'true',
    });
    this.viewport.append(
      this.edgeLayer, this.nodeLayer, this.frontNodeLayer, this.portLayer, this.frontPortLayer,
    );
    this.svg.append(this.defs, this.viewport);
    this.controls = htmlElement('div', { class: 'process-graph-controls', 'aria-label': 'Graph view controls' });
    this.fitButton = htmlElement('button', { class: 'process-fit-button', type: 'button', text: 'Fit', title: 'Fit graph to view' });
    this.controls.append(this.fitButton);
    this.root.append(this.svg, this.controls);
    if (this.feedbackEnabled) {
      this.actionTooltip = htmlElement('div', {
        class: 'process-action-tooltip', role: 'tooltip', id: `process-action-tooltip-${this.instanceID}`,
      });
      this.actionTooltip.setAttribute('aria-hidden', 'true');
      this.root.append(this.actionTooltip);
    }
    this.bindEvents();
    this.render();
    container.replaceChildren(this.root);
    if (options.fitOnRender !== false) requestAnimationFrame(() => this.fitToView());
  }

  setGraph(graph, { fit = false, resetInteractionLayering = false } = {}) {
    const nextGraph = graph || { nodes: [], edges: [] };
    const nextLayout = layoutProcessGraph(nextGraph, this.options.layout || {});
    this.graph = nextGraph;
    this.layout = nextLayout;
    if (resetInteractionLayering
        || !interactionNode(this.layout.nodes, this.frontNodeID)) this.frontNodeID = null;
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
    const feedbackResume = this.pointer?.mode === 'port' && this.actionTooltip?.textContent
      ? {
        message: this.actionTooltip.textContent,
        state: this.actionTooltip.dataset.state,
        visible: this.actionTooltip.classList.contains('is-visible'),
        remaining: Math.max(0, Number(this.options.actionFeedbackDelay ?? 750)
          - (Date.now() - (this.feedbackStartedAt || Date.now()))),
      } : null;
    this.clearActionFeedback();
    this.feedbackResume = feedbackResume;
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
    this.renderFrontNode();
    this.applyView();
    this.applySelection();
    this.cancelMissingPointerPort();
    this.connectionEvaluation = this.connectionSource
      ? this.options.connectionFeedbackPreparation?.() || null : null;
    this.applyKeyboardPort();
    this.applyConnectionFeedback();
    this.restoreFocus(focused);
    if (this.pointer?.mode === 'port') {
      this.queuePointerFeedback({
        clientX: this.pointer.lastClientX, clientY: this.pointer.lastClientY,
        pointerType: this.pointer.pointerType, target: this.svg,
      });
    }
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
    const overlay = overlayPresentation(node.overlay || node.stateOverlay);
    const group = svgElement('g', {
      class: `process-node process-node-${node.compound?.collapsed ? 'compound' : node.type || 'task'}${node.pinned ? ' is-pinned' : ''}`,
      transform: `translate(${node.x} ${node.y})`,
      'data-key': `node:${node.id}`,
      'data-node-id': node.id,
      role: 'button',
      tabindex: '0',
      'aria-pressed': 'false',
      'aria-label': `${node.label || node.id}, ${node.compound?.collapsed ? 'collapsed compound' : node.type || 'task'}${overlay ? `, ${overlay.description}` : ''}`,
    });
    group.dataset.baseAriaLabel = group.getAttribute('aria-label');
    renderShape(group, node);
    const labelSerial = this.labelSerial = (this.labelSerial || 0) + 1;
    renderText(group, node, `process-node-label-clip-${this.instanceID}-${labelSerial}`);
    renderOverlay(group, node, overlay);
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
    renderPorts(group, node, this.feedbackEnabled ? (port) => this.connectionFeedback({
      phase: 'source', source: { nodeId: node.id, port },
    }) : null);
    return group;
  }

  renderFrontNode() {
    this.frontNodeLayer.replaceChildren();
    this.frontPortLayer.replaceChildren();
    const node = interactionNode(this.layout.nodes, this.frontNodeID);
    if (!node || !this.options.interactionLayering) return;
    const visual = this.renderNode(node);
    visual.classList.add('process-front-copy');
    visual.removeAttribute('role');
    visual.removeAttribute('tabindex');
    visual.removeAttribute('aria-label');
    visual.removeAttribute('aria-pressed');
    visual.setAttribute('focusable', 'false');
    const ports = this.renderPortNode(node);
    ports.classList.add('process-front-copy');
    ports.querySelectorAll('[role], [tabindex], [aria-label], [aria-pressed], [aria-disabled]').forEach((port) => {
      port.removeAttribute('role');
      port.removeAttribute('tabindex');
      port.removeAttribute('aria-label');
      port.removeAttribute('aria-pressed');
      port.removeAttribute('aria-disabled');
      port.setAttribute('focusable', 'false');
    });
    this.frontNodeLayer.append(visual);
    this.frontPortLayer.append(ports);
    this.syncFrontFocus();
  }

  syncFrontFocus(active = document.activeElement) {
    const frontNode = this.frontNodeLayer.querySelector('.process-node');
    const frontPorts = this.frontPortLayer.querySelectorAll('.process-port');
    frontNode?.classList.remove('is-focus-copy');
    frontPorts.forEach((port) => port.classList.remove('is-focus-copy'));
    const focused = active && this.root.contains(active) ? this.eventTarget({ target: active }) : null;
    if (!focused?.node || focused.node.dataset.nodeId !== this.frontNodeID) return;
    if (focused.port) {
      this.frontPortLayer.querySelector(`[data-port="${CSS.escape(focused.port.dataset.port)}"]`)
        ?.classList.add('is-focus-copy');
    } else {
      frontNode?.classList.add('is-focus-copy');
    }
  }

  raiseNode(nodeID) {
    if (!this.options.interactionLayering) return false;
    const node = interactionNode(this.layout.nodes, nodeID);
    if (!node) return false;
    const id = String(node.id);
    if (this.frontNodeID !== id) {
      this.frontNodeID = id;
      this.renderFrontNode();
      this.applySelection();
      this.applyKeyboardPort();
      this.applyConnectionFeedback();
    }
    return true;
  }

  resetInteractionLayering() {
    this.frontNodeID = null;
    this.frontNodeLayer.replaceChildren();
    this.frontPortLayer.replaceChildren();
  }

  bindEvents() {
    const signal = this.abort.signal;
    document.addEventListener('keydown', () => { this.keyboardFocusModality = true; }, { capture: true, signal });
    document.addEventListener('pointerdown', () => { this.keyboardFocusModality = false; }, { capture: true, signal });
    this.root.addEventListener('focus', () => {
      if (this.options.redirectKeyboardRootFocus && this.keyboardFocusModality) {
        this.focusKeyboardTarget();
      }
    }, { signal });
    this.fitButton.addEventListener('click', () => this.fitToView(), { signal });
    this.svg.addEventListener('wheel', (event) => this.onWheel(event), { passive: false, signal });
    this.svg.addEventListener('pointerenter', (event) => this.observeCanvasPointer(event), { signal });
    this.svg.addEventListener('pointerdown', (event) => this.onPointerDown(event), { signal });
    this.svg.addEventListener('pointermove', (event) => this.onPointerMove(event), { signal });
    this.svg.addEventListener('pointerup', (event) => this.onPointerUp(event), { signal });
    this.svg.addEventListener('pointercancel', (event) => this.onPointerCancel(event), { signal });
    this.svg.addEventListener('lostpointercapture', (event) => this.onLostPointerCapture(event), { signal });
    this.svg.addEventListener('pointerleave', () => {
      hook(this.options, 'onCanvasPointerLeave')({ reason: 'leave' });
      this.updatePortHover(null);
      this.cancelQueuedPointerFeedback();
      this.clearActionFeedback();
    }, { signal });
    this.svg.addEventListener('click', (event) => this.onClick(event), { signal });
    this.svg.addEventListener('dblclick', (event) => this.onDoubleClick(event), { signal });
    this.svg.addEventListener('keydown', (event) => this.onKeyDown(event), { signal });
    this.svg.addEventListener('focusin', (event) => {
      const target = this.eventTarget(event);
      if (target.node) this.raiseNode(target.node.dataset.nodeId);
      this.syncFrontFocus(event.target);
      if (this.feedbackEnabled) this.onFeedbackFocus(event);
    }, { signal });
    this.svg.addEventListener('focusout', (event) => this.syncFrontFocus(event.relatedTarget), { signal });
    if (this.feedbackEnabled) {
      this.svg.addEventListener('focusout', (event) => {
        this.clearActionFeedback();
        if (this.keyboardPort && event.relatedTarget && !this.root.contains(event.relatedTarget)) {
          this.keyboardFocusOwned = false;
        }
      }, { signal });
    }
    document.addEventListener('keydown', (event) => this.onSpaceKey(event), { signal });
    document.addEventListener('keyup', (event) => this.onSpaceKey(event), { signal });
    window.addEventListener('blur', () => {
      hook(this.options, 'onCanvasPointerLeave')({ reason: 'blur' });
      this.setSpaceHeld(false);
      this.keyboardFocusOwned = false;
      this.cancelActivePointer();
    }, { signal });
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
    if (node && !this.nodeLayer.contains(node) && !this.frontNodeLayer.contains(node)
        && !this.portLayer.contains(node) && !this.frontPortLayer.contains(node)) node = null;
    if (edge && !this.edgeLayer.contains(edge)) edge = null;
    if (port && !this.portLayer.contains(port) && !this.frontPortLayer.contains(port)) port = null;
    return { node, edge, port };
  }

  portElement(nodeId, port) {
    return this.portLayer.querySelector(
      `[data-node-id="${CSS.escape(String(nodeId))}"] [data-port="${CSS.escape(String(port))}"]`,
    );
  }

  connectionFeedback(request) {
    if (!this.feedbackEnabled) return null;
    const feedback = this.options.connectionFeedback(request, this.connectionEvaluation) || {};
    return {
      state: feedback.state || 'neutral', enabled: feedback.enabled !== false,
      message: String(feedback.message || ''), ...feedback,
    };
  }

  feedbackCandidate(target) {
    const hit = this.eventTarget({ target });
    if (hit.port && hit.node) {
      return { element: hit.port, candidate: { nodeId: hit.node.dataset.nodeId, port: hit.port.dataset.port } };
    }
    if (hit.node) return { element: hit.node, candidate: { nodeId: hit.node.dataset.nodeId } };
    if (target && this.svg.contains(target) && !hit.edge) {
      return { element: this.root, candidate: { emptyCanvas: true } };
    }
    return null;
  }

  feedbackForTarget(target) {
    if (!this.feedbackEnabled || !target) return null;
    if (this.connectionSource) {
      const resolved = this.feedbackCandidate(target);
      if (!resolved) return null;
      return {
        element: resolved.element,
        feedback: this.connectionFeedback({
          phase: 'target', source: this.connectionSource, candidate: resolved.candidate,
        }),
      };
    }
    const hit = this.eventTarget({ target });
    if (!hit.port || !hit.node) return null;
    return {
      element: hit.port,
      feedback: this.connectionFeedback({
        phase: 'source', source: { nodeId: hit.node.dataset.nodeId, port: hit.port.dataset.port },
      }),
    };
  }

  positionActionTooltip(target) {
    if (!this.actionTooltip || !target?.getBoundingClientRect) return;
    const rootRect = this.root.getBoundingClientRect();
    const targetRect = target === this.root ? this.svg.getBoundingClientRect() : target.getBoundingClientRect();
    const tooltipRect = this.actionTooltip.getBoundingClientRect();
    const width = tooltipRect.width || Math.min(300, Math.max(180, this.actionTooltip.textContent.length * 6));
    const height = tooltipRect.height || 42;
    const centered = targetRect.left - rootRect.left + targetRect.width / 2 - width / 2;
    const left = clamp(centered, 8, Math.max(8, rootRect.width - width - 8));
    const above = targetRect.top - rootRect.top - height - 10;
    const top = above >= 8
      ? above : clamp(targetRect.bottom - rootRect.top + 10, 8, Math.max(8, rootRect.height - height - 8));
    this.actionTooltip.style.left = `${left}px`;
    this.actionTooltip.style.top = `${top}px`;
  }

  showActionFeedback(target, feedback, { immediate = false, keyboard = false } = {}) {
    if (!this.actionTooltip || !target || !feedback?.message) {
      this.clearActionFeedback();
      return;
    }
    const unchanged = this.feedbackTarget === target
      && this.actionTooltip.textContent === feedback.message;
    const resume = this.feedbackResume?.message === feedback.message ? this.feedbackResume : null;
    this.feedbackResume = null;
    if (!unchanged) {
      this.clearActionFeedback();
      this.feedbackTarget = target;
      this.actionTooltip.textContent = feedback.message;
      this.actionTooltip.dataset.state = feedback.state;
      this.actionTooltip.setAttribute('aria-hidden', 'false');
      if (target !== this.root && !target.closest?.('.process-front-copy')) {
        target.setAttribute('aria-describedby', this.actionTooltip.id);
      }
    }
    this.positionActionTooltip(target);
    if (unchanged && this.actionTooltip.classList.contains('is-visible')) return;
    if (unchanged && this.feedbackTimer && !immediate) return;
    if (this.feedbackTimer) {
      clearTimeout(this.feedbackTimer);
      this.feedbackTimer = null;
    }
    const delay = immediate || resume?.visible ? 0 : resume
      ? resume.remaining : keyboard
        ? Number(this.options.keyboardFeedbackDelay ?? 220)
        : Number(this.options.actionFeedbackDelay ?? 750);
    this.feedbackStartedAt = Date.now() - (resume
      ? Number(this.options.actionFeedbackDelay ?? 750) - resume.remaining : 0);
    this.feedbackTimer = setTimeout(() => {
      this.feedbackTimer = null;
      if (this.feedbackTarget === target) this.actionTooltip.classList.add('is-visible');
    }, Math.max(0, delay));
  }

  clearActionFeedback() {
    if (this.feedbackTimer) clearTimeout(this.feedbackTimer);
    this.feedbackTimer = null;
    this.feedbackStartedAt = 0;
    this.feedbackResume = null;
    if (this.feedbackTarget && this.feedbackTarget !== this.root) {
      this.feedbackTarget.removeAttribute('aria-describedby');
      this.feedbackTarget.classList.remove('is-connection-hover');
    }
    this.feedbackTarget = null;
    if (!this.actionTooltip) return;
    this.actionTooltip.classList.remove('is-visible');
    this.actionTooltip.setAttribute('aria-hidden', 'true');
    this.actionTooltip.textContent = '';
    delete this.actionTooltip.dataset.state;
  }

  onFeedbackFocus(event) {
    const resolved = this.feedbackForTarget(event.target);
    if (resolved) this.showActionFeedback(resolved.element, resolved.feedback, { keyboard: true });
    else this.clearActionFeedback();
  }

  queuePointerFeedback(event) {
    if (!this.feedbackEnabled) return;
    this.pendingFeedbackPointer = {
      clientX: event.clientX, clientY: event.clientY, pointerType: event.pointerType,
      target: event.target,
    };
    if (this.feedbackFrame != null) return;
    const schedule = typeof requestAnimationFrame === 'function'
      ? requestAnimationFrame : (callback) => setTimeout(callback, 16);
    const frame = {};
    this.feedbackFrame = frame;
    schedule(() => {
      if (this.feedbackFrame !== frame) return;
      this.feedbackFrame = null;
      const pending = this.pendingFeedbackPointer;
      this.pendingFeedbackPointer = null;
      if (!pending || this.destroyed) return;
      // Pointer capture retargets moves to the SVG. One coalesced hit-test per
      // animation frame is the only way this controller reads beneath it.
      const target = this.connectionSource
        ? document.elementFromPoint(pending.clientX, pending.clientY) : pending.target;
      this.root.querySelectorAll('.is-connection-hover').forEach((element) => {
        element.classList.remove('is-connection-hover');
      });
      const resolved = this.feedbackForTarget(target);
      if (!resolved || (pending.pointerType === 'touch' && !this.connectionSource)) {
        this.clearActionFeedback();
        return;
      }
      if (resolved.element !== this.root) resolved.element.classList.add('is-connection-hover');
      this.showActionFeedback(resolved.element, resolved.feedback);
    });
  }

  cancelQueuedPointerFeedback() {
    this.pendingFeedbackPointer = null;
    // The scheduled callback may already be queued and requestAnimationFrame
    // is not consistently cancellable in the test/fallback environments. A
    // per-frame identity makes it inert without letting an old callback clear
    // a newer frame.
    this.feedbackFrame = null;
  }

  beginConnectionFeedback(source) {
    if (!this.feedbackEnabled) return;
    this.cancelQueuedPointerFeedback();
    this.clearActionFeedback();
    this.connectionSource = { nodeId: source.nodeId, port: source.port };
    this.connectionEvaluation = this.options.connectionFeedbackPreparation?.() || null;
    this.applyConnectionFeedback();
  }

  endConnectionFeedback() {
    if (!this.feedbackEnabled) return;
    this.connectionSource = null;
    this.connectionEvaluation = null;
    this.cancelQueuedPointerFeedback();
    this.clearActionFeedback();
    this.applyConnectionFeedback();
  }

  applyConnectionFeedback() {
    if (!this.feedbackEnabled) return;
    const active = !!this.connectionSource;
    this.root.classList.toggle('is-connecting', active);
    this.root.classList.remove('is-connection-canvas-valid', 'is-connection-canvas-invalid');
    this.root.querySelectorAll('.process-node').forEach((node) => {
      const canonical = this.nodeLayer.contains(node);
      node.classList.remove('is-connection-valid', 'is-connection-invalid', 'is-connection-source', 'is-connection-hover');
      if (canonical) {
        node.removeAttribute('aria-disabled');
        node.setAttribute('aria-label', node.dataset.baseAriaLabel || node.getAttribute('aria-label'));
      }
      if (!active) return;
      const feedback = this.connectionFeedback({
        phase: 'target', source: this.connectionSource, candidate: { nodeId: node.dataset.nodeId },
      });
      node.classList.add(`is-connection-${feedback.state === 'valid' ? 'valid' : 'invalid'}`);
      if (canonical) {
        node.setAttribute('aria-disabled', String(feedback.state !== 'valid'));
        if (feedback.message) node.setAttribute('aria-label', `${node.dataset.baseAriaLabel}. ${feedback.message}`);
      }
    });
    this.root.querySelectorAll('.process-port').forEach((port) => {
      const canonical = this.portLayer.contains(port);
      const node = port.closest('[data-node-id]');
      const sourceState = port.dataset.sourceState;
      port.classList.remove('is-connection-valid', 'is-connection-invalid', 'is-connection-source', 'is-connection-hover');
      port.classList.toggle('is-action-disabled', !active && sourceState === 'disabled');
      if (canonical) {
        port.setAttribute('aria-disabled', String(sourceState === 'disabled'));
        port.setAttribute('aria-label', active
          ? port.dataset.baseAriaLabel || port.getAttribute('aria-label')
          : port.dataset.sourceAriaLabel || port.dataset.baseAriaLabel || port.getAttribute('aria-label'));
      }
      if (!active) return;
      const candidate = { nodeId: node.dataset.nodeId, port: port.dataset.port };
      const isSource = candidate.nodeId === this.connectionSource.nodeId
        && candidate.port === this.connectionSource.port;
      const feedback = this.connectionFeedback({ phase: 'target', source: this.connectionSource, candidate });
      const state = isSource ? 'source' : feedback.state === 'valid' ? 'valid' : 'invalid';
      port.classList.add(`is-connection-${state}`);
      if (canonical) {
        port.setAttribute('aria-disabled', String(state === 'invalid'));
        if (feedback.message) port.setAttribute('aria-label', `${port.dataset.baseAriaLabel}. ${feedback.message}`);
      }
    });
    if (!active) return;
    const canvas = this.connectionFeedback({
      phase: 'target', source: this.connectionSource, candidate: { emptyCanvas: true },
    });
    this.root.classList.add(`is-connection-canvas-${canvas.state === 'valid' ? 'valid' : 'invalid'}`);
  }

  updatePortHover(event) {
    const target = event ? this.eventTarget(event) : { node: null };
    const nodeID = target.node?.dataset.nodeId || null;
    this.root.querySelectorAll('.process-node-ports').forEach((ports) => {
      ports.classList.toggle('is-node-hover', ports.dataset.nodeId === nodeID);
    });
  }

  onPointerDown(event) {
    this.observeCanvasPointer?.(event);
    if (this.pointer) {
      // A pointerdown repeating the armed gesture's own pointer id AND button
      // means that button was released without its pointerup/pointercancel
      // ever reaching this SVG (one button cannot press twice in a row):
      // cancel the dead gesture instead of letting it swallow this one and
      // replay a stale drag. Everything else leaves the owned gesture alone —
      // a mouse keeps one pointer id across ALL its buttons, so a secondary
      // button pressed mid-drag arrives as a same-id pointerdown too, and a
      // multi-touch second finger arrives under a different id.
      if (this.pointer.id !== event.pointerId || this.pointer.button !== event.button) return;
      this.onPointerCancel({
        pointerId: this.pointer.id,
        clientX: this.pointer.lastClientX,
        clientY: this.pointer.lastClientY,
        type: 'stale-gesture',
      });
    }
    const middle = event.button === 1;
    if (event.button !== 0 && !middle) return;
    // Resolve the target before focus: focusing the graph blurs an inspector
    // input, whose synchronous change handler may refresh and replace every
    // SVG layer child. The detached original still carries the stable ids we
    // need to classify this gesture.
    const target = this.eventTarget(event);
    const connectionIntent = event.button === 0 && !this.spaceHeld;
    if (connectionIntent && target.node) this.raiseNode?.(target.node.dataset.nodeId);
    if (connectionIntent && target.port && target.node && this.feedbackEnabled) {
      const source = { nodeId: target.node.dataset.nodeId, port: target.port.dataset.port };
      const feedback = this.connectionFeedback({ phase: 'source', source });
      if (feedback.enabled === false) {
        this.cancelQueuedPointerFeedback();
        event.preventDefault();
        event.stopPropagation();
        const identity = { nodeId: source.nodeId, port: source.port };
        target.port.focus({ preventScroll: true });
        // Focusing a connector can blur-commit an inspector edit, whose
        // synchronous refresh replaces both SVG layers. Rebind by semantic
        // identity before focus/ARIA/tooltip ownership so none of the
        // disclosure lands on the detached event target.
        let livePort = this.portElement(identity.nodeId, identity.port);
        if (!livePort) {
          this.clearActionFeedback();
          return;
        }
        if (livePort !== target.port) {
          livePort.focus({ preventScroll: true });
          livePort = this.portElement(identity.nodeId, identity.port);
          if (!livePort) {
            this.clearActionFeedback();
            return;
          }
        }
        const liveFeedback = this.connectionFeedback({ phase: 'source', source: identity });
        if (liveFeedback.enabled !== false) {
          this.clearActionFeedback();
          return;
        }
        this.showActionFeedback(livePort, liveFeedback, { immediate: true, keyboard: event.pointerType === '' });
        return;
      }
    }
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
    if (directPan) {
      this.cancelQueuedPointerFeedback?.();
      this.clearActionFeedback?.();
    }
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
    // The gesture owns its own start-position snapshot: a node-drag commit
    // may only ever see coordinates captured inside this pointer's lifetime.
    const starts = mode !== 'node' ? [] : nodeIDs
      .map((id) => this.layout.nodes.find((node) => node.id === id))
      .filter(Boolean)
      .map(({ id, x, y }) => ({ id, x, y }));
    this.pointer = {
      id: event.pointerId, button: event.button, mode,
      startClientX: event.clientX, startClientY: event.clientY,
      lastClientX: event.clientX, lastClientY: event.clientY,
      pointerType: event.pointerType || 'mouse',
      startPoint: point, startView: { ...this.view }, nodeID, nodeIDs, starts,
      edgeID: target.edge?.dataset.edgeId, port: target.port?.dataset.port,
      selectionStarted: false,
    };
    hook(this.options, 'onInteractionStart')({ mode, pointerId: event.pointerId, event });
    this.dragMoved = false;
    this.svg.setPointerCapture?.(event.pointerId);
    if (mode === 'port') {
      event.stopPropagation();
      this.clearKeyboardPort();
      this.beginConnectionFeedback?.({ nodeId: this.pointer.nodeID, port: this.pointer.port });
      hook(this.options, 'onPortDragStart')({ nodeId: this.pointer.nodeID, port: this.pointer.port, point, event });
    } else if (mode === 'marquee') {
      this.marquee = svgElement('rect', { class: 'process-marquee', x: point.x, y: point.y, width: 0, height: 0 });
      this.viewport.append(this.marquee);
    }
  }

  onPointerMove(event) {
    this.observeCanvasPointer?.(event);
    if (!this.pointer) {
      this.updatePortHover(event);
      this.queuePointerFeedback?.(event);
      return;
    }
    if (this.pointer.id !== event.pointerId) return;
    if (this.pointer.mode !== 'port') this.updatePortHover(event);
    if (this.pointer.mode !== 'pan') this.queuePointerFeedback?.(event);
    this.pointer.lastClientX = event.clientX;
    this.pointer.lastClientY = event.clientY;
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
      const delta = {
        x: point.x - this.pointer.startPoint.x,
        y: point.y - this.pointer.startPoint.y,
      };
      // The terminal pointer sample is not necessarily the frame the user saw
      // (notably when releasing a fast-moving mouse). Keep the exact delta
      // rendered by this gesture so pointerup can commit that visible frame.
      this.pointer.nodeDelta = delta;
      for (const nodeID of this.pointer.nodeIDs) {
        const node = this.nodeLayer.querySelector(`[data-node-id="${CSS.escape(nodeID)}"]`);
        const ports = this.portLayer.querySelector(`[data-node-id="${CSS.escape(nodeID)}"]`);
        const frontNode = this.frontNodeID === nodeID ? this.frontNodeLayer.querySelector('[data-node-id]') : null;
        const frontPorts = this.frontNodeID === nodeID ? this.frontPortLayer.querySelector('[data-node-id]') : null;
        const laid = this.layout.nodes.find((candidate) => candidate.id === nodeID);
        if (node && laid) {
          const transform = `translate(${laid.x + delta.x} ${laid.y + delta.y})`;
          node.setAttribute('transform', transform);
          ports?.setAttribute('transform', transform);
          frontNode?.setAttribute('transform', transform);
          frontPorts?.setAttribute('transform', transform);
        }
      }
      hook(this.options, 'onNodeDrag')({
        nodeId: this.pointer.nodeID, nodeIds: [...this.pointer.nodeIDs], point,
        delta: { ...delta },
        event,
      });
      this.renderTransientEdges(this.pointer.nodeIDs, delta);
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
      this.endConnectionFeedback?.();
      hook(this.options, 'onPortDragEnd')({
        nodeId: pointer.nodeID, port: pointer.port, point,
        targetNodeId: target.node?.dataset.nodeId || null,
        targetPort: target.port?.dataset.port || null,
        emptyCanvas: !!hit && this.svg.contains(hit)
          && !target.node && !target.port && !target.edge,
        event,
      });
    } else if (pointer.mode === 'node') {
      // Position ownership stays outside the core. Snap the transient drag back
      // unless the hook's caller supplied a new pinned graph through setGraph.
      this.snapNodesHome(pointer.nodeIDs || [pointer.nodeID]);
      this.restoreTransientEdges();
      hook(this.options, 'onNodeDragEnd')({
        nodeId: pointer.nodeID, nodeIds: [...(pointer.nodeIDs || [pointer.nodeID])],
        starts: (pointer.starts || []).map((start) => ({ ...start })),
        // Commit the last frame actually rendered during the gesture. A
        // release delivered while the device is still moving can carry a
        // terminal coordinate behind that frame; using it makes the node snap
        // home even though the user visibly dragged beyond the start.
        delta: { ...(pointer.nodeDelta || {
          x: point.x - pointer.startPoint.x,
          y: point.y - pointer.startPoint.y,
        }) },
        moved: this.dragMoved, event,
      });
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
    hook(this.options, 'onInteractionEnd')({ mode: pointer.mode, pointerId: event.pointerId, cancelled: false, event });
    // Focusing the graph on pointerdown can synchronously commit an inspector
    // field and replace the SVG child under the pointer. Chrome then omits the
    // synthetic click even though pointer capture still delivers pointerup to
    // this stable SVG. Complete captured node/edge clicks here, and suppress
    // the redundant synthetic click when the original child survived.
    if (this.pendingClickTarget?.nodeID || this.pendingClickTarget?.edgeID) {
      this.onClick(event);
      this.suppressClick = true;
    }
    // The synthetic click follows pointerup in the same task. Clear on the next
    // task so a completed drag never also selects/activates the dragged node.
    // If another gesture armed before this drains, that gesture owns these
    // flags now — stomping dragMoved mid-drag would void its commit.
    setTimeout(() => {
      if (this.pointer) return;
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
      this.endConnectionFeedback?.();
      hook(this.options, 'onPortDragEnd')({
        nodeId: pointer.nodeID, port: pointer.port,
        point: this.clientToGraph(event.clientX, event.clientY),
        targetNodeId: null, targetPort: null, cancelled: true, event,
        ...(event.cancellation ? { cancellation: event.cancellation } : {}),
      });
    } else if (pointer.mode === 'node') {
      this.snapNodesHome(pointer.nodeIDs || [pointer.nodeID]);
      this.restoreTransientEdges();
      hook(this.options, 'onNodeDragCancel')({
        nodeId: pointer.nodeID, nodeIds: [...(pointer.nodeIDs || [pointer.nodeID])], event,
      });
    } else if (pointer.mode === 'marquee') {
      this.marquee?.remove();
      this.marquee = null;
    }
    hook(this.options, 'onInteractionEnd')({ mode: pointer.mode, pointerId: event.pointerId, cancelled: true, event });
    this.suppressClick = this.dragMoved;
    this.pendingClickTarget = null;
    // The stale-gesture heal cancels from INSIDE the pointerdown arming the
    // replacement gesture, so this timer can fire mid-drag: never let it
    // stomp a live gesture's flags (a false dragMoved voids the commit).
    setTimeout(() => {
      if (this.pointer) return;
      this.dragMoved = false;
      this.suppressClick = false;
    }, 0);
  }

  onLostPointerCapture(event) {
    if (!this.pointer || this.pointer.id !== event.pointerId) return;
    this.onPointerCancel({
      pointerId: event.pointerId,
      clientX: this.pointer.lastClientX,
      clientY: this.pointer.lastClientY,
      type: 'lostpointercapture',
    });
  }

  cancelActivePointer() {
    if (!this.pointer) return false;
    this.onPointerCancel({
      pointerId: this.pointer.id,
      clientX: this.pointer.lastClientX,
      clientY: this.pointer.lastClientY,
      type: 'blur',
    });
    return true;
  }

  cancelMissingPointerPort() {
    const pointer = this.pointer;
    if (!pointer || pointer.mode !== 'port'
      || this.portElement(pointer.nodeID, pointer.port)) return false;
    this.onPointerCancel({
      pointerId: pointer.id,
      clientX: pointer.lastClientX,
      clientY: pointer.lastClientY,
      type: 'source-removed',
      cancellation: 'source-removed',
    });
    return true;
  }

  snapNodeHome(nodeID) {
    const node = this.nodeLayer.querySelector(`[data-node-id="${CSS.escape(nodeID)}"]`);
    const ports = this.portLayer.querySelector(`[data-node-id="${CSS.escape(nodeID)}"]`);
    const frontNode = this.frontNodeID === nodeID ? this.frontNodeLayer.querySelector('[data-node-id]') : null;
    const frontPorts = this.frontNodeID === nodeID ? this.frontPortLayer.querySelector('[data-node-id]') : null;
    const laid = this.layout.nodes.find((candidate) => candidate.id === nodeID);
    if (node && laid) {
      const transform = `translate(${laid.x} ${laid.y})`;
      node.setAttribute('transform', transform);
      ports?.setAttribute('transform', transform);
      frontNode?.setAttribute('transform', transform);
      frontPorts?.setAttribute('transform', transform);
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
    if (this.feedbackEnabled && this.keyboardPort && target.node && !target.port) {
      event.preventDefault();
      const source = this.keyboardPort;
      const feedback = this.connectionFeedback({
        phase: 'target', source, candidate: { nodeId: target.node.dataset.nodeId },
      });
      if (feedback?.state !== 'valid') {
        this.showActionFeedback(target.node, feedback, { immediate: true, keyboard: true });
        return;
      }
      const node = this.layout.nodes.find((candidate) => candidate.id === target.node.dataset.nodeId);
      const point = { x: node.x, y: node.y };
      this.clearKeyboardPort();
      hook(this.options, 'onPortDragEnd')({
        nodeId: source.nodeId, port: source.port, point,
        targetNodeId: target.node.dataset.nodeId, targetPort: null, keyboard: true, event,
      });
      return;
    }
    if (target.port && target.node) {
      let sourceFeedback = null;
      if (!this.keyboardPort) {
        sourceFeedback = this.connectionFeedback({
          phase: 'source', source: { nodeId: target.node.dataset.nodeId, port: target.port.dataset.port },
        });
        // A disabled connector still explains itself when Space is tapped,
        // but it must not consume the graph's Space+pointer pan modifier.
        // Claim pan ownership before preventDefault stops the document-level
        // listener; Enter and unclaimed Space activation stay unchanged.
        if (sourceFeedback?.enabled === false && (event.key === ' ' || event.code === 'Space')) {
          this.onSpaceKey(event, { allowActionTarget: true });
        }
      }
      event.preventDefault();
      const nodeId = target.node.dataset.nodeId;
      const port = target.port.dataset.port;
      const node = this.layout.nodes.find((candidate) => candidate.id === nodeId);
      const point = { x: node.x, y: node.y + (port === 'out' ? node.height / 2 : -node.height / 2) };
      if (!this.keyboardPort) {
        const feedback = sourceFeedback;
        if (feedback?.enabled === false) {
          this.showActionFeedback(target.port, feedback, { immediate: true, keyboard: true });
          return;
        }
        this.keyboardPort = { nodeId, port, point };
        this.keyboardFocusOwned = true;
        this.beginConnectionFeedback(this.keyboardPort);
        this.applyKeyboardPort();
        hook(this.options, 'onPortDragStart')({ nodeId, port, point, keyboard: true, event });
      } else {
        const source = this.keyboardPort;
        if (this.feedbackEnabled) {
          const feedback = this.connectionFeedback({
            phase: 'target', source, candidate: { nodeId, port },
          });
          if (feedback?.state !== 'valid' && feedback?.state !== 'source') {
            this.showActionFeedback(target.port, feedback, { immediate: true, keyboard: true });
            return;
          }
        }
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

  onSpaceKey(event, { allowActionTarget = false } = {}) {
    if (event.key !== ' ' && event.code !== 'Space') return;
    if (event.type === 'keyup') {
      this.setSpaceHeld(false);
      return;
    }
    if (event.defaultPrevented || event.repeat || this.pointer
      || (!allowActionTarget && isGraphTypingTarget(event.target))) return;
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

  containsClientPoint(clientX, clientY) {
    if (!Number.isFinite(clientX) || !Number.isFinite(clientY)) return false;
    const rect = this.svg.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0
      && clientX >= rect.left && clientX <= rect.left + rect.width
      && clientY >= rect.top && clientY <= rect.top + rect.height;
  }

  // Passive observation only: this hook never claims input, captures a
  // pointer, moves focus, or participates in pan/drag ownership.
  observeCanvasPointer(event) {
    const clientX = Number(event?.clientX);
    const clientY = Number(event?.clientY);
    if (!this.containsClientPoint(clientX, clientY)) {
      hook(this.options, 'onCanvasPointerLeave')({ reason: 'bounds', event });
      return;
    }
    hook(this.options, 'onCanvasPointerMove')({
      clientX, clientY, pointerType: event.pointerType || '', event,
    });
  }

  applyView() {
    this.viewport.setAttribute('transform', `translate(${this.view.x} ${this.view.y}) scale(${this.view.k})`);
  }

  setZoom(zoom, { clientX, clientY } = {}) {
    const rect = this.svg.getBoundingClientRect();
    if (rect.width <= 0 || rect.height <= 0) return false;
    const anchorX = Number.isFinite(clientX) ? clientX - rect.left : rect.width / 2;
    const anchorY = Number.isFinite(clientY) ? clientY - rect.top : rect.height / 2;
    const oldZoom = this.view.k;
    const nextZoom = clamp(zoom, MIN_ZOOM, MAX_ZOOM);
    const graphX = (anchorX - this.view.x) / oldZoom;
    const graphY = (anchorY - this.view.y) / oldZoom;
    this.view.k = nextZoom;
    this.view.x = anchorX - graphX * nextZoom;
    this.view.y = anchorY - graphY * nextZoom;
    this.applyView();
    return true;
  }

  zoomBy(factor) {
    if (!Number.isFinite(factor) || factor <= 0) return false;
    return this.setZoom(this.view.k * factor);
  }

  resetZoom() {
    return this.setZoom(1);
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

  focusNode(id) {
    this.raiseNode(id);
    const node = this.nodeLayer.querySelector(`[data-node-id="${CSS.escape(String(id))}"]`);
    if (!node) return false;
    node.focus({ preventScroll: true });
    return true;
  }

  // An editor canvas root is a programmatic shortcut sink, not a visible
  // selection. When keyboard-origin focus restoration lands there, move focus
  // synchronously to a real graph item (or its Fit control) so focus remains
  // visible without painting a ring around the entire canvas.
  focusKeyboardTarget() {
    const source = this.keyboardPort || this.connectionSource;
    const sourcePort = source && this.portElement(source.nodeId, source.port);
    if (sourcePort) {
      sourcePort.focus({ preventScroll: true });
      return true;
    }
    for (const item of selectionItems(this.selected)) {
      const target = item.type === 'node'
        ? this.nodeLayer.querySelector(`[data-node-id="${CSS.escape(String(item.id))}"]`)
        : this.edgeLayer.querySelector(`[data-edge-id="${CSS.escape(String(item.id))}"]`);
      if (target) {
        target.focus({ preventScroll: true });
        return true;
      }
    }
    const target = this.nodeLayer.querySelector('.process-node')
      || this.edgeLayer.querySelector('.process-edge') || this.fitButton;
    target?.focus({ preventScroll: true });
    return !!target;
  }

  select(selection) {
    this.selected = makeSelection(selectionItems(selection));
    const items = selectionItems(this.selected);
    if (items.length === 1 && items[0].type === 'node') this.raiseNode(items[0].id);
    this.applySelection();
  }

  applySelection() {
    this.nodeLayer.querySelectorAll('.process-node').forEach((element) => {
      element.classList.remove('is-selected');
      element.setAttribute('aria-pressed', 'false');
    });
    this.edgeLayer.querySelectorAll('.process-edge').forEach((element) => {
      element.classList.remove('is-selected');
      element.setAttribute('aria-pressed', 'false');
    });
    this.frontNodeLayer.querySelector('.process-node')?.classList.remove('is-selected');
    for (const item of selectionItems(this.selected)) {
      let selected = null;
      if (item.type === 'node') {
        selected = this.nodeLayer.querySelector(`[data-node-id="${CSS.escape(String(item.id))}"]`);
        if (this.frontNodeID === String(item.id)) {
          this.frontNodeLayer.querySelector('[data-node-id]')?.classList.add('is-selected');
        }
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
    this.frontPortLayer.querySelectorAll('.process-port').forEach((port) => {
      port.classList.remove('is-keyboard-source');
    });
    if (!this.keyboardPort) return;
    const source = this.portLayer.querySelector(
      `[data-node-id="${CSS.escape(this.keyboardPort.nodeId)}"] [data-port="${this.keyboardPort.port}"]`,
    );
    if (!source) {
      this.cancelMissingKeyboardPort();
      return;
    }
    source.classList.add('is-keyboard-source');
    source.setAttribute('aria-pressed', 'true');
    if (this.keyboardPort.nodeId === this.frontNodeID) {
      this.frontPortLayer.querySelector(`[data-port="${CSS.escape(this.keyboardPort.port)}"]`)
        ?.classList.add('is-keyboard-source');
    }
  }

  clearKeyboardPort() {
    this.keyboardPort = null;
    this.keyboardFocusOwned = false;
    this.endConnectionFeedback();
    this.applyKeyboardPort();
  }

  cancelMissingKeyboardPort() {
    if (!this.keyboardPort) return false;
    const source = this.keyboardPort;
    this.keyboardPort = null;
    this.keyboardCancellationFocus = {
      nodeId: source.nodeId, port: source.port, restore: this.keyboardFocusOwned,
    };
    this.keyboardFocusOwned = false;
    this.endConnectionFeedback();
    // A graph replacement can remove a keyboard gesture's source (for
    // example, undoing a just-created node). Finish through the same semantic
    // event the adapter uses for Escape so its rubber band and interaction
    // generation cannot remain live. The editor's cancelled branch performs
    // no commit or status mutation.
    hook(this.options, 'onPortDragEnd')({
      nodeId: source.nodeId, port: source.port, point: source.point,
      targetNodeId: null, targetPort: null, keyboard: true, cancelled: true,
      cancellation: 'source-removed',
    });
    return true;
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
    const cancelled = this.keyboardCancellationFocus;
    this.keyboardCancellationFocus = null;
    if (!focused) {
      if (cancelled?.restore) this.root.focus({ preventScroll: true });
      return;
    }
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
    if (target) target.focus({ preventScroll: true });
    else if (cancelled?.restore && focused.type === 'port'
      && focused.nodeId === cancelled.nodeId && focused.port === cancelled.port) {
      this.root.focus({ preventScroll: true });
    }
  }

  destroy() {
    if (this.destroyed) return;
    this.destroyed = true;
    this.endConnectionFeedback();
    this.abort.abort();
    this.container.replaceChildren();
  }
}

export function createProcessGraph(container, graph, options) {
  return new ProcessGraph(container, graph, options);
}
// dashboard-imperative-boundary: process-graph
