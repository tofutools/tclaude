// board-model.js -- the pure half of the collab board (tcl-4f86kp): a shared
// drawing board the operator and agents edit together. Elements are a subset
// of Excalidraw's element JSON (so a board exports as .excalidraw), merged per
// element the way Excalidraw reconciles: a higher `version` wins, a lower
// `versionNonce` breaks a tie, and deletion is a tombstone (`isDeleted`).
//
// The server stores and merges boards (/api/boards, /v1/boards); agents read
// and write them with `tclaude agent board ...`. This module is DOM-free so
// the editor's geometry, merge and change summaries can be tested directly.

export const BOARD_TOOLS = Object.freeze([
  { id: 'select', key: 'v', label: 'Select', icon: '⬚' },
  { id: 'hand', key: 'h', label: 'Pan', icon: '✋' },
  { id: 'rectangle', key: 'r', label: 'Rectangle', icon: '▭' },
  { id: 'ellipse', key: 'o', label: 'Ellipse', icon: '◯' },
  { id: 'diamond', key: 'd', label: 'Diamond', icon: '◇' },
  { id: 'arrow', key: 'a', label: 'Arrow', icon: '→' },
  { id: 'line', key: 'l', label: 'Line', icon: '╱' },
  { id: 'freedraw', key: 'p', label: 'Pen', icon: '✎' },
  { id: 'text', key: 't', label: 'Text', icon: 'T' },
  { id: 'eraser', key: 'e', label: 'Eraser', icon: '⌫' },
]);

export const BOARD_COLORS = Object.freeze(['#1e1e1e', '#e03131', '#2f9e44', '#1971c2', '#f08c00', '#9c36b5']);
export const BOARD_FILLS = Object.freeze(['transparent', '#ffc9c9', '#b2f2bb', '#a5d8ff', '#ffec99', '#eebefa']);

const SHAPES = new Set(['rectangle', 'ellipse', 'diamond']);
const LINEAR = new Set(['arrow', 'line', 'freedraw']);
const BOARD_ID = /^[a-z0-9][a-z0-9-]{0,63}$/;

export function isBoardId(value) {
  return typeof value === 'string' && BOARD_ID.test(value);
}

export function boardIdFromTitle(title) {
  const id = String(title || '').toLowerCase().normalize('NFKD').replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 64);
  return isBoardId(id) ? id : '';
}

let idCounter = 0;
export function newElementId(random = Math.random) {
  idCounter = (idCounter + 1) % 1e6;
  return `el-${Date.now().toString(36)}-${Math.floor(random() * 1e9).toString(36)}${idCounter.toString(36)}`;
}

function nonce(random = Math.random) {
  return Math.floor(random() * 2 ** 31);
}

// newElement builds a fresh version-1 element with Excalidraw defaults.
export function newElement(type, props = {}, { random = Math.random, now = Date.now } = {}) {
  return {
    id: props.id || newElementId(random),
    type,
    x: 0, y: 0, width: 0, height: 0, angle: 0,
    strokeColor: '#1e1e1e', backgroundColor: 'transparent', fillStyle: 'solid',
    strokeWidth: type === 'freedraw' ? 2 : 2, strokeStyle: 'solid', roughness: 1, opacity: 100,
    ...(LINEAR.has(type) ? { points: [[0, 0]] } : {}),
    ...(type === 'text' ? { text: '', fontSize: 20, textAlign: 'left' } : {}),
    ...props,
    version: 1, versionNonce: nonce(random), isDeleted: false, updated: now(),
  };
}

// bump returns `el` with `changes` applied as a new local version.
export function bump(el, changes = {}, { random = Math.random, now = Date.now } = {}) {
  return { ...el, ...changes, version: (el.version || 0) + 1, versionNonce: nonce(random), updated: now() };
}

// wins reports whether `incoming` should replace `current` (Excalidraw's
// reconciliation rule).
export function wins(incoming, current) {
  if (!current) return true;
  if ((incoming.version || 0) !== (current.version || 0)) return (incoming.version || 0) > (current.version || 0);
  return (incoming.versionNonce || 0) < (current.versionNonce || 0);
}

// mergeElements folds incoming elements into a Map(id -> element), keeping
// the winners, and returns the ids that changed. `skip` (a Set) holds ids the
// operator is manipulating right now, whose local copy must not jump.
export function mergeElements(map, incoming, skip = null) {
  const changed = [];
  for (const el of incoming || []) {
    if (!el || typeof el.id !== 'string') continue;
    if (skip?.has(el.id)) continue;
    if (wins(el, map.get(el.id))) {
      map.set(el.id, el);
      changed.push(el.id);
    }
  }
  return changed;
}

export function liveElements(map) {
  return [...map.values()].filter((el) => !el.isDeleted);
}

// normalizeBox turns a drag from (x0,y0) to (x1,y1) into a positive box.
export function normalizeBox(x0, y0, x1, y1) {
  return { x: Math.min(x0, x1), y: Math.min(y0, y1), width: Math.abs(x1 - x0), height: Math.abs(y1 - y0) };
}

// elementBounds is the axis-aligned box an element covers, in board units.
export function elementBounds(el) {
  if (el.points && el.points.length) {
    let minX = Infinity; let minY = Infinity; let maxX = -Infinity; let maxY = -Infinity;
    for (const [px, py] of el.points) {
      minX = Math.min(minX, px); minY = Math.min(minY, py); maxX = Math.max(maxX, px); maxY = Math.max(maxY, py);
    }
    return { x: el.x + minX, y: el.y + minY, width: maxX - minX, height: maxY - minY };
  }
  if (el.type === 'text') {
    const lines = String(el.text || '').split('\n');
    const size = el.fontSize || 20;
    const width = el.width || Math.max(...lines.map((l) => l.length), 1) * size * 0.58;
    return { x: el.x, y: el.y, width, height: el.height || lines.length * size * 1.25 };
  }
  return { x: el.x, y: el.y, width: el.width, height: el.height };
}

export function center(el) {
  const b = elementBounds(el);
  return { x: b.x + b.width / 2, y: b.y + b.height / 2 };
}

function distToSegment(px, py, ax, ay, bx, by) {
  const dx = bx - ax; const dy = by - ay;
  const len = dx * dx + dy * dy;
  const t = len ? Math.max(0, Math.min(1, ((px - ax) * dx + (py - ay) * dy) / len)) : 0;
  return Math.hypot(px - (ax + t * dx), py - (ay + t * dy));
}

// hitTest returns whether board point (px,py) touches the element, with a
// tolerance in board units. Filled shapes hit inside; outlines hit near edges.
export function hitTest(el, px, py, tol = 6) {
  if (el.isDeleted) return false;
  if (el.points && el.points.length > 1) {
    for (let i = 1; i < el.points.length; i++) {
      const [ax, ay] = el.points[i - 1];
      const [bx, by] = el.points[i];
      if (distToSegment(px, py, el.x + ax, el.y + ay, el.x + bx, el.y + by) <= tol + (el.strokeWidth || 2)) return true;
    }
    return false;
  }
  const b = elementBounds(el);
  const inside = px >= b.x - tol && px <= b.x + b.width + tol && py >= b.y - tol && py <= b.y + b.height + tol;
  if (!inside) return false;
  if (el.type === 'text' || (el.backgroundColor && el.backgroundColor !== 'transparent')) return true;
  // An unfilled shape: near its outline only, so shapes drawn inside it stay clickable.
  const nearEdge = Math.min(Math.abs(px - b.x), Math.abs(px - (b.x + b.width)), Math.abs(py - b.y), Math.abs(py - (b.y + b.height)));
  if (el.type === 'ellipse') {
    const cx = b.x + b.width / 2; const cy = b.y + b.height / 2;
    const rx = Math.max(b.width / 2, 1); const ry = Math.max(b.height / 2, 1);
    const d = Math.sqrt(((px - cx) / rx) ** 2 + ((py - cy) / ry) ** 2);
    return Math.abs(d - 1) * Math.min(rx, ry) <= tol;
  }
  return nearEdge <= tol;
}

// topHit returns the topmost (last drawn) live element under the point.
export function topHit(elements, px, py, tol = 6) {
  for (let i = elements.length - 1; i >= 0; i--) {
    if (hitTest(elements[i], px, py, tol)) return elements[i];
  }
  return null;
}

// edgePoint clips the ray from an element's centre toward (tx,ty) at its box,
// so a bound arrow starts and ends on the shape's edge, not its middle.
export function edgePoint(el, tx, ty, gap = 4) {
  const b = elementBounds(el);
  const cx = b.x + b.width / 2; const cy = b.y + b.height / 2;
  const dx = tx - cx; const dy = ty - cy;
  if (!dx && !dy) return { x: cx, y: cy };
  const hw = b.width / 2 + gap; const hh = b.height / 2 + gap;
  const scale = 1 / Math.max(Math.abs(dx) / Math.max(hw, 1e-6), Math.abs(dy) / Math.max(hh, 1e-6));
  return { x: cx + dx * scale, y: cy + dy * scale };
}

// connectArrow returns arrow geometry (x, y, points) joining two elements.
export function connectArrow(from, to) {
  const ct = center(to); const cf = center(from);
  const a = edgePoint(from, ct.x, ct.y);
  const b = edgePoint(to, cf.x, cf.y);
  return { x: a.x, y: a.y, width: Math.abs(b.x - a.x), height: Math.abs(b.y - a.y), points: [[0, 0], [b.x - a.x, b.y - a.y]] };
}

// reboundArrows recomputes every arrow bound to one of `movedIds`, returning
// the bumped arrows to save.
export function reboundArrows(map, movedIds, opts) {
  const moved = new Set(movedIds);
  const out = [];
  for (const el of map.values()) {
    if (el.isDeleted || el.type !== 'arrow') continue;
    const fromId = el.startBinding?.elementId;
    const toId = el.endBinding?.elementId;
    if (!moved.has(fromId) && !moved.has(toId)) continue;
    const from = fromId && map.get(fromId);
    const to = toId && map.get(toId);
    if (from && !from.isDeleted && to && !to.isDeleted) {
      out.push(bump(el, connectArrow(from, to), opts));
    } else if (from && !from.isDeleted) {
      const end = el.points[el.points.length - 1];
      const tip = { x: el.x + end[0], y: el.y + end[1] };
      const a = edgePoint(from, tip.x, tip.y);
      out.push(bump(el, { x: a.x, y: a.y, points: [[0, 0], [tip.x - a.x, tip.y - a.y]] }, opts));
    } else if (to && !to.isDeleted) {
      const start = { x: el.x, y: el.y };
      const b = edgePoint(to, start.x, start.y);
      out.push(bump(el, { points: [[0, 0], [b.x - start.x, b.y - start.y]] }, opts));
    }
  }
  return out;
}

// shapeAt finds the shape (not a line) under a point, for arrow binding.
export function shapeAt(elements, px, py, ignoreId = '') {
  for (let i = elements.length - 1; i >= 0; i--) {
    const el = elements[i];
    if (el.isDeleted || el.id === ignoreId || (!SHAPES.has(el.type) && el.type !== 'text')) continue;
    const b = elementBounds(el);
    if (px >= b.x && px <= b.x + b.width && py >= b.y && py <= b.y + b.height) return el;
  }
  return null;
}

// simplifyStroke thins a freehand stroke: drops points closer than `min`
// board units to the last kept one.
export function simplifyStroke(points, min = 1.5) {
  if (points.length < 3) return points;
  const out = [points[0]];
  for (let i = 1; i < points.length - 1; i++) {
    const [lx, ly] = out[out.length - 1];
    if (Math.hypot(points[i][0] - lx, points[i][1] - ly) >= min) out.push(points[i]);
  }
  out.push(points[points.length - 1]);
  return out;
}

export function contentBounds(elements) {
  let minX = Infinity; let minY = Infinity; let maxX = -Infinity; let maxY = -Infinity;
  for (const el of elements) {
    if (el.isDeleted) continue;
    const b = elementBounds(el);
    minX = Math.min(minX, b.x); minY = Math.min(minY, b.y);
    maxX = Math.max(maxX, b.x + b.width); maxY = Math.max(maxY, b.y + b.height);
  }
  if (!Number.isFinite(minX)) return null;
  return { x: minX, y: minY, width: maxX - minX, height: maxY - minY };
}

// changeBounds is the board area an edit batch touched: where the changed
// elements are now and where they were before, padded with some context so
// the image shows what the change sits next to. Null when nothing is left.
export function changeBounds(map, changedIds, before, context = 80) {
  const touched = [];
  for (const id of new Set(changedIds)) {
    const now = map.get(id);
    if (now && !now.isDeleted) touched.push(now);
    const prev = before.get(id);
    if (prev && !prev.isDeleted) touched.push(prev);
  }
  const box = contentBounds(touched);
  if (!box) return null;
  return { x: box.x - context, y: box.y - context, width: box.width + context * 2, height: box.height + context * 2 };
}

function label(el, map) {
  if (el.type === 'text') return `text “${String(el.text || '').slice(0, 40)}”`;
  // A shape's label is the text inside it, when there is one.
  if (SHAPES.has(el.type) && map) {
    const inner = [...map.values()].find((t) => !t.isDeleted && t.type === 'text' && (() => {
      const c = center(t); const b = elementBounds(el);
      return c.x >= b.x && c.x <= b.x + b.width && c.y >= b.y && c.y <= b.y + b.height;
    })());
    if (inner) return `${el.type} “${String(inner.text || '').slice(0, 40)}”`;
  }
  return el.type;
}

function nearestLabelled(map, point, ignore) {
  let best = null; let bestD = Infinity;
  for (const el of map.values()) {
    if (el.isDeleted || ignore.has(el.id) || !(SHAPES.has(el.type) || el.type === 'text')) continue;
    const c = center(el);
    const d = Math.hypot(c.x - point.x, c.y - point.y);
    if (d < bestD) { bestD = d; best = el; }
  }
  return best && bestD < 600 ? best : null;
}

// describeChanges summarises the operator's recent edits as one short line
// for the board's participating agents: what was added, changed or removed,
// and where (near which labelled shape). `before` maps id -> the element as
// it was before the batch (undefined for new ones).
export function describeChanges(map, changedIds, before) {
  const added = []; const edited = []; const removed = [];
  for (const id of new Set(changedIds)) {
    const el = map.get(id);
    if (!el) continue;
    const prev = before.get(id);
    if (el.isDeleted) { if (prev && !prev.isDeleted) removed.push(prev); continue; }
    if (!prev || prev.isDeleted) added.push(el);
    else edited.push(el);
  }
  if (!added.length && !edited.length && !removed.length) return '';
  const ignore = new Set(changedIds);
  const parts = [];
  const group = (list, verb) => {
    if (!list.length) return;
    const strokes = list.filter((el) => el.type === 'freedraw');
    const named = list.filter((el) => el.type !== 'freedraw').map((el) => label(el, map));
    const bits = [...named.slice(0, 4)];
    if (named.length > 4) bits.push(`${named.length - 4} more`);
    if (strokes.length) {
      const near = nearestLabelled(map, center(strokes[strokes.length - 1]), ignore);
      bits.push(`${strokes.length} freehand stroke${strokes.length === 1 ? '' : 's'}${near ? ` near ${label(near, map)}` : ''}`);
    }
    parts.push(`${verb} ${bits.join(', ')}`);
  };
  group(added, 'drew');
  group(edited, 'changed');
  group(removed, 'removed');
  return parts.join('; ');
}

// toExcalidraw wraps elements as an .excalidraw file.
export function toExcalidraw(elements) {
  return JSON.stringify({ type: 'excalidraw', version: 2, source: 'tclaude', elements, appState: { viewBackgroundColor: '#ffffff' }, files: {} }, null, 2);
}
