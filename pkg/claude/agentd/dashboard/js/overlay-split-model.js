// overlay-split-model.js -- how overlay windows share the room (tcl-ppvf8g), so
// that two windows, 2+1 and four are all one thing: a tree of splits.
//
//   leaf    { id }                          one window
//   gap     { gap: true }                   a cell no window holds yet, which
//                                           stays a place to drop one
//   binary  { axis, at, a, b }              one area in two. axis 'x' puts a left
//                                           of b, 'y' puts a above b; at is the
//                                           line's share of the area, 0 to 1.
//   quad    { x, y, cells: [tl, tr, bl, br] }  one area in four around a shared
//                                           crossing. Each line runs the full
//                                           width or height, so it is not two
//                                           nested binaries.
//
// A line can be unlocked. It is no longer shared, so each side keeps its own
// edge and a space or an overlap can open between them: at (or a quad's x, y)
// is then where the first side ends, and free (freeX, freeY) where the second
// starts. The children stay in their places.
//
// Any child can be any node. A tree has at least one window, and a window that
// leaves or is away leaves its cell empty, so the rest never move. A path names
// a node by the child indexes that reach it. Everything here is DOM-free.

import { OVERLAY_MIN_H, OVERLAY_MIN_W } from './overlay-window-model.js';

// How far into a window, as a share of its size, a drop counts as its edge.
export const OVERLAY_SPLIT_BAND = 0.3;

const GAP = Object.freeze({ gap: true });

const isLeaf = (node) => typeof node?.id === 'string';
const isGap = (node) => node?.gap === true;
const isQuad = (node) => Array.isArray(node?.cells);
const childrenOf = (node) => (isLeaf(node) || isGap(node) ? [] : isQuad(node) ? node.cells : [node.a, node.b]);
const clampShare = (share) => Math.min(1, Math.max(0, share));

function withChild(node, index, child) {
  if (isQuad(node)) return { ...node, cells: node.cells.map((cell, i) => (i === index ? child : cell)) };
  return index === 0 ? { ...node, a: child } : { ...node, b: child };
}

// atPath rebuilds the tree with the node at path replaced by what change makes of it.
function atPath(node, path, change) {
  if (!path.length) return change(node);
  const [index, ...rest] = path;
  return withChild(node, index, atPath(childrenOf(node)[index], rest, change));
}

export function leavesOf(node) {
  if (!node) return [];
  return isLeaf(node) ? [node.id] : childrenOf(node).flatMap(leavesOf);
}

function pathTo(node, id, path = []) {
  if (isLeaf(node)) return node.id === id ? path : null;
  for (const [index, child] of childrenOf(node).entries()) {
    const found = pathTo(child, id, [...path, index]);
    if (found) return found;
  }
  return null;
}

export function nodeAt(node, path) {
  return path.reduce((current, index) => childrenOf(current)[index], node);
}

// minSize is the smallest area a node fits in with no window under its minimum.
export function minSize(node) {
  if (isLeaf(node) || isGap(node)) return { w: OVERLAY_MIN_W, h: OVERLAY_MIN_H };
  const sizes = childrenOf(node).map(minSize);
  if (isQuad(node)) {
    const [tl, tr, bl, br] = sizes;
    return { w: Math.max(tl.w, bl.w) + Math.max(tr.w, br.w), h: Math.max(tl.h, tr.h) + Math.max(bl.h, br.h) };
  }
  const [a, b] = sizes;
  return node.axis === 'x' ? { w: a.w + b.w, h: Math.max(a.h, b.h) } : { w: Math.max(a.w, b.w), h: a.h + b.h };
}

// clampLine keeps a line where the areas on both sides of it fit, or in the
// middle of an area too small for that.
function clampLine(share, size, before, after) {
  const low = before / size;
  const high = 1 - after / size;
  return low > high ? 0.5 : Math.min(high, Math.max(low, clampShare(share)));
}

// lineSpan is where a line leaves its two sides, in pixels from the area's start:
// where the first ends and where the second starts. A locked line is one place.
function lineSpan(share, free, size, before, after) {
  if (free === undefined) {
    const at = Math.round(size * clampLine(share, size, before, after));
    return [at, at];
  }
  return [Math.round(Math.min(size, Math.max(before, size * share))), Math.round(Math.max(0, Math.min(size - after, size * free)))];
}

function binaryAreas(node, area) {
  const [a, b] = [minSize(node.a), minSize(node.b)];
  if (node.axis === 'x') {
    const [end, start] = lineSpan(node.at, node.free, area.w, a.w, b.w);
    return { line: area.x + end, areas: [{ ...area, w: end }, { ...area, x: area.x + start, w: area.w - start }] };
  }
  const [end, start] = lineSpan(node.at, node.free, area.h, a.h, b.h);
  return { line: area.y + end, areas: [{ ...area, h: end }, { ...area, y: area.y + start, h: area.h - start }] };
}

function quadAreas(node, area) {
  const [tl, tr, bl, br] = node.cells.map(minSize);
  const [w, right] = lineSpan(node.x, node.freeX, area.w, Math.max(tl.w, bl.w), Math.max(tr.w, br.w));
  const [h, lower] = lineSpan(node.y, node.freeY, area.h, Math.max(tl.h, tr.h), Math.max(bl.h, br.h));
  const rightSide = { x: area.x + right, w: area.w - right };
  const lowerSide = { y: area.y + lower, h: area.h - lower };
  return {
    cross: { x: area.x + w, y: area.y + h },
    areas: [{ ...area, w, h }, { ...area, ...rightSide, h }, { ...area, w, ...lowerSide }, { ...area, ...rightSide, ...lowerSide }],
  };
}

// splitLayout lays a tree out over area. rects maps each window to its
// rectangle; lines are the shared split lines, each { path, area, axis, x, y, w,
// h } with axis as the node's; crossings are the quads', { path, area, x, y };
// gaps are the empty cells, { path, rect, held }, held naming a window that is
// away and keeps the cell. edges maps a window to the compass
// edges an unlocked line left it to resize, each { path, area, axis, side, partner },
// side 'end' or 'start' of its line and partner where the other side's edge stands.
export function splitLayout(tree, area) {
  const rects = {};
  const lines = [];
  const crossings = [];
  const gaps = [];
  const edges = {};
  (function walk(node, here, path, free) {
    if (isLeaf(node)) {
      rects[node.id] = here;
      edges[node.id] = free;
      return;
    }
    if (isGap(node)) {
      gaps.push({ path, rect: here, held: node.held });
      return;
    }
    const edge = (axis, side, unlocked, partner) => (unlocked ? { path, area: here, axis, side, partner } : undefined);
    if (isQuad(node)) {
      const { cross, areas } = quadAreas(node, here);
      const [openX, openY] = [node.freeX !== undefined, node.freeY !== undefined];
      if (!openX) lines.push({ path, area: here, axis: 'x', x: cross.x, y: here.y, w: 0, h: here.h });
      if (!openY) lines.push({ path, area: here, axis: 'y', x: here.x, y: cross.y, w: here.w, h: 0 });
      if (!openX && !openY) crossings.push({ path, area: here, ...cross });
      node.cells.forEach((cell, index) => {
        const [right, lower] = [index % 2 === 1, index > 1];
        walk(cell, areas[index], [...path, index], {
          e: right ? free.e : edge('x', 'end', openX, areas[1].x),
          w: right ? edge('x', 'start', openX, cross.x) : free.w,
          s: lower ? free.s : edge('y', 'end', openY, areas[2].y),
          n: lower ? edge('y', 'start', openY, cross.y) : free.n,
        });
      });
      return;
    }
    const { line, areas } = binaryAreas(node, here);
    const open = node.free !== undefined;
    if (!open) {
      lines.push(node.axis === 'x'
        ? { path, area: here, axis: 'x', x: line, y: here.y, w: 0, h: here.h }
        : { path, area: here, axis: 'y', x: here.x, y: line, w: here.w, h: 0 });
    }
    const partnerOfEnd = node.axis === 'x' ? areas[1].x : areas[1].y;
    walk(node.a, areas[0], [...path, 0], { ...free, [node.axis === 'x' ? 'e' : 's']: edge(node.axis, 'end', open, partnerOfEnd) });
    walk(node.b, areas[1], [...path, 1], { ...free, [node.axis === 'x' ? 'w' : 'n']: edge(node.axis, 'start', open, line) });
  }(tree, area, [], {}));
  return { rects, lines, crossings, gaps, edges };
}

// splitEdges names the handles a window has for the edges an unlocked line left
// it, with the corner between any two.
export function splitEdges(free) {
  const sides = ['n', 's', 'e', 'w'].filter((side) => free?.[side]);
  const corners = ['ne', 'nw', 'se', 'sw'].filter((corner) => [...corner].every((side) => sides.includes(side)));
  return [...sides, ...corners];
}

// movedLine is the tree with one line of the node at path at a new position.
// A quad's axis 'x' moves its vertical line, and 'y' its horizontal one.
export function movedLine(tree, path, axis, pointer, area) {
  return atPath(tree, path, (node) => {
    if (isQuad(node)) {
      return axis === 'x' ? { ...node, x: clampShare((pointer.x - area.x) / area.w) } : { ...node, y: clampShare((pointer.y - area.y) / area.h) };
    }
    return { ...node, at: node.axis === 'x' ? clampShare((pointer.x - area.x) / area.w) : clampShare((pointer.y - area.y) / area.h) };
  });
}

// movedEdge is the tree with an unlocked edge of the node at path at a new
// position.
export function movedEdge(tree, path, { axis, side, area }, pointer) {
  const share = clampShare(axis === 'x' ? (pointer.x - area.x) / area.w : (pointer.y - area.y) / area.h);
  return atPath(tree, path, (node) => {
    if (isQuad(node)) return { ...node, [side === 'end' ? axis : axis === 'x' ? 'freeX' : 'freeY']: share };
    return { ...node, [side === 'end' ? 'at' : 'free']: share };
  });
}

// edgeMeets reports whether a pointer dragging an unlocked edge is within reach
// of the edge across its line, from either side.
export function edgeMeets({ axis, partner }, pointer, reach) {
  return Math.abs((axis === 'x' ? pointer.x : pointer.y) - partner) <= reach;
}

// lockedEdge is the tree with an unlocked edge's line shared again, where the
// edge across it stands. The other line of a quad stays as it is.
export function lockedEdge(tree, path, { axis, area, partner }) {
  const share = clampShare(axis === 'x' ? (partner - area.x) / area.w : (partner - area.y) / area.h);
  return atPath(tree, path, (node) => {
    if (isQuad(node)) {
      const { [axis === 'x' ? 'freeX' : 'freeY']: _, ...rest } = node;
      return { ...rest, [axis]: share };
    }
    const { free: _, ...rest } = node;
    return { ...rest, at: share };
  });
}

// unlockedLine is the tree with one line of the node at path no longer shared.
// Both sides start where they stand, and every window stays in its place: that
// is where the line shows in area, which a stored share can lie past.
export function unlockedLine(tree, path, axis, area) {
  const size = axis === 'x' ? area.w : area.h;
  const across = (node) => (axis === 'x' ? minSize(node).w : minSize(node).h);
  return atPath(tree, path, (node) => {
    if (!isQuad(node)) {
      const at = clampLine(node.at, size, across(node.a), across(node.b));
      return { ...node, at, free: at };
    }
    const [tl, tr, bl, br] = node.cells.map(across);
    const [before, after] = axis === 'x' ? [Math.max(tl, bl), Math.max(tr, br)] : [Math.max(tl, tr), Math.max(bl, br)];
    const at = clampLine(axis === 'x' ? node.x : node.y, size, before, after);
    return axis === 'x' ? { ...node, x: at, freeX: at } : { ...node, y: at, freeY: at };
  });
}

function lineOf(axis, at, free, a, b) {
  return { axis, at, ...(free !== undefined && { free }), a, b };
}

// tidied folds whatever only holds empty cells into one.
function tidied(node) {
  if (isLeaf(node) || isGap(node)) return node;
  const kept = childrenOf(node).map(tidied);
  if (kept.every(isGap)) return GAP;
  return isQuad(node) ? { ...node, cells: kept } : { ...node, a: kept[0], b: kept[1] };
}

// withoutWindow takes a window out and leaves its cell empty, so nothing else
// moves and the cell stays a place to drop one. Null with no window left.
export function withoutWindow(tree, id) {
  const path = tree && pathTo(tree, id);
  if (!path) return tree;
  if (!path.length) return null;
  const next = tidied(atPath(tree, path, () => GAP));
  return leavesOf(next).length ? next : null;
}

// keepingWindows is what is showing now: the tree with the same shape, and an
// empty cell held for each window that is away, for when it is back. A path in
// one names the same node in the other.
export function keepingWindows(tree, present) {
  if (!tree) return null;
  const showing = (node) => {
    if (isLeaf(node)) return present.has(node.id) ? node : { gap: true, held: node.id };
    if (isGap(node)) return node;
    return childrenOf(node).reduce((next, child, index) => withChild(next, index, showing(child)), node);
  };
  const kept = showing(tree);
  return leavesOf(kept).length ? kept : null;
}

// whatever edge or corner of an area a zone names.
const sideOf = (zone) => (zone.includes('left') ? 'left' : zone.includes('right') ? 'right' : '');
const endOf = (zone) => (zone.includes('top') ? 'top' : zone.includes('bottom') ? 'bottom' : '');

function pairOf(axis, first, second) {
  return { axis, at: 0.5, a: first, b: second };
}

// The room's four cells, top left to bottom right, and the ones each zone takes.
const ZONE_CELLS = Object.freeze({
  'top-left': [0], 'top-right': [1], 'bottom-left': [2], 'bottom-right': [3], left: [0, 2], right: [1, 3], bottom: [2, 3],
});

// roomTree is the tree windows snapped alone make together, from their [id, zone]
// pairs, or null where two want one cell. Each stays the kind of snap it is: a
// corner stays a corner beside a half, and what nobody took stays a gap.
export function roomTree(placed) {
  const cells = [GAP, GAP, GAP, GAP];
  for (const [id, zone] of placed) {
    const taken = ZONE_CELLS[zone] || [];
    if (!taken.length || taken.some((cell) => !isGap(cells[cell]))) return null;
    const window = { id };
    for (const cell of taken) cells[cell] = window;
  }
  const [tl, tr, bl, br] = cells;
  const whole = (first, second) => !isGap(first) && first === second;
  const column = (top, low) => (top === low ? top : pairOf('y', top, low));
  if (whole(tl, bl) || whole(tr, br)) return pairOf('x', column(tl, bl), column(tr, br));
  if (whole(bl, br)) return pairOf('y', pairOf('x', tl, tr), bl);
  return { x: 0.5, y: 0.5, cells: [tl, tr, bl, br] };
}

// rectZone names the snap of a rectangle in area, for a window that fills a gap.
export function rectZone(rect, area) {
  const side = rect.x + rect.w / 2 < area.x + area.w / 2 ? 'left' : 'right';
  const end = rect.y + rect.h / 2 < area.y + area.h / 2 ? 'top' : 'bottom';
  if (rect.h >= area.h) return side;
  return rect.w >= area.w && end === 'bottom' ? 'bottom' : `${end}-${side}`;
}

// withinGap brings id into an empty cell. A room zone that takes two empty cells
// of a quad takes both, and a corner of an empty half splits it against the line
// beside it, so it completes a quad. Otherwise the window fills the cell.
export function withinGap(tree, gap, pointer, id, room = '') {
  const parentPath = gap.path.slice(0, -1);
  const index = gap.path[gap.path.length - 1];
  const parent = nodeAt(tree, parentPath);
  const window = { id };
  const taken = ZONE_CELLS[room] || [];
  if (isQuad(parent) && taken.length === 2 && taken.includes(index) && taken.every((cell) => isGap(parent.cells[cell]))) {
    return atPath(tree, parentPath, (quad) => wholeSide(quad, room, window));
  }
  if (!isQuad(parent)) {
    const axis = parent.axis === 'y' ? 'x' : 'y';
    const zone = dropZone(pointer, gap.rect);
    const toward = axis === 'x' ? sideOf(zone) : endOf(zone);
    const beside = childrenOf(parent)[1 - index];
    if (toward && !isLeaf(beside) && !isGap(beside) && beside.axis === axis) {
      const part = toward === 'left' || toward === 'top' ? pairOf(axis, window, GAP) : pairOf(axis, GAP, window);
      return atPath(atPath(tree, gap.path, () => part), parentPath, (node) => promoted(node, index));
    }
  }
  return atPath(tree, gap.path, () => window);
}

function wholeSide(quad, room, window) {
  const [tl, tr, bl, br] = quad.cells;
  if (room === 'bottom') return lineOf('y', quad.y, quad.freeY, lineOf('x', quad.x, quad.freeX, tl, tr), window);
  if (room === 'left') return lineOf('x', quad.x, quad.freeX, window, lineOf('y', quad.y, quad.freeY, tr, br));
  return lineOf('x', quad.x, quad.freeX, lineOf('y', quad.y, quad.freeY, tl, bl), window);
}

// promoted makes a quad of a binary whose two children split across it: a corner
// drop that completes four windows shares one crossing. It lies on the line that
// was there, not the one the drop just made in child fresh.
function promoted(node, fresh) {
  if (isLeaf(node) || isGap(node) || isQuad(node)) return node;
  const { a, b } = node;
  const across = (child) => !isLeaf(child) && !isGap(child) && !isQuad(child) && child.axis !== node.axis && child.free === undefined;
  if (node.free !== undefined || !across(a) || !across(b)) return node;
  const line = (fresh === 0 ? b : a).at;
  if (node.axis === 'x') return { x: node.at, y: line, cells: [a.a, b.a, a.b, b.b] };
  return { x: line, y: node.at, cells: [a.a, a.b, b.a, b.b] };
}

// withWindow splits the window target to bring id in beside it, on target's zone.
// An edge halves it. A corner takes a quarter of what target's split gave it,
// across that split, and completes a quad where that makes four around a crossing.
export function withWindow(tree, target, zone, id) {
  const path = pathTo(tree, target);
  if (!path) return tree;
  const parentPath = path.slice(0, -1);
  const parent = path.length ? nodeAt(tree, parentPath) : null;
  const side = sideOf(zone);
  const end = endOf(zone);
  const corner = !!side && !!end;
  const axis = corner ? (parent?.axis === 'y' ? 'x' : 'y') : side ? 'x' : 'y';
  const first = axis === 'x' ? side === 'left' : end === 'top';
  const split = (leaf) => (first ? pairOf(axis, { id }, leaf) : pairOf(axis, leaf, { id }));
  const next = atPath(tree, path, split);
  return corner && path.length ? atPath(next, parentPath, (node) => promoted(node, path[path.length - 1])) : next;
}

// dropZone is the edge or corner of rect a pointer is on, or '' in the middle of it.
export function dropZone(pointer, rect) {
  const dx = (pointer.x - rect.x) / rect.w;
  const dy = (pointer.y - rect.y) / rect.h;
  if (dx < 0 || dx > 1 || dy < 0 || dy > 1) return '';
  const side = dx < OVERLAY_SPLIT_BAND ? 'left' : dx > 1 - OVERLAY_SPLIT_BAND ? 'right' : '';
  const end = dy < OVERLAY_SPLIT_BAND ? 'top' : dy > 1 - OVERLAY_SPLIT_BAND ? 'bottom' : '';
  return side && end ? `${end}-${side}` : side || end;
}

// detachSpot is where a line's detach button sits: its middle, or on a quad, the
// middle of the arm before the crossing, which is the crossing's own.
export function detachSpot(line, crossing = null) {
  const end = crossing ? (line.axis === 'x' ? crossing.y - line.y : crossing.x - line.x) : (line.axis === 'x' ? line.h : line.w);
  return line.axis === 'x' ? { x: line.x, y: line.y + end / 2 } : { x: line.x + end / 2, y: line.y };
}

// fits reports whether the room holds every window of a tree at its minimum.
export function fits(tree, area) {
  const { w, h } = minSize(tree);
  return w <= area.w && h <= area.h;
}

function validNode(raw, depth = 0) {
  if (depth > 8 || !raw || typeof raw !== 'object') return null;
  if (typeof raw.id === 'string' && raw.id) return { id: raw.id };
  if (raw.gap === true) return GAP;
  const open = (field) => (Number.isFinite(raw[field]) ? { [field]: clampShare(raw[field]) } : {});
  if (Array.isArray(raw.cells)) {
    const cells = raw.cells.map((cell) => validNode(cell, depth + 1));
    if (cells.length !== 4 || cells.some((cell) => !cell)) return null;
    return Number.isFinite(raw.x) && Number.isFinite(raw.y) ? { x: clampShare(raw.x), y: clampShare(raw.y), ...open('freeX'), ...open('freeY'), cells } : null;
  }
  const [a, b] = [validNode(raw.a, depth + 1), validNode(raw.b, depth + 1)];
  if (!a || !b || (raw.axis !== 'x' && raw.axis !== 'y') || !Number.isFinite(raw.at)) return null;
  return { axis: raw.axis, at: clampShare(raw.at), ...open('free'), a, b };
}

// parseSplit reads a saved tree, or null. A window named twice cannot be laid out.
export function parseSplit(raw) {
  try {
    const tree = validNode(JSON.parse(raw || 'null'));
    if (!tree) return null;
    const ids = leavesOf(tree);
    return !isLeaf(tree) && ids.length >= 1 && new Set(ids).size === ids.length ? tree : null;
  } catch {
    return null;
  }
}
