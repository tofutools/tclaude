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
  parallel: [108, 108],
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
  if (node.type === 'decision' || node.type === 'parallel') {
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

function isPortEndpoint(node, point, outgoing) {
  return point.x === node.x
    && point.y === node.y + (outgoing ? node.height / 2 : -node.height / 2);
}

// Boundary fallbacks must approach along the centre-to-boundary ray. SVG markers use
// the rendered path's final tangent, so feeding every cubic a vertical final
// control segment makes a side-routed arrow point down even though its endpoint
// sits on the node's side. Port-snapped endpoints intentionally retain their
// vertical tangent; every other shape uses that same endpoint ray.
export function endpointTangent(node, point, outgoing) {
  if (isPortEndpoint(node, point, outgoing)) return { x: 0, y: 1 };
  const dx = point.x - node.x;
  const dy = point.y - node.y;
  const length = Math.hypot(dx, dy);
  if (!length) return { x: 0, y: outgoing ? 1 : -1 };
  const direction = outgoing ? 1 : -1;
  return { x: direction * dx / length, y: direction * dy / length };
}

// The points arrays mirror SVG path command geometry: cubic routes contain
// [start, control1, control2, end], while orthogonal routes contain each line
// waypoint. Walking backwards also handles coincident controls safely.
export function terminalTangent(points) {
  const end = points?.at(-1);
  if (!end) return { x: 0, y: 0 };
  for (let index = points.length - 2; index >= 0; index -= 1) {
    const dx = end.x - points[index].x;
    const dy = end.y - points[index].y;
    const length = Math.hypot(dx, dy);
    if (length) return { x: dx / length, y: dy / length };
  }
  return { x: 0, y: 0 };
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

// edgeOrientation classifies an edge by its OVERALL delta, not by the local run
// its label happens to sit on. Decorations anchored to the label (the editor's
// pin toggle) hang below a horizontal edge, where there is empty canvas, but go
// beside a vertical one -- below is where the target node is.
//
// The local-segment reading is the tempting refinement and is wrong for exactly
// that purpose: a tall edge routes down-across-down, so its label lands on the
// short horizontal jog, and the space below that jog is the target node. Ties
// count as horizontal, matching the older always-below behaviour.
function edgeOrientation(from, to) {
  return Math.abs(to.y - from.y) > Math.abs(to.x - from.x) ? 'vertical' : 'horizontal';
}

function polylineMidpoint(points) {
  const segments = points.slice(1).map((point, index) => ({
    from: points[index], to: point,
    length: Math.hypot(point.x - points[index].x, point.y - points[index].y),
  }));
  const total = segments.reduce((sum, segment) => sum + segment.length, 0);
  let remaining = total / 2;
  for (const segment of segments) {
    if (remaining <= segment.length) {
      const ratio = segment.length ? remaining / segment.length : 0;
      return {
        x: segment.from.x + (segment.to.x - segment.from.x) * ratio,
        y: segment.from.y + (segment.to.y - segment.from.y) * ratio,
        orientation: edgeOrientation(points[0], points[points.length - 1]),
      };
    }
    remaining -= segment.length;
  }
  return { ...points[Math.floor(points.length / 2)] };
}

function cubicPoint([start, control1, control2, end], ratio) {
  const inverse = 1 - ratio;
  const inverseSquared = inverse * inverse;
  const ratioSquared = ratio * ratio;
  return {
    x: inverseSquared * inverse * start.x + 3 * inverseSquared * ratio * control1.x
      + 3 * inverse * ratioSquared * control2.x + ratioSquared * ratio * end.x,
    y: inverseSquared * inverse * start.y + 3 * inverseSquared * ratio * control1.y
      + 3 * inverse * ratioSquared * control2.y + ratioSquared * ratio * end.y,
  };
}

function cubicPathMidpoint(points) {
  // Cubic paths are stored as [P0, P1, P2, P3, P4, P5, P6, ...], with each
  // command after the first sharing the preceding command's endpoint. Flatten
  // them deterministically so the label follows half the visible path length,
  // rather than t=0.5 (which is not the distance midpoint on asymmetric curves).
  const samples = [];
  // 24 chords balance reroute cost with around-pixel midpoint accuracy even on
  // broad curves; practical templates are far smaller than the authoring bound.
  const steps = 24;
  for (let offset = 0; offset + 3 < points.length; offset += 3) {
    const curve = points.slice(offset, offset + 4);
    for (let step = 0; step <= steps; step += 1) {
      const ratio = step / steps;
      samples.push({ point: cubicPoint(curve, ratio), offset, ratio });
    }
  }
  let total = 0;
  for (let index = 1; index < samples.length; index += 1) {
    const before = samples[index - 1].point;
    const after = samples[index].point;
    samples[index].length = Math.hypot(after.x - before.x, after.y - before.y);
    total += samples[index].length;
  }
  let remaining = total / 2;
  for (let index = 1; index < samples.length; index += 1) {
    // Adjacent cubics both emit their shared endpoint. Skip that zero-length
    // boundary so an exact half at the join cannot borrow the next curve's
    // offset while retaining the preceding curve's ratio.
    if (!samples[index].length) continue;
    if (remaining <= samples[index].length) {
      const before = samples[index - 1];
      const after = samples[index];
      const within = after.length ? remaining / after.length : 0;
      const ratio = before.ratio + (after.ratio - before.ratio) * within;
      const curveOffset = after.offset;
      return {
        ...cubicPoint(points.slice(curveOffset, curveOffset + 4), ratio),
        orientation: edgeOrientation(points[0], points[points.length - 1]),
      };
    }
    remaining -= samples[index].length;
  }
  return { ...points.at(-1), orientation: edgeOrientation(points[0], points.at(-1)) };
}

function routeForward(edge, from, to, lane, nodes, edgeSep) {
  let start = edgeEndpoint(from, to, true);
  let end = edgeEndpoint(to, from, false);
  if (from.x === to.x && from.y === to.y) {
    const points = visibleFallbackRoute(from, to, lane, edgeSep);
    return {
      path: points.map((point, index) => `${index ? 'L' : 'M'} ${point.x} ${point.y}`).join(' '),
      points,
      label: polylineMidpoint(points),
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
      label: polylineMidpoint(points),
    };
  }
  const laneOffset = ((lane % 3) - 1) * edgeSep;
  const midY = (start.y + end.y) / 2 + laneOffset;
  const startSnapped = isPortEndpoint(from, start, true);
  const endSnapped = isPortEndpoint(to, end, false);
  if (startSnapped && endSnapped) {
    const points = [start, { x: start.x, y: midY }, { x: end.x, y: midY }, end];
    const path = `M ${start.x} ${start.y} C ${start.x} ${midY}, ${end.x} ${midY}, ${end.x} ${end.y}`;
    return {
      path,
      points,
      label: cubicPathMidpoint(points),
    };
  }

  // A lane-offset midpoint keeps parallel fallback edges visually distinct.
  // Two cubics let the route bend through that midpoint without sacrificing
  // either endpoint tangent (a single cubic has only those two control slots).
  const chordX = end.x - start.x;
  const chordY = end.y - start.y;
  const chordLength = Math.hypot(chordX, chordY) || 1;
  const chord = { x: chordX / chordLength, y: chordY / chordLength };
  const midpoint = {
    x: (start.x + end.x) / 2 - chord.y * laneOffset,
    y: (start.y + end.y) / 2 + chord.x * laneOffset,
  };
  const handle = Math.max(24, chordLength / 4);
  const startTangent = endpointTangent(from, start, true);
  const endTangent = endpointTangent(to, end, false);
  const control1 = { x: start.x + startTangent.x * handle, y: start.y + startTangent.y * handle };
  const midpointIn = { x: midpoint.x - chord.x * handle, y: midpoint.y - chord.y * handle };
  const midpointOut = { x: midpoint.x + chord.x * handle, y: midpoint.y + chord.y * handle };
  const control2 = { x: end.x - endTangent.x * handle, y: end.y - endTangent.y * handle };
  const points = [start, control1, midpointIn, midpoint, midpointOut, control2, end];
  const path = `M ${start.x} ${start.y} C ${control1.x} ${control1.y}, ${midpointIn.x} ${midpointIn.y}, ${midpoint.x} ${midpoint.y} C ${midpointOut.x} ${midpointOut.y}, ${control2.x} ${control2.y}, ${end.x} ${end.y}`;
  return {
    path,
    points,
    label: cubicPathMidpoint(points),
  };
}

function routeBack(edge, from, to, lane, bounds) {
  if (from.id === to.id || (from.x === to.x && from.y === to.y)) {
    const radius = Math.max(from.width, from.height) / 2 + 42 + lane;
    const start = { x: from.x + from.width / 2, y: from.y - 10 };
    const end = { x: from.x + from.width / 2, y: from.y + 10 };
    const outsideX = from.x + radius;
    const path = `M ${start.x} ${start.y} C ${outsideX} ${from.y - radius}, ${outsideX} ${from.y + radius}, ${end.x} ${end.y}`;
    const points = [start, { x: outsideX, y: from.y - radius }, { x: outsideX, y: from.y + radius }, end];
    return {
      path,
      points,
      label: cubicPathMidpoint(points),
    };
  }
  const start = { x: from.x + from.width / 2, y: from.y };
  const end = { x: to.x + to.width / 2, y: to.y };
  const outsideX = bounds.maxX + 44 + lane;
  const path = `M ${start.x} ${start.y} C ${outsideX} ${start.y}, ${outsideX} ${end.y}, ${end.x} ${end.y}`;
  const points = [start, { x: outsideX, y: start.y }, { x: outsideX, y: end.y }, end];
  return { path, points, label: cubicPathMidpoint(points) };
}

function graphBounds(nodes, marginX, marginY) {
  if (!nodes.length) return { x: 0, y: 0, width: marginX * 2, height: marginY * 2, minX: 0, minY: 0, maxX: marginX * 2, maxY: marginY * 2 };
  const minX = Math.min(...nodes.map((n) => n.x - n.width / 2)) - marginX;
  const minY = Math.min(...nodes.map((n) => n.y - n.height / 2)) - marginY;
  const maxX = Math.max(...nodes.map((n) => n.x + n.width / 2)) + marginX;
  const maxY = Math.max(...nodes.map((n) => n.y + n.height / 2)) + marginY;
  return { x: minX, y: minY, width: maxX - minX, height: maxY - minY, minX, minY, maxX, maxY };
}

// rerouteProcessLayout recomputes edge geometry against transient node
// positions without repeating layering/crossing work. The editor uses this on
// every drag frame; the model and the stable layout remain untouched until the
// pointerup commit.
export function rerouteProcessLayout(layout, positions, overrides = {}) {
  const options = { ...PROCESS_LAYOUT_DEFAULTS, ...overrides };
  const transient = positions != null;
  const moved = positions instanceof Map ? positions : new Map(Object.entries(positions || {}));
  const nodes = (layout?.nodes || []).map((node) => {
    const position = moved.get(node.id);
    return position ? { ...node, x: position.x, y: position.y } : { ...node };
  });
  const byID = new Map(nodes.map((node) => [node.id, node]));
  const bounds = graphBounds(nodes, options.marginX, options.marginY);
  const stableBounds = transient
    ? graphBounds(layout?.nodes || [], options.marginX, options.marginY)
    : bounds;
  const movedNodes = nodes.filter((node) => moved.has(node.id));
  const backBoundsChanged = transient && bounds.maxX !== stableBounds.maxX;
  let forwardLane = 0;
  let backLane = 0;
  const edges = (layout?.edges || []).map((edge) => {
    const lane = edge.back ? backLane++ : forwardLane++;
    const from = byID.get(edge.from);
    const to = byID.get(edge.to);
    if (!from || !to) return { ...edge };
    const incident = moved.has(edge.from) || moved.has(edge.to);
    let obstacleInvalidated = false;
    if (!incident && !edge.back && to.layer - from.layer > 1 && movedNodes.length) {
      const gap = 14 + (lane % 3) * Math.max(4, options.edgeSep / 3);
      const movedObstacles = movedNodes.map((node) => ({
        left: node.x - node.width / 2 - gap,
        right: node.x + node.width / 2 + gap,
        top: node.y - node.height / 2 - gap,
        bottom: node.y + node.height / 2 + gap,
      }));
      obstacleInvalidated = (edge.points || []).slice(1).some((point, index) => (
        segmentBlocked(edge.points[index], point, movedObstacles)
      ));
    }
    // Preserve stable routes unless their endpoint moved, a moved node now
    // obstructs a long route, or the right-hand bound that owns return lanes
    // changed. This keeps ordinary frames cheap without leaving stale paths.
    if (transient && !incident && !obstacleInvalidated && !(edge.back && backBoundsChanged)) {
      return { ...edge };
    }
    const route = edge.back
      ? routeBack(edge, from, to, lane * options.edgeSep, bounds)
      : routeForward(edge, from, to, lane, nodes, options.edgeSep);
    return { ...edge, ...route, kind: edge.back ? 'back' : 'forward' };
  });
  const routedBounds = graphBounds([
    ...nodes,
    ...edges.flatMap((edge) => (edge.points || []).map((point) => ({ ...point, width: 0, height: 0 }))),
  ], options.marginX, options.marginY);
  return { ...layout, nodes, edges, bounds: routedBounds };
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
  return rerouteProcessLayout({
    nodes: laidNodes,
    edges: layered.edges,
    layers: layers.map((ids) => [...ids]),
  }, null, options);
}
