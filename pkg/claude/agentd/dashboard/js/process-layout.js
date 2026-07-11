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
    return { ...raw, ...nodeSize(raw), id };
  }).sort((a, b) => cmpText(a.id, b.id));
}

function stableEdges(graph, byID) {
  const occurrences = new Map();
  const explicitIDs = new Set();
  return (graph.edges || []).map((raw, inputIndex) => {
    const from = String(raw.from || '');
    const to = String(raw.to || '');
    if (!byID.has(from) || !byID.has(to)) {
      throw new Error(`process layout: edge ${from || '?'} -> ${to || '?'} references an unknown node`);
    }
    const semantic = JSON.stringify([from, to, raw.outcome || '', raw.joinOnTarget || '', raw.back === true]);
    const occurrence = occurrences.get(semantic) || 0;
    occurrences.set(semantic, occurrence + 1);
    const explicitID = raw.id != null ? String(raw.id) : '';
    if (explicitID && explicitIDs.has(explicitID)) {
      throw new Error(`process layout: duplicate edge id ${explicitID}`);
    }
    if (explicitID) explicitIDs.add(explicitID);
    const id = explicitID ? `id:${explicitID}` : `semantic:${encodeURIComponent(semantic)}:${occurrence}`;
    return {
      ...raw,
      from,
      to,
      id,
      inputIndex,
      key: id,
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
  // Iterative DFS keeps a very large imported chain from overflowing the
  // JavaScript call stack while preserving the recursive traversal's order.
  for (const node of nodes) {
    if (state.has(node.id)) continue;
    state.set(node.id, 1);
    const stack = [{ id: node.id, index: 0 }];
    while (stack.length) {
      const frame = stack[stack.length - 1];
      const list = outgoing.get(frame.id);
      if (frame.index >= list.length) {
        state.set(frame.id, 2);
        stack.pop();
        continue;
      }
      const edge = list[frame.index++];
      const next = state.get(edge.to) || 0;
      if (next === 1) feedback.add(edge.inputIndex);
      else if (next === 0) {
        state.set(edge.to, 1);
        stack.push({ id: edge.to, index: 0 });
      }
    }
  }
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
      // A total key tuple: pinned partition, pin x, then id. Mixing x-order for
      // pinned pairs with id-order for mixed pairs creates comparator cycles
      // whose output differs across JS engines.
      if (Boolean(pa) !== Boolean(pb)) return pa ? -1 : 1;
      if (pa && finite(pa.x, 0) !== finite(pb.x, 0)) return finite(pa.x, 0) - finite(pb.x, 0);
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

// edgeEndpoint reconciles the graph's fixed top/bottom interaction ports with
// routing. A route approaching within this vertical cone terminates exactly at
// the matching port; inverted and strongly sideways geometry keeps the shape-
// boundary intersection so pinned back/side links do not wrap awkwardly.
export function edgeEndpoint(node, toward, outgoing) {
  const dx = toward.x - node.x;
  const dy = toward.y - node.y;
  const naturalDistance = outgoing ? dy : -dy;
  if (naturalDistance > 0 && Math.abs(dx) <= naturalDistance * 2) {
    return { x: node.x, y: node.y + (outgoing ? node.height / 2 : -node.height / 2) };
  }
  return boundaryPoint(node, toward, outgoing);
}

function pointKey(point) {
  return `${point.x}\u0000${point.y}`;
}

function segmentBlocked(a, b, obstacles) {
  if (a.x === b.x) {
    const low = Math.min(a.y, b.y);
    const high = Math.max(a.y, b.y);
    return obstacles.some((box) => a.x > box.left && a.x < box.right && high > box.top && low < box.bottom);
  }
  const low = Math.min(a.x, b.x);
  const high = Math.max(a.x, b.x);
  return obstacles.some((box) => a.y > box.top && a.y < box.bottom && high > box.left && low < box.right);
}

function visibleFallbackRoute(from, to, lane, edgeSep) {
  if (from.x === to.x && from.y === to.y) {
    const radius = Math.max(from.width, from.height, to.width, to.height) / 2
      + 36 + lane * Math.max(4, edgeSep / 3);
    return [
      { x: from.x + from.width / 2, y: from.y - 9 },
      { x: from.x + radius, y: from.y - radius },
      { x: from.x + radius, y: from.y + radius },
      { x: to.x + to.width / 2, y: to.y + 9 },
    ];
  }
  return [edgeEndpoint(from, to, true), edgeEndpoint(to, from, false)];
}

function orthogonalRoute(from, to, nodes, lane, edgeSep) {
  if (from.x === to.x && from.y === to.y) return visibleFallbackRoute(from, to, lane, edgeSep);
  const gap = 14 + (lane % 3) * Math.max(4, edgeSep / 3);
  const obstacles = nodes.filter((node) => node.id !== from.id && node.id !== to.id).map((node) => ({
    left: node.x - node.width / 2 - gap,
    right: node.x + node.width / 2 + gap,
    top: node.y - node.height / 2 - gap,
    bottom: node.y + node.height / 2 + gap,
  }));
  const allLeft = Math.min(...nodes.map((node) => node.x - node.width / 2)) - gap - 30;
  const allRight = Math.max(...nodes.map((node) => node.x + node.width / 2)) + gap + 30;
  const allTop = Math.min(...nodes.map((node) => node.y - node.height / 2)) - gap - 30;
  const allBottom = Math.max(...nodes.map((node) => node.y + node.height / 2)) + gap + 30;
  const xs = [...new Set([from.x, to.x, allLeft, allRight,
    ...obstacles.flatMap((box) => [box.left, box.right])])].sort((a, b) => a - b);
  const ys = [...new Set([from.y, to.y, allTop, allBottom,
    ...obstacles.flatMap((box) => [box.top, box.bottom])])].sort((a, b) => a - b);
  const inside = (point) => obstacles.some((box) => point.x > box.left && point.x < box.right
    && point.y > box.top && point.y < box.bottom);
  const points = [];
  const byKey = new Map();
  const columns = new Map(xs.map((x) => [x, []]));
  const rows = new Map(ys.map((y) => [y, []]));
  for (const x of xs) {
    for (const y of ys) {
      const point = { x, y };
      if (!inside(point)) {
        points.push(point);
        byKey.set(pointKey(point), point);
        columns.get(x).push(point);
        rows.get(y).push(point);
      }
    }
  }
  const adjacency = new Map(points.map((point) => [pointKey(point), []]));
  const connectLine = (line) => {
    line.sort((a, b) => a.x === b.x ? a.y - b.y : a.x - b.x);
    for (let i = 1; i < line.length; i += 1) {
      const a = line[i - 1];
      const b = line[i];
      if (segmentBlocked(a, b, obstacles)) continue;
      const distance = Math.abs(a.x - b.x) + Math.abs(a.y - b.y);
      adjacency.get(pointKey(a)).push({ point: b, distance });
      adjacency.get(pointKey(b)).push({ point: a, distance });
    }
  };
  for (const x of xs) connectLine(columns.get(x));
  for (const y of ys) connectLine(rows.get(y));

  const startKey = pointKey({ x: from.x, y: from.y });
  const endKey = pointKey({ x: to.x, y: to.y });
  const distance = new Map([[startKey, 0]]);
  const previous = new Map();
  const visited = new Set();
  const heap = [];
  const heapLess = (a, b) => a.cost < b.cost || (a.cost === b.cost && cmpText(a.key, b.key) < 0);
  const heapPush = (entry) => {
    heap.push(entry);
    let index = heap.length - 1;
    while (index > 0) {
      const parent = Math.floor((index - 1) / 2);
      if (!heapLess(heap[index], heap[parent])) break;
      [heap[index], heap[parent]] = [heap[parent], heap[index]];
      index = parent;
    }
  };
  const heapPop = () => {
    const first = heap[0];
    const last = heap.pop();
    if (heap.length && last) {
      heap[0] = last;
      let index = 0;
      while (true) {
        const left = index * 2 + 1;
        const right = left + 1;
        let smallest = index;
        if (left < heap.length && heapLess(heap[left], heap[smallest])) smallest = left;
        if (right < heap.length && heapLess(heap[right], heap[smallest])) smallest = right;
        if (smallest === index) break;
        [heap[index], heap[smallest]] = [heap[smallest], heap[index]];
        index = smallest;
      }
    }
    return first;
  };
  // Overlapping pins may place an endpoint centre inside another node's
  // inflated obstacle, so it is intentionally absent from this free grid. In
  // that transient editor state an obstacle-free route is impossible; return a
  // visible direct fallback instead of throwing or leaving a half-mounted UI.
  if (!adjacency.has(startKey) || !adjacency.has(endKey)) {
    return visibleFallbackRoute(from, to, lane, edgeSep);
  }
  heapPush({ key: startKey, cost: 0 });
  while (heap.length) {
    const entry = heapPop();
    const current = entry.key;
    if (visited.has(current) || entry.cost !== distance.get(current)) continue;
    visited.add(current);
    if (current === endKey) break;
    for (const next of adjacency.get(current)) {
      const key = pointKey(next.point);
      if (visited.has(key)) continue;
      const candidate = distance.get(current) + next.distance;
      if (!distance.has(key) || candidate < distance.get(key)) {
        distance.set(key, candidate);
        previous.set(key, current);
        heapPush({ key, cost: candidate });
      }
    }
  }
  if (!distance.has(endKey)) return visibleFallbackRoute(from, to, lane, edgeSep);
  const route = [];
  for (let key = endKey; key; key = previous.get(key)) route.push(byKey.get(key));
  route.reverse();
  const compressed = route.filter((point, index) => {
    if (index === 0 || index === route.length - 1) return true;
    const before = route[index - 1];
    const after = route[index + 1];
    return !((before.x === point.x && point.x === after.x) || (before.y === point.y && point.y === after.y));
  });
  if (compressed.length < 2 || !compressed[0] || !compressed[1]) {
    return visibleFallbackRoute(from, to, lane, edgeSep);
  }
  compressed[0] = edgeEndpoint(from, compressed[1], true);
  compressed[compressed.length - 1] = edgeEndpoint(to, compressed[compressed.length - 2], false);
  return compressed;
}

function routeLabel(points) {
  const segments = points.slice(1).map((point, index) => ({
    from: points[index], to: point,
    length: Math.abs(point.x - points[index].x) + Math.abs(point.y - points[index].y),
  }));
  const total = segments.reduce((sum, segment) => sum + segment.length, 0);
  let remaining = total / 2;
  for (const segment of segments) {
    if (remaining <= segment.length) {
      const ratio = segment.length ? remaining / segment.length : 0;
      return {
        x: segment.from.x + (segment.to.x - segment.from.x) * ratio + 7,
        y: segment.from.y + (segment.to.y - segment.from.y) * ratio - 7,
      };
    }
    remaining -= segment.length;
  }
  return { ...points[Math.floor(points.length / 2)] };
}

function routeForward(edge, from, to, lane, nodes, edgeSep) {
  let start = edgeEndpoint(from, to, true);
  let end = edgeEndpoint(to, from, false);
  if (from.x === to.x && from.y === to.y) {
    const points = visibleFallbackRoute(from, to, lane, edgeSep);
    return {
      path: points.map((point, index) => `${index ? 'L' : 'M'} ${point.x} ${point.y}`).join(' '),
      points,
      label: routeLabel(points),
    };
  }
  if (to.layer - from.layer > 1) {
    // This is the dummy-rank equivalent for small graphs: a deterministic
    // Manhattan visibility graph routes around every node rectangle, including
    // siblings on the source/target ranks and arbitrarily positioned pins.
    const points = orthogonalRoute(from, to, nodes, lane, edgeSep);
    const path = points.map((point, index) => `${index ? 'L' : 'M'} ${point.x} ${point.y}`).join(' ');
    return {
      path,
      points,
      label: routeLabel(points),
    };
  }
  const laneOffset = ((lane % 3) - 1) * edgeSep;
  const midY = (start.y + end.y) / 2 + laneOffset;
  const path = `M ${start.x} ${start.y} C ${start.x} ${midY}, ${end.x} ${midY}, ${end.x} ${end.y}`;
  return { path, points: [start, { x: start.x, y: midY }, { x: end.x, y: midY }, end], label: { x: (start.x + end.x) / 2, y: midY - 8 } };
}

function routeBack(edge, from, to, lane, bounds) {
  if (from.id === to.id || (from.x === to.x && from.y === to.y)) {
    const radius = Math.max(from.width, from.height) / 2 + 42 + lane;
    const start = { x: from.x + from.width / 2, y: from.y - 10 };
    const end = { x: from.x + from.width / 2, y: from.y + 10 };
    const outsideX = from.x + radius;
    const path = `M ${start.x} ${start.y} C ${outsideX} ${from.y - radius}, ${outsideX} ${from.y + radius}, ${end.x} ${end.y}`;
    return {
      path,
      points: [start, { x: outsideX, y: from.y - radius }, { x: outsideX, y: from.y + radius }, end],
      label: { x: outsideX - 7, y: from.y },
    };
  }
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
