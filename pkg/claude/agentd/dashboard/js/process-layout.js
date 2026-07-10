// process-layout.js -- deterministic, dependency-free layered layout for the
// shared process editor/viewer graph. This module deliberately has no DOM
// dependency: Node's built-in test runner exercises the exact file shipped to
// the browser.

export const PROCESS_LAYOUT_DEFAULTS = Object.freeze({
  marginX: 72,
  marginY: 64,
  nodeSep: 52,
  rankSep: 112,
  edgeSep: 18,
  sweeps: 6,
});

const TYPE_SIZES = Object.freeze({
  task: [168, 68],
  decision: [108, 108],
  wait: [78, 78],
  start: [58, 58],
  end: [62, 62],
});

function cmpText(a, b) {
  return String(a).localeCompare(String(b), 'en');
}

function finite(value, fallback) {
  return Number.isFinite(value) ? value : fallback;
}

function nodeSize(node) {
  if (node.compound?.collapsed) return { width: 190, height: 88 };
  const [width, height] = TYPE_SIZES[node.type] || TYPE_SIZES.task;
  return { width, height };
}

function stableNodes(graph) {
  const seen = new Set();
  return (graph.nodes || []).map((raw, inputIndex) => {
    const id = String(raw.id || '');
    if (!id) throw new Error(`process layout: node at index ${inputIndex} has no id`);
    if (seen.has(id)) throw new Error(`process layout: duplicate node id ${id}`);
    seen.add(id);
    return { ...raw, ...nodeSize(raw), id, inputIndex };
  }).sort((a, b) => cmpText(a.id, b.id));
}

function stableEdges(graph, byID) {
  return (graph.edges || []).map((raw, inputIndex) => {
    const from = String(raw.from || '');
    const to = String(raw.to || '');
    if (!byID.has(from) || !byID.has(to)) {
      throw new Error(`process layout: edge ${from || '?'} -> ${to || '?'} references an unknown node`);
    }
    return {
      ...raw,
      from,
      to,
      inputIndex,
      key: `${from}\u0000${to}\u0000${raw.outcome || ''}\u0000${inputIndex}`,
      back: raw.back === true,
    };
  }).sort((a, b) => cmpText(a.key, b.key));
}

// defaultFeedbackArc is the deliberately narrow cycle-breaking seam. V1 input
// already marks its one sanctioned retry edge with `back: true`, so normally it
// returns an empty set. If a future loop construct reaches this layer without a
// marker, this deterministic DFS identifies feedback arcs; replacing this one
// function (or passing options.feedbackArc) is sufficient to adopt a stronger
// heuristic without changing crossing reduction, coordinates, or rendering.
export function defaultFeedbackArc(nodes, edges) {
  const outgoing = new Map(nodes.map((n) => [n.id, []]));
  for (const edge of edges) {
    if (!edge.back) outgoing.get(edge.from).push(edge);
  }
  for (const list of outgoing.values()) list.sort((a, b) => cmpText(a.key, b.key));

  const state = new Map();
  const feedback = new Set();
  const visit = (id) => {
    state.set(id, 1);
    for (const edge of outgoing.get(id)) {
      const next = state.get(edge.to) || 0;
      if (next === 1) feedback.add(edge.inputIndex);
      else if (next === 0) visit(edge.to);
    }
    state.set(id, 2);
  };
  for (const node of nodes) if (!state.has(node.id)) visit(node.id);
  return feedback;
}

function assignLayers(nodes, edges, feedbackArc) {
  const extraBack = feedbackArc(nodes, edges);
  const ignored = extraBack instanceof Set ? extraBack : new Set(extraBack || []);
  const forward = edges.filter((edge) => !edge.back && !ignored.has(edge.inputIndex));
  const incoming = new Map(nodes.map((n) => [n.id, []]));
  const outgoing = new Map(nodes.map((n) => [n.id, []]));
  const indegree = new Map(nodes.map((n) => [n.id, 0]));
  for (const edge of forward) {
    incoming.get(edge.to).push(edge);
    outgoing.get(edge.from).push(edge);
    indegree.set(edge.to, indegree.get(edge.to) + 1);
  }
  for (const list of incoming.values()) list.sort((a, b) => cmpText(a.key, b.key));
  for (const list of outgoing.values()) list.sort((a, b) => cmpText(a.key, b.key));

  const ready = nodes.filter((n) => indegree.get(n.id) === 0).map((n) => n.id).sort(cmpText);
  const topo = [];
  while (ready.length) {
    const id = ready.shift();
    topo.push(id);
    for (const edge of outgoing.get(id)) {
      const degree = indegree.get(edge.to) - 1;
      indegree.set(edge.to, degree);
      if (degree === 0) {
        ready.push(edge.to);
        ready.sort(cmpText);
      }
    }
  }
  if (topo.length !== nodes.length) {
    throw new Error('process layout: feedback-arc seam left a cycle in the forward graph');
  }

  const layer = new Map(nodes.map((n) => [n.id, 0]));
  for (const id of topo) {
    for (const edge of outgoing.get(id)) {
      layer.set(edge.to, Math.max(layer.get(edge.to), layer.get(id) + 1));
    }
  }
  const classified = edges.map((edge) => ({
    ...edge,
    back: edge.back || ignored.has(edge.inputIndex),
  }));
  return { layer, incoming, outgoing, edges: classified };
}

function reduceCrossings(nodes, layerOf, edges, sweeps) {
  const maxLayer = Math.max(0, ...layerOf.values());
  const layers = Array.from({ length: maxLayer + 1 }, () => []);
  const nodeByID = new Map(nodes.map((n) => [n.id, n]));
  for (const node of nodes) layers[layerOf.get(node.id)].push(node.id);
  for (const layer of layers) {
    layer.sort((a, b) => {
      const pa = nodeByID.get(a).pinned;
      const pb = nodeByID.get(b).pinned;
      if (pa && pb && finite(pa.x, 0) !== finite(pb.x, 0)) return finite(pa.x, 0) - finite(pb.x, 0);
      return cmpText(a, b);
    });
  }

  const predecessors = new Map(nodes.map((n) => [n.id, []]));
  const successors = new Map(nodes.map((n) => [n.id, []]));
  for (const edge of edges) {
    if (edge.back) continue;
    predecessors.get(edge.to).push(edge.from);
    successors.get(edge.from).push(edge.to);
  }

  const sweep = (indices, neighbours) => {
    for (const layerIndex of indices) {
      const adjacentIndex = layerIndex + (neighbours === predecessors ? -1 : 1);
      if (adjacentIndex < 0 || adjacentIndex >= layers.length) continue;
      const order = new Map(layers[adjacentIndex].map((id, i) => [id, i]));
      const prior = new Map(layers[layerIndex].map((id, i) => [id, i]));
      layers[layerIndex].sort((a, b) => {
        const score = (id) => {
          const positions = neighbours.get(id).filter((n) => order.has(n)).map((n) => order.get(n)).sort((x, y) => x - y);
          if (!positions.length) return prior.get(id);
          const middle = Math.floor(positions.length / 2);
          return positions.length % 2 ? positions[middle] : (positions[middle - 1] + positions[middle]) / 2;
        };
        const delta = score(a) - score(b);
        return delta || prior.get(a) - prior.get(b) || cmpText(a, b);
      });
    }
  };

  for (let pass = 0; pass < sweeps; pass += 1) {
    sweep(Array.from({ length: maxLayer }, (_, i) => i + 1), predecessors);
    sweep(Array.from({ length: maxLayer }, (_, i) => maxLayer - i - 1), successors);
  }
  return layers;
}

function overlaps(a, b, gap = 10) {
  return Math.abs(a.x - b.x) < (a.width + b.width) / 2 + gap
    && Math.abs(a.y - b.y) < (a.height + b.height) / 2 + gap;
}

function assignCoordinates(nodes, layers, options) {
  const byID = new Map(nodes.map((n) => [n.id, n]));
  const placed = [];
  const positions = new Map();
  const layerHeights = layers.map((ids) => Math.max(0, ...ids.map((id) => byID.get(id).height)));
  const layerY = [];
  let cursorY = options.marginY;
  for (let i = 0; i < layers.length; i += 1) {
    layerY[i] = cursorY + layerHeights[i] / 2;
    cursorY += layerHeights[i] + options.rankSep;
  }

  // Pinned coordinates are editor-owned and absolute. Put them in the obstacle
  // set first so every automatic node lays out around every pin, even a pin
  // moved visually outside its semantic layer.
  for (const node of nodes) {
    if (!node.pinned) continue;
    const position = {
      id: node.id,
      x: finite(node.pinned.x, 0),
      y: finite(node.pinned.y, 0),
      width: node.width,
      height: node.height,
      layer: layers.findIndex((ids) => ids.includes(node.id)),
      pinned: true,
    };
    positions.set(node.id, position);
    placed.push(position);
  }

  layers.forEach((ids, layerIndex) => {
    let cursorX = options.marginX;
    for (const id of ids) {
      const node = byID.get(id);
      if (node.pinned) continue;
      const position = {
        id,
        x: cursorX + node.width / 2,
        y: layerY[layerIndex],
        width: node.width,
        height: node.height,
        layer: layerIndex,
        pinned: false,
      };
      // Deterministically slide right until arbitrary pinned positions and
      // nodes already placed in other layers cannot overlap this node.
      let blocker;
      while ((blocker = placed.find((other) => overlaps(position, other)))) {
        position.x = blocker.x + (blocker.width + position.width) / 2 + options.nodeSep;
      }
      positions.set(id, position);
      placed.push(position);
      cursorX = position.x + node.width / 2 + options.nodeSep;
    }
  });
  return positions;
}

function boundaryPoint(node, toward, outgoing) {
  const dx = toward.x - node.x;
  const dy = toward.y - node.y;
  if (node.type === 'decision') {
    const scale = 1 / ((Math.abs(dx) / (node.width / 2)) + (Math.abs(dy) / (node.height / 2)) || 1);
    return { x: node.x + dx * scale, y: node.y + dy * scale };
  }
  if (node.type === 'wait' || node.type === 'start' || node.type === 'end') {
    const length = Math.hypot(dx, dy) || 1;
    const radius = Math.min(node.width, node.height) / 2;
    return { x: node.x + dx / length * radius, y: node.y + dy / length * radius };
  }
  // Task/compound pins are absolute and may deliberately invert the visual
  // direction of a semantic edge. Intersect the centre-to-centre ray with the
  // rectangle instead of assuming every exit is bottom and every entry top.
  if (dx === 0 && dy === 0) return { x: node.x, y: node.y + (outgoing ? node.height / 2 : -node.height / 2) };
  const xScale = dx === 0 ? Infinity : (node.width / 2) / Math.abs(dx);
  const yScale = dy === 0 ? Infinity : (node.height / 2) / Math.abs(dy);
  const scale = Math.min(xScale, yScale);
  return { x: node.x + dx * scale, y: node.y + dy * scale };
}

function routeForward(edge, from, to, lane, nodes, edgeSep) {
  let start = boundaryPoint(from, to, true);
  let end = boundaryPoint(to, from, false);
  if (to.layer - from.layer > 1) {
    // Sugiyama normally inserts dummy nodes for every crossed rank. Small
    // process graphs need only the equivalent obstacle guarantee: take a
    // deterministic lane left of every intermediate-rank rectangle, keeping
    // the long edge out of aligned nodes while back-edges own the right side.
    const intermediate = nodes.filter((node) => node.layer > from.layer && node.layer < to.layer);
    const left = Math.min(from.x - from.width / 2, to.x - to.width / 2,
      ...intermediate.map((node) => node.x - node.width / 2));
    const outsideX = left - 44 - lane * edgeSep;
    // Enter the outside lane from each shape's leftmost boundary. Using a
    // centre-to-centre circle/diamond intersection here could make the first
    // horizontal segment cut back through its own endpoint shape.
    start = { x: from.x - from.width / 2, y: from.y };
    end = { x: to.x - to.width / 2, y: to.y };
    const path = `M ${start.x} ${start.y} L ${outsideX} ${start.y} L ${outsideX} ${end.y} L ${end.x} ${end.y}`;
    return {
      path,
      points: [start, { x: outsideX, y: start.y }, { x: outsideX, y: end.y }, end],
      label: { x: outsideX + 7, y: (start.y + end.y) / 2 },
    };
  }
  const laneOffset = ((lane % 3) - 1) * edgeSep;
  const midY = (start.y + end.y) / 2 + laneOffset;
  const path = `M ${start.x} ${start.y} C ${start.x} ${midY}, ${end.x} ${midY}, ${end.x} ${end.y}`;
  return { path, points: [start, { x: start.x, y: midY }, { x: end.x, y: midY }, end], label: { x: (start.x + end.x) / 2, y: midY - 8 } };
}

function routeBack(edge, from, to, lane, bounds) {
  const start = { x: from.x + from.width / 2, y: from.y };
  const end = { x: to.x + to.width / 2, y: to.y };
  const outsideX = bounds.maxX + 44 + lane;
  const path = `M ${start.x} ${start.y} C ${outsideX} ${start.y}, ${outsideX} ${end.y}, ${end.x} ${end.y}`;
  return { path, points: [start, { x: outsideX, y: start.y }, { x: outsideX, y: end.y }, end], label: { x: outsideX - 7, y: (start.y + end.y) / 2 } };
}

function graphBounds(nodes, marginX, marginY) {
  if (!nodes.length) return { x: 0, y: 0, width: marginX * 2, height: marginY * 2, minX: 0, minY: 0, maxX: marginX * 2, maxY: marginY * 2 };
  const minX = Math.min(...nodes.map((n) => n.x - n.width / 2)) - marginX;
  const minY = Math.min(...nodes.map((n) => n.y - n.height / 2)) - marginY;
  const maxX = Math.max(...nodes.map((n) => n.x + n.width / 2)) + marginX;
  const maxY = Math.max(...nodes.map((n) => n.y + n.height / 2)) + marginY;
  return { x: minX, y: minY, width: maxX - minX, height: maxY - minY, minX, minY, maxX, maxY };
}

// layoutProcessGraph is the entire swappable layout boundary: plain graph in,
// serialisable positions/routes out. The renderer depends on no intermediate
// Sugiyama data structure, so a vendored dagre-class implementation can replace
// this module later without touching editor/viewer callers.
export function layoutProcessGraph(graph, overrides = {}) {
  const options = { ...PROCESS_LAYOUT_DEFAULTS, ...overrides };
  const nodes = stableNodes(graph || {});
  const byID = new Map(nodes.map((n) => [n.id, n]));
  const edges = stableEdges(graph || {}, byID);
  const feedbackArc = typeof options.feedbackArc === 'function' ? options.feedbackArc : defaultFeedbackArc;
  const layered = assignLayers(nodes, edges, feedbackArc);
  const layers = reduceCrossings(nodes, layered.layer, layered.edges, Math.max(0, finite(options.sweeps, 6)));
  const positions = assignCoordinates(nodes, layers, options);
  const laidNodes = nodes.map((node) => ({ ...node, ...positions.get(node.id) }));
  const laidByID = new Map(laidNodes.map((n) => [n.id, n]));
  const bounds = graphBounds(laidNodes, options.marginX, options.marginY);
  let forwardLane = 0;
  let backLane = 0;
  const laidEdges = layered.edges.map((edge) => {
    const from = laidByID.get(edge.from);
    const to = laidByID.get(edge.to);
    const route = edge.back
      ? routeBack(edge, from, to, backLane++ * options.edgeSep, bounds)
      : routeForward(edge, from, to, forwardLane++, laidNodes, options.edgeSep);
    return { ...edge, ...route, kind: edge.back ? 'back' : 'forward' };
  });
  const routedBounds = graphBounds([
    ...laidNodes,
    ...laidEdges.flatMap((edge) => edge.points.map((point) => ({ ...point, width: 0, height: 0 }))),
  ], options.marginX, options.marginY);
  return { nodes: laidNodes, edges: laidEdges, layers: layers.map((ids) => [...ids]), bounds: routedBounds };
}
