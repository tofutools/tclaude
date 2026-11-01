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

// boardToolDigit is the number key of the tool at `index`: the keyboard's row,
// 1 to 9 and then 0. A tool past the tenth has none.
export function boardToolDigit(index) {
  return index < 9 ? String(index + 1) : index === 9 ? '0' : '';
}

export const BOARD_ZOOM_MIN = 0.2;
export const BOARD_ZOOM_MAX = 4;

export function clampBoardZoom(zoom) {
  return Math.min(BOARD_ZOOM_MAX, Math.max(BOARD_ZOOM_MIN, zoom));
}

export const BOARD_COLORS = Object.freeze(['#1e1e1e', '#e03131', '#2f9e44', '#1971c2', '#f08c00', '#9c36b5']);
export const BOARD_FILLS = Object.freeze(['transparent', '#ffc9c9', '#b2f2bb', '#a5d8ff', '#ffec99', '#eebefa']);

// Text styles. fontFamily is Excalidraw's number, so a family survives the
// .excalidraw export; an element without one is in the board's own hand.
export const BOARD_FONTS = Object.freeze([
  { id: 1, label: 'Hand', css: "'Segoe Print','Comic Sans MS','Chalkboard SE',cursive" },
  { id: 2, label: 'Sans', css: "'Helvetica Neue',Helvetica,Arial,sans-serif" },
  { id: 3, label: 'Mono', css: "'Cascadia Code','SF Mono',Menlo,Consolas,monospace" },
]);
export const BOARD_FONT_SIZES = Object.freeze([12, 16, 20, 24, 32, 48, 64, 96]);
export const BOARD_TEXT_STYLE = Object.freeze({ fontSize: 20, fontFamily: 1, fontWeight: undefined, fontStyle: undefined });

export function boardFont(el) {
  return BOARD_FONTS.find((font) => font.id === el?.fontFamily) || BOARD_FONTS[0];
}

// textStyleOf reads an element's style, every key present so it can be compared
// and laid over another element, clearing what that one had.
export function textStyleOf(el) {
  return {
    fontSize: el?.fontSize || BOARD_TEXT_STYLE.fontSize,
    fontFamily: boardFont(el).id,
    fontWeight: el?.fontWeight === 'bold' ? 'bold' : undefined,
    fontStyle: el?.fontStyle === 'italic' ? 'italic' : undefined,
  };
}

export function sameTextStyle(a, b) {
  const [x, y] = [textStyleOf(a), textStyleOf(b)];
  return Object.keys(x).every((key) => x[key] === y[key]);
}

// boardFontSizes is what the size list offers: an agent's size not among them
// still shows as itself.
export function boardFontSizes(current) {
  return BOARD_FONT_SIZES.includes(current) ? BOARD_FONT_SIZES : [...BOARD_FONT_SIZES, current].sort((a, b) => a - b);
}

const BOARD_GRID = 20; // board units between the paper's dots
const BOARD_GRID_MIN_PX = 12; // closer than this they turn into a texture

// What quick connect can make. Search reads the words as well as the label, and
// digits pick a row, so no name here may hold a digit.
export const QUICK_SHAPES = Object.freeze([
  { id: 'rectangle', label: 'Rectangle', words: 'box square process', digit: '1' },
  { id: 'ellipse', label: 'Ellipse', words: 'circle oval round', digit: '2' },
  { id: 'diamond', label: 'Diamond', words: 'rhombus decision', digit: '3' },
]);

// quickShapes is the shapes a search leaves, each keeping its digit.
export function quickShapes(query = '') {
  const wanted = query.trim().toLowerCase();
  return QUICK_SHAPES.filter(({ label, words }) => `${label} ${words}`.toLowerCase().includes(wanted));
}

const SHAPES = new Set(['rectangle', 'ellipse', 'diamond']);
const LINEAR = new Set(['arrow', 'line', 'freedraw']);
const WHEEL_LINE_PX = 33; // so a notch moves as far in every browser
const WHEEL_PAGE_PX = 600;
const BOARD_ID = /^[a-z0-9][a-z0-9-]{0,63}$/;

// A board open as a tab: the terminal shell's second pane kind. Its seed names
// the board instead of a socket, and its key is derived from the id, so a board
// has one tab however it was opened.
export function boardPaneKey(id) {
  return `board:${id}`;
}

export function boardPaneSeed(id, label = '') {
  return { board: id, key: boardPaneKey(id), label: label || id };
}

export function isBoardId(value) {
  return typeof value === 'string' && BOARD_ID.test(value);
}

export function boardIdFromTitle(title) {
  const id = String(title || '').toLowerCase().normalize('NFKD').replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 64);
  return isBoardId(id) ? id : '';
}

// wheelIsMouse tells a mouse wheel from a touchpad's two-finger scroll, which
// browsers report as the same event: a mouse moves in lines or in notches of
// 120, and only vertically, and a touchpad in anything else. deltaMode comes
// first because Firefox answers in lines only if it is read before the deltas.
// It is a guess. A free-spinning or smoothed wheel reads as a touchpad and pans,
// and one touchpad event can look like a notch, which is why onWheel holds a
// scroll to its first answer.
export function wheelIsMouse({ deltaMode, deltaX, wheelDeltaY }) {
  if (deltaMode === 1 || deltaMode === 2) return !deltaX;
  return !deltaX && !!wheelDeltaY && wheelDeltaY % 120 === 0;
}

const wheelUnit = (deltaMode) => (deltaMode === 1 ? WHEEL_LINE_PX : deltaMode === 2 ? WHEEL_PAGE_PX : 1);

// wheelPixels is how far a wheel event moved vertically, whatever its unit.
export function wheelPixels({ deltaMode, deltaY }) {
  return deltaY * wheelUnit(deltaMode);
}

// wheelPan is how far a wheel event pans in pixels. Shift turns a wheel sideways;
// Chrome has swapped the axes by now, Firefox only does so for its own scrolling.
export function wheelPan({ deltaMode, deltaX, deltaY, shiftKey }) {
  const unit = wheelUnit(deltaMode);
  if (shiftKey && !deltaX) return { x: deltaY * unit, y: 0 };
  return { x: deltaX * unit, y: deltaY * unit };
}

// groupBoards sorts a board listing into its groups, those that match `query`:
// the groups in the order of their newest board, and the boards without one
// last. Boards come newest first.
export function groupBoards(boards, query = '') {
  const wanted = query.trim().toLowerCase();
  const groups = new Map();
  for (const board of boards) {
    if (wanted && !`${board.title || ''} ${board.id} ${board.group || ''}`.toLowerCase().includes(wanted)) continue;
    const name = board.group || '';
    if (!groups.has(name)) groups.set(name, []);
    groups.get(name).push(board);
  }
  const rows = [...groups].map(([name, members]) => ({ name, boards: members }));
  return [...rows.filter(({ name }) => name), ...rows.filter(({ name }) => !name)];
}

// boardAge says how long ago a board changed, as briefly as a row allows.
export function boardAge(iso, now = Date.now()) {
  const at = Date.parse(iso || '');
  if (!Number.isFinite(at)) return '';
  const minutes = Math.max(0, Math.floor((now - at) / 60000));
  if (minutes < 1) return 'now';
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h`;
  const days = Math.floor(hours / 24);
  return days < 14 ? `${days}d` : `${Math.floor(days / 7)}w`;
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

// Stacking: elements draw in the order the board stores them. One sent to the
// front or back carries a `z`, above or below all those without one, so nothing
// else is rewritten and every client sorts alike. Attached text stands with its
// container. boardTopZ says what an editor's new element needs to land on top; an
// agent's goes among those without a `z`, under anything sent to the front.
function stacked(map) {
  const all = [...map.values()];
  if (!all.some((el) => Number.isFinite(el.z))) return all;
  const key = (el) => el.z ?? map.get(el.containerId)?.z ?? 0;
  return all.sort((a, b) => key(a) - key(b));
}

// boardTopZ is the highest `z` on the board, 0 when nothing is above the rest.
export function boardTopZ(map) {
  return Math.max(0, ...[...map.values()].map((el) => (Number.isFinite(el.z) ? el.z : 0)));
}

// liveElements lists the elements in stacking order, bottom first.
export function liveElements(map) {
  return stacked(map).filter((el) => !el.isDeleted);
}

// stackOrder sends elements (with their labels) to the front or back, keeping
// their order among themselves, and returns the bumped elements.
export function stackOrder(map, ids, where, opts) {
  const moving = new Set(withLabels(map, ids));
  const low = Math.min(0, ...[...map.values()].map((el) => (Number.isFinite(el.z) ? el.z : 0)));
  const start = where === 'front' ? boardTopZ(map) + 1 : low - moving.size;
  return stacked(map).filter((el) => moving.has(el.id) && !el.isDeleted).map((el, i) => bump(el, { z: start + i }, opts));
}

// normalizeBox turns a drag from (x0,y0) to (x1,y1) into a positive box.
export function normalizeBox(x0, y0, x1, y1) {
  return { x: Math.min(x0, x1), y: Math.min(y0, y1), width: Math.abs(x1 - x0), height: Math.abs(y1 - y0) };
}

// boardGrid places the paper's grid in a view: the screen distance between dots
// and where the dot at the board's origin lands. Zoomed out, every second dot
// goes, as often as it takes to keep them apart; the rest stay on the board's
// multiples of BOARD_GRID, so the grid never shifts under the drawing.
export function boardGrid({ x, y, zoom }) {
  let size = BOARD_GRID * zoom;
  while (size < BOARD_GRID_MIN_PX) size *= 2;
  return { size, x: -x * zoom, y: -y * zoom };
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
  if (el.type === 'diamond') {
    const cx = b.x + b.width / 2; const cy = b.y + b.height / 2;
    const corners = [[cx, b.y], [b.x + b.width, cy], [cx, b.y + b.height], [b.x, cy]];
    return corners.some(([ax, ay], i) => distToSegment(px, py, ax, ay, ...corners[(i + 1) % 4]) <= tol);
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

// ---- connectors (tcl-n023fi, tcl-umx796) ----
//
// A line's `points` always hold the path it draws, so a client that knows
// nothing of routing still draws it, and hit tests need no special case.
// `routing` says how to work that path out again when an end moves. Bindings
// are the truth for an attached end: the editor draws from them (drawnElements),
// which puts a line back on its shape whoever moved the shape and however two
// edits merged. A binding to a deleted element is read as a free end, so
// undoing that delete, anywhere, attaches the line again.
//
// pkg/claude/agent/board_routing.go keeps these paths for the CLI, checked
// against the same cases in testdata/board_routes.json.

export const LINE_ROUTES = Object.freeze([
  { id: 'straight', label: 'Straight line', icon: '╱' },
  { id: 'elbow', label: 'Elbow line', icon: '└' },
  { id: 'curved', label: 'Curved line', icon: '∿' },
]);

// The middle of each side, which lies on the outline of all three shapes, as
// Excalidraw fixedPoints: fractions of the element's box.
export const SNAP_POINTS = Object.freeze([[0.5, 0], [1, 0.5], [0.5, 1], [0, 0.5]]);

const ROUTED = new Set(['arrow', 'line']);
const END_GAP = 4; // keeps an arrowhead readable against the outline
const ELBOW_STUB = 24; // how far a path leaves a shape before it turns
const CURVE_STEPS = 24;

const round = (v) => Math.round(v * 100) / 100;

function liveTarget(map, binding) {
  const el = binding && map.get(binding.elementId);
  return el && !el.isDeleted ? el : null;
}

export function sideOf([fx, fy]) {
  const dx = fx - 0.5; const dy = fy - 0.5;
  return Math.abs(dx) >= Math.abs(dy) ? [dx < 0 ? -1 : 1, 0] : [0, dy < 0 ? -1 : 1];
}

function snapAt(el, fixedPoint) {
  const b = elementBounds(el);
  const dir = sideOf(fixedPoint);
  return { x: b.x + b.width * fixedPoint[0] + dir[0] * END_GAP, y: b.y + b.height * fixedPoint[1] + dir[1] * END_GAP, dir };
}

// facingSnap is the snap point on the side of `el` that faces (tx,ty).
function facingSnap(el, tx, ty) {
  const b = elementBounds(el);
  const dx = (tx - (b.x + b.width / 2)) / Math.max(b.width, 1);
  const dy = (ty - (b.y + b.height / 2)) / Math.max(b.height, 1);
  if (Math.abs(dx) >= Math.abs(dy)) return dx < 0 ? [0, 0.5] : [1, 0.5];
  return dy < 0 ? [0.5, 0] : [0.5, 1];
}

// snapPoints lists a shape's snap points in board units, for drawing them.
export function snapPoints(el) {
  if (!SHAPES.has(el.type)) return [];
  const b = elementBounds(el);
  return SNAP_POINTS.map((fixedPoint) => ({ fixedPoint, x: b.x + b.width * fixedPoint[0], y: b.y + b.height * fixedPoint[1] }));
}

// shapeNear is the topmost shape within `margin` of the point: whose snap
// points to show.
export function shapeNear(elements, px, py, margin, ignoreId = '') {
  for (let i = elements.length - 1; i >= 0; i--) {
    const el = elements[i];
    if (el.isDeleted || el.id === ignoreId || !SHAPES.has(el.type)) continue;
    const b = elementBounds(el);
    if (px >= b.x - margin && px <= b.x + b.width + margin && py >= b.y - margin && py <= b.y + b.height + margin) return el;
  }
  return null;
}

// snapHit is the binding for the snap point within `tol` of the point, or null.
export function snapHit(elements, px, py, tol, ignoreId = '') {
  for (let i = elements.length - 1; i >= 0; i--) {
    if (elements[i].isDeleted || elements[i].id === ignoreId) continue;
    for (const { fixedPoint, x, y } of snapPoints(elements[i])) {
      if (Math.hypot(px - x, py - y) <= tol) return { elementId: elements[i].id, fixedPoint: [...fixedPoint] };
    }
  }
  return null;
}

// bindingAt is what an end dropped on the point attaches to: the shape under it
// or within `margin`, at the snap point nearest the end. With `border` it takes
// no snap point and meets the shape wherever the line reaches it. Text has no
// snap points, so it always attaches that way.
export function bindingAt(elements, px, py, margin, ignoreId = '', border = false) {
  const under = shapeAt(elements, px, py, ignoreId);
  const shape = shapeNear(elements, px, py, 0, ignoreId) || under || shapeNear(elements, px, py, margin, ignoreId);
  if (!shape) return null;
  if (border || !SHAPES.has(shape.type)) return { elementId: shape.id };
  const nearest = (a, b) => (Math.hypot(px - a.x, py - a.y) <= Math.hypot(px - b.x, py - b.y) ? a : b);
  return { elementId: shape.id, fixedPoint: [...snapPoints(shape).reduce(nearest).fixedPoint] };
}

function facing(from, to) {
  const dx = to.x - from.x; const dy = to.y - from.y;
  return Math.abs(dx) >= Math.abs(dy) ? [dx < 0 ? -1 : 1, 0] : [0, dy < 0 ? -1 : 1];
}

// dropBends leaves out repeated points and points in the middle of a run.
function dropBends(path) {
  const out = [];
  for (const point of path) {
    if (out.length && out[out.length - 1][0] === point[0] && out[out.length - 1][1] === point[1]) continue;
    while (out.length >= 2) {
      const [ax, ay] = out[out.length - 2]; const [bx, by] = out[out.length - 1];
      if ((bx - ax) * (point[1] - ay) !== (by - ay) * (point[0] - ax)) break;
      out.pop();
    }
    out.push(point);
  }
  return out;
}

// elbowPath leaves each end along its side, then joins the two stubs with
// horizontal and vertical runs. It does not steer around other shapes.
function elbowPath(start, end) {
  const stub = Math.min(ELBOW_STUB, Math.hypot(end.x - start.x, end.y - start.y) / 3);
  const a = [start.x + start.dir[0] * stub, start.y + start.dir[1] * stub];
  const b = [end.x + end.dir[0] * stub, end.y + end.dir[1] * stub];
  const along = start.dir[0] !== 0 ? 0 : 1; // the axis the path leaves on
  const across = 1 - along;
  const at = (main, side) => (along === 0 ? [main, side] : [side, main]);
  let bends;
  if ((end.dir[0] !== 0) === (start.dir[0] !== 0)) {
    const sd = start.dir[along]; const ed = end.dir[along];
    if (sd === ed) {
      const turn = sd > 0 ? Math.max(a[along], b[along]) : Math.min(a[along], b[along]);
      bends = [at(turn, a[across]), at(turn, b[across])];
    } else if ((b[along] - a[along]) * sd >= 0) {
      const mid = (a[along] + b[along]) / 2;
      bends = [at(mid, a[across]), at(mid, b[across])];
    } else {
      const mid = (a[across] + b[across]) / 2;
      bends = [at(a[along], mid), at(b[along], mid)];
    }
  } else {
    const corner = at(b[along], a[across]);
    const ahead = (corner[along] - a[along]) * start.dir[along] >= 0 && (corner[across] - b[across]) * end.dir[across] >= 0;
    bends = [ahead ? corner : at(a[along], b[across])];
  }
  return dropBends([[start.x, start.y], a, ...bends, b, [end.x, end.y]]);
}

// curvedPath is a cubic leaving each end along its side, sampled into points.
function curvedPath(start, end) {
  const reach = Math.max(30, Math.hypot(end.x - start.x, end.y - start.y) * 0.4);
  const c1 = [start.x + start.dir[0] * reach, start.y + start.dir[1] * reach];
  const c2 = [end.x + end.dir[0] * reach, end.y + end.dir[1] * reach];
  const path = [];
  for (let i = 0; i <= CURVE_STEPS; i++) {
    const t = i / CURVE_STEPS; const u = 1 - t;
    const mix = (s, p1, p2, e) => u * u * u * s + 3 * u * u * t * p1 + 3 * u * t * t * p2 + t * t * t * e;
    path.push([mix(start.x, c1[0], c2[0], end.x), mix(start.y, c1[1], c2[1], end.y)]);
  }
  return path;
}

// routePath is the path between two ends. An end's `dir` is the way it leaves
// its shape; a free end has none and faces the other end.
export function routePath(routing, start, end) {
  if (routing !== 'elbow' && routing !== 'curved') return [[start.x, start.y], [end.x, end.y]];
  const s = { ...start, dir: start.dir || facing(start, end) };
  const e = { ...end, dir: end.dir || facing(end, start) };
  return routing === 'elbow' ? elbowPath(s, e) : curvedPath(s, e);
}

function pathGeometry(route) {
  const path = route.length > 1 ? route : [route[0], route[0]]; // ends that meet
  const [ox, oy] = path[0];
  const points = path.map(([px, py]) => [round(px - ox), round(py - oy)]);
  const b = elementBounds({ x: 0, y: 0, points });
  return { x: round(ox), y: round(oy), width: b.width, height: b.height, points };
}

// endOf places one end of a line. `toward` is where the other end points it.
function endOf(binding, target, toward, routed) {
  if (binding.fixedPoint) return snapAt(target, binding.fixedPoint);
  if (routed) return snapAt(target, facingSnap(target, toward.x, toward.y));
  return edgePoint(target, toward.x, toward.y);
}

// routeConnector is where a line belongs now, or null when it stays as stored:
// a free line keeps its path, and so does a bound one with hand-placed bends
// and no routing (`board from-process` draws rework that way). `force` works
// the path out regardless, for a line whose ends or routing just changed.
export function routeConnector(el, map, force = false) {
  const from = liveTarget(map, el.startBinding);
  const to = liveTarget(map, el.endBinding);
  if (el.type === 'freedraw') {
    if (!from || !el.startBinding.fixedPoint) return null;
    const at = snapAt(from, el.startBinding.fixedPoint);
    return { x: round(at.x - el.points[0][0]), y: round(at.y - el.points[0][1]) };
  }
  if (!ROUTED.has(el.type) || !el.points?.length) return null;
  const routed = el.routing === 'elbow' || el.routing === 'curved';
  // Still being drawn: a free line of two points has no route stored yet.
  const unrouted = routed && el.points.length === 2;
  if (!force && !unrouted && (!(from || to) || (!el.routing && el.points.length > 2))) return null;
  const first = el.points[0]; const last = el.points[el.points.length - 1];
  const a = { x: el.x + first[0], y: el.y + first[1] };
  const b = { x: el.x + last[0], y: el.y + last[1] };
  const aim = (binding, target, free) => {
    if (!target) return free;
    return binding.fixedPoint ? snapAt(target, binding.fixedPoint) : center(target);
  };
  const start = from ? endOf(el.startBinding, from, aim(el.endBinding, to, b), routed) : a;
  const end = to ? endOf(el.endBinding, to, aim(el.startBinding, from, a), routed) : b;
  return pathGeometry(routePath(el.routing, start, end));
}

// reboundArrows recomputes every line bound to one of `movedIds`, returning the
// bumped lines to save.
export function reboundArrows(map, movedIds, opts) {
  const moved = new Set(movedIds);
  const out = [];
  for (const el of map.values()) {
    if (el.isDeleted || !LINEAR.has(el.type)) continue;
    const fromId = el.startBinding?.elementId;
    const toId = el.endBinding?.elementId;
    if (!moved.has(fromId) && !moved.has(toId)) continue;
    if (!liveTarget(map, el.startBinding) && !liveTarget(map, el.endBinding)) continue;
    const route = routeConnector(el, map, true);
    if (route) out.push(bump(el, route, opts));
  }
  return out;
}

// detachedLines frees the ends of lines bound to elements being deleted, each
// where it is drawn now. Saved in one step with the deletion, undo reattaches.
export function detachedLines(map, gone, opts) {
  const out = [];
  for (const el of map.values()) {
    if (el.isDeleted || gone.has(el.id) || !LINEAR.has(el.type)) continue;
    const start = gone.has(el.startBinding?.elementId);
    const end = gone.has(el.endBinding?.elementId);
    if (!start && !end) continue;
    const changes = { ...routeConnector(el, map) };
    if (start) changes.startBinding = undefined;
    if (end) changes.endBinding = undefined;
    out.push(bump(el, changes, opts));
  }
  return out;
}

// drawnElements is the live elements as they are to be drawn: attached ends
// sit on their shapes, whatever the stored copy of the line says.
export function drawnElements(map) {
  const drawn = new Map();
  for (const el of map.values()) {
    if (el.isDeleted) continue;
    const route = routeConnector(el, map);
    drawn.set(el.id, route ? { ...el, ...route } : el);
  }
  for (const el of drawn.values()) {
    const container = el.containerId && drawn.get(el.containerId);
    if (container && container.type !== 'text') drawn.set(el.id, { ...el, ...textBoxOn(el, container) });
  }
  return liveElements(map).map((el) => drawn.get(el.id));
}

// labelPoint is where an element's attached text sits: the middle of a box,
// or halfway along a line's path.
export function labelPoint(el) {
  if (!el.points?.length) return center(el);
  const lengths = [];
  let total = 0;
  for (let i = 1; i < el.points.length; i++) {
    const [ax, ay] = el.points[i - 1]; const [bx, by] = el.points[i];
    lengths.push(Math.hypot(bx - ax, by - ay));
    total += lengths[lengths.length - 1];
  }
  let left = total / 2;
  for (let i = 0; i < lengths.length; i++) {
    if (left <= lengths[i] && lengths[i] > 0) {
      const [ax, ay] = el.points[i]; const [bx, by] = el.points[i + 1];
      const t = left / lengths[i];
      return { x: el.x + ax + (bx - ax) * t, y: el.y + ay + (by - ay) * t };
    }
    left -= lengths[i];
  }
  return { x: el.x + el.points[0][0], y: el.y + el.points[0][1] };
}

function textBoxOn(text, container) {
  const at = labelPoint(container);
  const b = elementBounds(text);
  return { x: round(at.x - b.width / 2), y: round(at.y - b.height / 2) };
}

// attachedText puts attached text back on its container, for any text or
// container in `ids`, returning the bumped text elements that moved.
export function attachedText(map, ids, opts) {
  const touched = new Set(ids);
  const out = [];
  for (const el of map.values()) {
    if (el.isDeleted || !el.containerId || !(touched.has(el.id) || touched.has(el.containerId))) continue;
    const container = map.get(el.containerId);
    if (!container || container.isDeleted || container.type === 'text') continue;
    const route = routeConnector(container, map);
    const box = textBoxOn(el, route ? { ...container, ...route } : container);
    if (box.x !== el.x || box.y !== el.y) out.push(bump(el, box, opts));
  }
  return out;
}

const NEXT_GAP = 80;

// snapPosition is where a snap point lies on the board.
export function snapPosition(el, fixedPoint) {
  const b = elementBounds(el);
  return { x: b.x + b.width * fixedPoint[0], y: b.y + b.height * fixedPoint[1] };
}

// suggestedShape is the next shape a snap point offers: a blank copy of `from`,
// a step out from that side, or centred on the pointer once it moves. Given a
// direction it is what a dragged connector was aiming for instead: its near side
// on `at`, so the connector ends where it was dropped.
export function suggestedShape(from, fixedPoint, at = null, { kind = from.type, dir = null } = {}) {
  const b = elementBounds(from);
  const [dx, dy] = sideOf(fixedPoint);
  let x = at ? at.x - b.width / 2 : b.x + dx * (b.width + NEXT_GAP);
  let y = at ? at.y - b.height / 2 : b.y + dy * (b.height + NEXT_GAP);
  if (at && dir?.[0]) x = dir[0] > 0 ? at.x : at.x - b.width;
  if (at && dir?.[1]) y = dir[1] > 0 ? at.y : at.y - b.height;
  return { type: kind, x, y, width: b.width, height: b.height, strokeColor: from.strokeColor, backgroundColor: from.backgroundColor };
}

// dropDirection is the way a connector dragged from a snap point to `at` runs.
export function dropDirection(from, fixedPoint, at) {
  return facing(snapPosition(from, fixedPoint), at);
}

// entryPoint is the snap point of `shape` that a connector arrives at from `start`.
export function entryPoint(shape, start) {
  const [dx, dy] = facing(start, center(shape));
  return [0.5 - dx / 2, 0.5 - dy / 2];
}

function overlaps(a, b) {
  return a.x < b.x + b.width && b.x < a.x + a.width && a.y < b.y + b.height && b.y < a.y + a.height;
}

// nextShapeSpot is where a shape added from the keyboard goes: a step out of the
// snap point, or beside that spot when something is there, so branching from one
// shape does not stack its branches. Returns the drop point and direction.
export function nextShapeSpot(from, fixedPoint, elements) {
  const b = elementBounds(from);
  const dir = sideOf(fixedPoint);
  const start = snapPosition(from, fixedPoint);
  const across = (dir[0] ? b.height : b.width) + NEXT_GAP / 2;
  const base = { x: start.x + dir[0] * NEXT_GAP, y: start.y + dir[1] * NEXT_GAP };
  const taken = elements.filter((el) => !el.isDeleted && SHAPES.has(el.type)).map(elementBounds);
  for (const step of [0, 1, -1, 2, -2, 3, -3, 4, -4]) {
    const at = { x: base.x + (dir[1] ? step * across : 0), y: base.y + (dir[0] ? step * across : 0) };
    const box = suggestedShape(from, fixedPoint, at, { dir });
    if (!taken.some((other) => overlaps(other, box))) return { at, dir };
  }
  return { at: base, dir };
}

// shapeAt finds the shape (not a line) under a point, for arrow binding.
export function shapeAt(elements, px, py, ignoreId = '') {
  for (let i = elements.length - 1; i >= 0; i--) {
    const el = elements[i];
    if (el.isDeleted || el.id === ignoreId || el.containerId || (!SHAPES.has(el.type) && el.type !== 'text')) continue;
    const b = elementBounds(el);
    if (px >= b.x && px <= b.x + b.width && py >= b.y && py <= b.y + b.height) return el;
  }
  return null;
}

// shapeInside is the topmost shape whose outline encloses the point, filled or
// not, which is where a drag or a double-click takes the shape. Ellipses and
// diamonds leave out the corners of their box, so what lies there stays reachable.
export function shapeInside(elements, px, py) {
  for (let i = elements.length - 1; i >= 0; i--) {
    const el = elements[i];
    if (el.isDeleted || !SHAPES.has(el.type)) continue;
    const b = elementBounds(el);
    const dx = (px - (b.x + b.width / 2)) / Math.max(b.width / 2, 1e-6);
    const dy = (py - (b.y + b.height / 2)) / Math.max(b.height / 2, 1e-6);
    const enclosed = el.type === 'ellipse' ? dx * dx + dy * dy <= 1
      : el.type === 'diamond' ? Math.abs(dx) + Math.abs(dy) <= 1
        : Math.abs(dx) <= 1 && Math.abs(dy) <= 1;
    if (enclosed) return el;
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

// ---- editing several elements (tcl-s1c73w) ----

function inside(point, box) {
  return point.x >= box.x && point.x <= box.x + box.width && point.y >= box.y && point.y <= box.y + box.height;
}

// labelOwners maps each live text element to what it labels: the container it
// names (tcl-jzs3g1), or, for text from before containers, the `<id>` of its
// `<id>-label` id, or else the innermost shape its centre sits in.
export function labelOwners(map) {
  const shapes = liveElements(map).filter((el) => SHAPES.has(el.type));
  const out = new Map();
  const canOwn = (id) => { const el = map.get(id); return !!el && !el.isDeleted && el.type !== 'text'; };
  for (const t of map.values()) {
    if (t.isDeleted || t.type !== 'text') continue;
    const named = t.containerId || (t.id.endsWith('-label') ? t.id.slice(0, -6) : '');
    if (named && canOwn(named)) { out.set(t.id, named); continue; }
    const c = center(t);
    let best = null; let area = Infinity;
    for (const s of shapes) {
      const b = elementBounds(s);
      if (inside(c, b) && b.width * b.height < area) { best = s; area = b.width * b.height; }
    }
    if (best) out.set(t.id, best.id);
  }
  return out;
}

// withLabels adds the labels of any shapes in `ids`, so a shape and its
// label move, copy and delete together.
export function withLabels(map, ids) {
  const set = new Set(ids);
  for (const [textId, ownerId] of labelOwners(map)) if (set.has(ownerId)) set.add(textId);
  return [...set];
}

// labelOf returns the shape's label element, or null.
export function labelOf(map, shapeId) {
  const own = map.get(`${shapeId}-label`);
  if (own && !own.isDeleted && own.type === 'text') return own;
  for (const [textId, ownerId] of labelOwners(map)) if (ownerId === shapeId) return map.get(textId);
  return null;
}

// newLabel builds text attached to an element, under the CLI's `<id>-label` id.
export function newLabel(shape, text, props = {}, opts) {
  const lines = String(text).split('\n');
  const size = props.fontSize || 20;
  const width = Math.ceil(Math.max(...lines.map((l) => l.length), 1) * size * 0.6);
  const height = Math.ceil(lines.length * size * 1.25);
  const c = labelPoint(shape);
  const id = `${shape.id}-label`.length <= 64 ? `${shape.id}-label` : undefined;
  return newElement('text', {
    ...(id ? { id } : {}), containerId: shape.id, text, fontSize: size, textAlign: 'center', strokeColor: shape.strokeColor || '#1e1e1e',
    x: c.x - width / 2, y: c.y - height / 2, width, height, ...props,
  }, opts);
}

// elementsInBox returns the live elements lying wholly inside a box (a
// rubber-band selection).
export function elementsInBox(elements, box) {
  return elements.filter((el) => {
    if (el.isDeleted) return false;
    const b = elementBounds(el);
    return b.x >= box.x && b.y >= box.y && b.x + b.width <= box.x + box.width && b.y + b.height <= box.y + box.height;
  });
}

export const ALIGNMENTS = Object.freeze([
  { id: 'left', label: 'Align left', icon: '⇤' },
  { id: 'center', label: 'Align centres horizontally', icon: '↔' },
  { id: 'right', label: 'Align right', icon: '⇥' },
  { id: 'top', label: 'Align top', icon: '⤒' },
  { id: 'middle', label: 'Align centres vertically', icon: '↕' },
  { id: 'bottom', label: 'Align bottom', icon: '⤓' },
  { id: 'distribute-x', label: 'Space evenly horizontally', icon: '⋯', min: 3 },
  { id: 'distribute-y', label: 'Space evenly vertically', icon: '⋮', min: 3 },
]);

// alignMoves returns Map(id -> {dx, dy}) that aligns or spaces the elements
// (the shapes being arranged; their labels follow separately).
export function alignMoves(elements, how) {
  const boxes = elements.map((el) => ({ id: el.id, b: elementBounds(el) }));
  const moves = new Map();
  if (boxes.length < 2) return moves;
  const minX = Math.min(...boxes.map(({ b }) => b.x));
  const maxX = Math.max(...boxes.map(({ b }) => b.x + b.width));
  const minY = Math.min(...boxes.map(({ b }) => b.y));
  const maxY = Math.max(...boxes.map(({ b }) => b.y + b.height));
  const set = (id, dx, dy) => { if (dx || dy) moves.set(id, { dx, dy }); };
  if (how === 'distribute-x' || how === 'distribute-y') {
    const horizontal = how === 'distribute-x';
    const sorted = [...boxes].sort((a, b) => (horizontal ? a.b.x - b.b.x : a.b.y - b.b.y));
    const total = sorted.reduce((sum, { b }) => sum + (horizontal ? b.width : b.height), 0);
    const gap = ((horizontal ? maxX - minX : maxY - minY) - total) / (sorted.length - 1);
    let at = horizontal ? minX : minY;
    for (const { id, b } of sorted) {
      if (horizontal) { set(id, at - b.x, 0); at += b.width + gap; } else { set(id, 0, at - b.y); at += b.height + gap; }
    }
    return moves;
  }
  for (const { id, b } of boxes) {
    if (how === 'left') set(id, minX - b.x, 0);
    else if (how === 'right') set(id, maxX - (b.x + b.width), 0);
    else if (how === 'center') set(id, (minX + maxX) / 2 - (b.x + b.width / 2), 0);
    else if (how === 'top') set(id, 0, minY - b.y);
    else if (how === 'bottom') set(id, 0, maxY - (b.y + b.height));
    else if (how === 'middle') set(id, 0, (minY + maxY) / 2 - (b.y + b.height / 2));
  }
  return moves;
}

export const CLIPBOARD_TYPE = 'excalidraw/clipboard';

// clipboardText serialises elements the way Excalidraw puts them on the
// clipboard, so a copy pastes into excalidraw.com and back.
export function clipboardText(elements) {
  return JSON.stringify({ type: CLIPBOARD_TYPE, elements: elements.map(({ server_version: _s, ...el }) => el) });
}

const PASTE_TYPES = new Set(['rectangle', 'ellipse', 'diamond', 'arrow', 'line', 'freedraw', 'text']);
const MAX_COORD = 1e6;
const MAX_FILE_ELEMENTS = 5000;
const MAX_TEXT_BYTES = 4000;
const COLOR = /^(#[0-9A-Fa-f]{6}|#[0-9A-Fa-f]{3}|transparent)$/;
const ELEMENT_ID = /^[A-Za-z0-9_-]{1,64}$/;

// cleanText makes text acceptable to the daemon: printable characters,
// newlines and tabs only, at most 4000 bytes.
export function cleanText(text, maxBytes = MAX_TEXT_BYTES) {
  let out = String(text ?? '').replace(/\r\n?/g, '\n')
    .replace(/[\p{C}\p{Z}]/gu, (ch) => (ch === '\n' || ch === '\t' || ch === ' ' ? ch : /\p{Z}/u.test(ch) ? ' ' : ''));
  const encoder = new TextEncoder();
  if (encoder.encode(out).length <= maxBytes) return out;
  let bytes = 0; let cut = '';
  for (const ch of out) {
    bytes += encoder.encode(ch).length;
    if (bytes > maxBytes) break;
    cut += ch;
  }
  return cut;
}

const clampNum = (v, lo, hi, fallback) => (Number.isFinite(v) ? Math.min(hi, Math.max(lo, v)) : fallback);

// sanitizeElement keeps only the fields the daemon stores, each within its
// rules, so a pasted element from anywhere (excalidraw.com included) saves.
// Null when there is nothing usable.
export function sanitizeElement(raw) {
  if (!raw || typeof raw !== 'object' || raw.isDeleted || !PASTE_TYPES.has(raw.type)) return null;
  if (!Number.isFinite(raw.x) || !Number.isFinite(raw.y)) return null;
  const el = {
    id: typeof raw.id === 'string' && ELEMENT_ID.test(raw.id) ? raw.id : newElementId(),
    type: raw.type,
    x: clampNum(raw.x, -MAX_COORD, MAX_COORD, 0), y: clampNum(raw.y, -MAX_COORD, MAX_COORD, 0),
    width: clampNum(raw.width, 0, MAX_COORD, 0), height: clampNum(raw.height, 0, MAX_COORD, 0),
    angle: clampNum(raw.angle, -MAX_COORD, MAX_COORD, 0),
    strokeColor: COLOR.test(raw.strokeColor || '') ? raw.strokeColor : '#1e1e1e',
    backgroundColor: COLOR.test(raw.backgroundColor || '') ? raw.backgroundColor : 'transparent',
    fillStyle: ['solid', 'hachure', 'none'].includes(raw.fillStyle) ? raw.fillStyle : 'solid',
    strokeStyle: ['solid', 'dashed', 'dotted'].includes(raw.strokeStyle) ? raw.strokeStyle : 'solid',
    strokeWidth: clampNum(raw.strokeWidth, 0.5, 20, 2),
    roughness: clampNum(raw.roughness, 0, 2, 1),
    opacity: clampNum(raw.opacity, 0, 100, 100),
  };
  if (LINEAR.has(raw.type)) {
    const points = Array.isArray(raw.points) ? raw.points.slice(0, 5000) : [];
    if (points.length < 2 || !points.every((p) => Array.isArray(p) && p.length === 2 && p.every((v) => Number.isFinite(v) && Math.abs(v) <= MAX_COORD))) return null;
    el.points = points.map(([px, py]) => [px, py]);
  }
  if (raw.type === 'text') {
    const text = cleanText(raw.text);
    if (!text.trim()) return null;
    el.text = text;
    el.fontSize = clampNum(raw.fontSize, 4, 200, 20);
    el.textAlign = ['left', 'center', 'right'].includes(raw.textAlign) ? raw.textAlign : 'left';
    const { fontFamily, fontWeight, fontStyle } = textStyleOf(raw);
    if (fontFamily !== BOARD_TEXT_STYLE.fontFamily) el.fontFamily = fontFamily;
    if (fontWeight) el.fontWeight = fontWeight;
    if (fontStyle) el.fontStyle = fontStyle;
    if (typeof raw.containerId === 'string' && ELEMENT_ID.test(raw.containerId)) el.containerId = raw.containerId;
  }
  if (LINEAR.has(raw.type)) {
    for (const key of ['startBinding', 'endBinding']) {
      const id = raw[key]?.elementId;
      if (typeof id !== 'string' || !ELEMENT_ID.test(id)) continue;
      el[key] = { elementId: id };
      const point = raw[key].fixedPoint;
      if (Array.isArray(point) && point.length === 2 && point.every(Number.isFinite)) el[key].fixedPoint = point.map((v) => clampNum(v, 0, 1, 0.5));
    }
  }
  if (ROUTED.has(raw.type) && LINE_ROUTES.some(({ id }) => id === raw.routing)) el.routing = raw.routing;
  return el;
}

// parseClipboard reads clipboard text written by clipboardText (or by
// Excalidraw) as sanitized elements; null when it is not board elements.
export function parseClipboard(text) {
  try {
    const data = JSON.parse(text);
    if (data?.type !== CLIPBOARD_TYPE || !Array.isArray(data.elements)) return null;
    const out = data.elements.slice(0, 5000).map(sanitizeElement).filter(Boolean);
    return out.length ? out : null;
  } catch {
    return null;
  }
}

// cloneElements copies elements as new version-1 elements moved by (dx, dy),
// with fresh ids. Arrows keep bindings to elements copied with them and drop
// the rest; a copied label keeps following its copied shape.
export function cloneElements(elements, dx, dy, opts = {}) {
  const ids = new Map(elements.map((el) => [el.id, newElementId(opts.random)]));
  for (const el of elements) {
    if (el.type === 'text' && /-label$/.test(el.id) && ids.has(el.id.slice(0, -6))) {
      const owner = `${ids.get(el.id.slice(0, -6))}-label`;
      if (owner.length <= 64) ids.set(el.id, owner);
    }
  }
  const rebind = (binding) => (binding && ids.has(binding.elementId) ? { ...binding, elementId: ids.get(binding.elementId) } : undefined);
  return elements.map((el) => {
    const { server_version: _s, customData: _c, startBinding, endBinding, containerId, z: _z, ...rest } = el;
    const copy = newElement(el.type, {
      ...rest, id: ids.get(el.id),
      x: clampNum(el.x + dx, -MAX_COORD, MAX_COORD, 0), y: clampNum(el.y + dy, -MAX_COORD, MAX_COORD, 0),
    }, opts);
    const start = rebind(startBinding); const end = rebind(endBinding);
    if (start) copy.startBinding = start;
    if (end) copy.endBinding = end;
    if (ids.has(containerId)) copy.containerId = ids.get(containerId);
    return copy;
  });
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

// operatorWrote lists the text elements the operator wrote in this batch. Only
// these are quoted: the summary reaches the board's agents as the operator's
// own words. An element's saved author cannot show this, because it names
// whoever touched it last, so an agent's text that the operator merely dragged
// reads as the operator's.
function operatorWrote(map, changedIds, before) {
  const wrote = new Set();
  for (const id of changedIds) {
    const el = map.get(id);
    const prev = before.get(id);
    if (el?.type !== 'text' || (el.customData?.author || 'operator') !== 'operator') continue;
    if (!prev || prev.isDeleted || prev.text !== el.text) wrote.add(id);
  }
  return wrote;
}

function label(el, map, wrote) {
  if (el.type === 'text') return wrote.has(el.id) ? `text “${String(el.text || '').slice(0, 40)}”` : 'text';
  // A shape's label is the text inside it, when there is one.
  if (SHAPES.has(el.type) && map) {
    const inner = [...map.values()].find((t) => !t.isDeleted && t.type === 'text' && (() => {
      const c = center(t); const b = elementBounds(el);
      return c.x >= b.x && c.x <= b.x + b.width && c.y >= b.y && c.y <= b.y + b.height;
    })());
    if (inner && wrote.has(inner.id)) return `${el.type} “${String(inner.text || '').slice(0, 40)}”`;
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
  const wrote = operatorWrote(map, new Set(changedIds), before);
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
    const named = list.filter((el) => el.type !== 'freedraw').map((el) => label(el, map, wrote));
    const bits = [...named.slice(0, 4)];
    if (named.length > 4) bits.push(`${named.length - 4} more`);
    if (strokes.length) {
      const near = nearestLabelled(map, center(strokes[strokes.length - 1]), ignore);
      bits.push(`${strokes.length} freehand stroke${strokes.length === 1 ? '' : 's'}${near ? ` near ${label(near, map, wrote)}` : ''}`);
    }
    parts.push(`${verb} ${bits.join(', ')}`);
  };
  group(added, 'drew');
  group(edited, 'changed');
  group(removed, 'removed');
  return parts.join('; ');
}

// excalidrawLinks adds what Excalidraw reads from the other side of a link: a
// container lists its text and lines in boundElements.
function excalidrawLinks(elements) {
  const bound = new Map();
  const link = (ownerId, id, type) => { if (ownerId) bound.set(ownerId, [...(bound.get(ownerId) || []), { id, type }]); };
  for (const el of elements) {
    link(el.containerId, el.id, 'text');
    if (el.type === 'arrow') { link(el.startBinding?.elementId, el.id, 'arrow'); link(el.endBinding?.elementId, el.id, 'arrow'); }
  }
  return elements.map((el) => {
    const links = bound.get(el.id);
    return { ...el, ...(links ? { boundElements: links } : {}), ...(el.containerId ? { verticalAlign: 'middle' } : {}) };
  });
}

// fromExcalidraw reads an .excalidraw file as elements for a new board, with
// fresh ids, and counts those a board cannot hold, such as images; null when
// the text is no such file.
export function fromExcalidraw(text) {
  let data = null;
  try { data = JSON.parse(text); } catch { return null; }
  if (data?.type !== 'excalidraw' || !Array.isArray(data.elements)) return null;
  const live = data.elements.filter((el) => el && !el.isDeleted);
  const ordered = stacked(new Map(live.map((el, i) => [el.id ?? `#${i}`, el])));
  const clean = ordered.slice(0, MAX_FILE_ELEMENTS).map(sanitizeElement).filter(Boolean);
  return { elements: cloneElements(clean, 0, 0), skipped: live.length - clean.length };
}

// toExcalidraw wraps elements as an .excalidraw file.
export function toExcalidraw(elements) {
  return JSON.stringify({ type: 'excalidraw', version: 2, source: 'tclaude', elements: excalidrawLinks(elements), appState: { viewBackgroundColor: '#ffffff' }, files: {} }, null, 2);
}
