// The sole boundary between Preact-owned process editor state and the opaque
// imperative SVG graph widget. Inputs flow through commands; graph gestures
// flow out as semantic events. Consumers never inspect ProcessGraph DOM.

import { ProcessGraph } from './process-graph.js';

const SVG_NS = 'http://www.w3.org/2000/svg';

function noop() {}

function cloneLayout(layout) {
  return structuredClone(layout || { nodes: [], edges: [] });
}

export class ProcessGraphAdapter {
  constructor(host, { graph = { nodes: [], edges: [] }, ariaLabel = '', events = {} } = {}) {
    this.host = host;
    this.events = events;
    this.disposed = false;
    this.interactionGeneration = 0;
    this.connectionBand = null;
    const emit = (name) => (payload) => {
      if (!this.disposed) this.events[name]?.(payload);
    };
    this.widget = new ProcessGraph(host, graph, {
      ariaLabel,
      colorScheme: 'dark',
      onInteractionStart: () => { this.interactionGeneration += 1; },
      onInteractionEnd: () => { this.interactionGeneration += 1; },
      onNodeClick: emit('nodeClick'),
      onNodeDblClick: emit('nodeDoubleClick'),
      onEdgeClick: emit('edgeClick'),
      onCanvasClick: emit('canvasClick'),
      onMarqueeSelect: emit('marqueeSelection'),
      onNodeDragStart: emit('nodeDragStart'),
      onNodeDrag: (payload) => {
        if (!this.pendingNodeDrag) {
          const ids = payload.nodeIds || [payload.nodeId];
          this.pendingNodeDrag = {
            starts: ids.map((id) => this.widget.layout.nodes.find((node) => node.id === id))
              .filter(Boolean).map(({ id, x, y }) => ({ id, x, y })),
          };
        }
        this.pendingNodeDrag.latest = payload;
      },
      onNodeDragEnd: (payload) => {
        const pending = this.pendingNodeDrag;
        this.pendingNodeDrag = null;
        emit('nodeDragEnd')({
          ...payload,
          starts: pending?.starts || [],
          delta: pending?.latest?.delta || payload.delta,
        });
      },
      onNodeDragCancel: (payload) => {
        this.pendingNodeDrag = null;
        emit('nodeDragCancel')(payload);
      },
      onPortDragStart: (payload) => {
        if (payload.keyboard) this.interactionGeneration += 1;
        const laid = this.widget.layout.nodes.find((node) => node.id === payload.nodeId);
        if (laid) this.beginConnectionBand({
          x: laid.x,
          y: laid.y + (payload.port === 'in' ? -laid.height / 2 : laid.height / 2),
        });
        emit('portDragStart')(payload);
      },
      onPortDragMove: (payload) => this.updateConnectionBand(payload.point),
      onPortDragEnd: (payload) => {
        if (payload.keyboard) this.interactionGeneration += 1;
        this.endConnectionBand();
        emit('portDragEnd')(payload);
      },
      onCanvasDrop: emit('canvasDrop'),
      marqueeSelect: true,
      wheelPan: true,
    });
  }

  setGraph(graph, options) {
    if (this.disposed) return { nodes: [], edges: [] };
    return cloneLayout(this.widget.setGraph(graph, options));
  }

  setSelection(selection) {
    if (!this.disposed) this.widget.select(selection);
  }

  layoutSnapshot() {
    return this.disposed ? { nodes: [], edges: [] } : cloneLayout(this.widget.layout);
  }

  viewSnapshot() {
    return this.disposed ? { x: 0, y: 0, k: 1 } : { ...this.widget.view };
  }

  hasActiveInteraction() {
    return !this.disposed && !!(this.widget.pointer || this.widget.keyboardPort || this.connectionBand);
  }

  interactionSnapshot() {
    return {
      generation: this.interactionGeneration,
      active: this.hasActiveInteraction(),
    };
  }

  fit() { if (!this.disposed) this.widget.fitToView(); }
  centerOn(x, y) { if (!this.disposed) this.widget.centerOn(x, y); }
  zoomBy(factor) { return this.disposed ? false : this.widget.zoomBy(factor); }
  resetZoom() { return this.disposed ? false : this.widget.resetZoom(); }
  focus() { if (!this.disposed) this.widget.root.focus({ preventScroll: true }); }
  focusNode(id) { if (!this.disposed) this.widget.focusNode(id); }

  clientToGraph(clientX, clientY) {
    return this.disposed ? { x: 0, y: 0 } : this.widget.clientToGraph(clientX, clientY);
  }

  canvasCenter() {
    if (this.disposed) return { x: 0, y: 0 };
    const rect = this.widget.svg.getBoundingClientRect();
    return this.widget.clientToGraph(rect.left + rect.width / 2, rect.top + rect.height / 2);
  }

  graphPointToHost(point, host = this.host) {
    if (this.disposed) return { left: 0, top: 0 };
    const svgRect = this.widget.svg.getBoundingClientRect();
    const hostRect = host.getBoundingClientRect();
    const view = this.widget.view;
    return {
      left: svgRect.left - hostRect.left + view.x + point.x * view.k,
      top: svgRect.top - hostRect.top + view.y + point.y * view.k,
    };
  }

  clientPointToHost({ clientX, clientY }, host = this.host) {
    if (this.disposed || !host) return { left: 0, top: 0 };
    const rect = host.getBoundingClientRect();
    return { left: clientX - rect.left, top: clientY - rect.top };
  }

  beginConnectionBand(start) {
    if (this.disposed) return;
    this.endConnectionBand();
    const element = document.createElementNS(SVG_NS, 'path');
    element.setAttribute('class', 'process-editor-band');
    element.setAttribute('fill', 'none');
    element.setAttribute('d', `M ${start.x} ${start.y} L ${start.x} ${start.y}`);
    this.widget.viewport.append(element);
    this.connectionBand = { element, start: { ...start } };
  }

  updateConnectionBand(point) {
    const band = this.connectionBand;
    if (!band || this.disposed) return;
    band.element.setAttribute('d', `M ${band.start.x} ${band.start.y} L ${point.x} ${point.y}`);
  }

  endConnectionBand() {
    this.connectionBand?.element?.remove();
    this.connectionBand = null;
  }

  dispose() {
    if (this.disposed) return;
    this.disposed = true;
    this.endConnectionBand();
    this.events = {};
    this.widget?.destroy();
    this.widget = null;
    this.host = null;
  }
}

export function createProcessGraphAdapter(host, options) {
  if (!(host instanceof Element)) throw new TypeError('process graph adapter host must be a DOM Element');
  return new ProcessGraphAdapter(host, options);
}

export const processGraphAdapterNoop = noop;
