// overlay-split-model.js -- how overlay windows share the room (tcl-ppvf8g), so
// that two windows, 2+1 and four are all one thing: a tree of splits.
//
//   leaf    { id }                          one window
//   binary  { axis, at, a, b }              one area in two. axis 'x' puts a left
//                                           of b, 'y' puts a above b; at is the
//                                           line's share of the area, 0 to 1.
//   quad    { x, y, cells: [tl, tr, bl, br] }  one area in four around a shared
//                                           crossing. Each line runs the full
//                                           width or height, so it is not two
//                                           nested binaries.
//
// Any child can be any node. A tree has at least two windows, and a quad holds
// four children: anything smaller collapses (collapseQuad). A path names a node
// by the child indexes that reach it. Everything here is DOM-free.

import { OVERLAY_MIN_H, OVERLAY_MIN_W } from './overlay-window-model.js';

// How far into a window, as a share of its size, a drop counts as its edge.
export const OVERLAY_SPLIT_BAND = 0.3;

const isLeaf = (node) => typeof node?.id === 'string';
const isQuad = (node) => Array.isArray(node?.cells);
const childrenOf = (node) => (isLeaf(node) ? [] : isQuad(node) ? node.cells : [node.a, node.b]);
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
  if (isLeaf(node)) return { w: OVERLAY_MIN_W, h: OVERLAY_MIN_H };
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

function clampBinary(node, area) {
  const [a, b] = [minSize(node.a), minSize(node.b)];
  return node.axis === 'x' ? clampLine(node.at, area.w, a.w, b.w) : clampLine(node.at, area.h, a.h, b.h);
}

function clampQuad(node, area) {
  const [tl, tr, bl, br] = node.cells.map(minSize);
  return {
    x: clampLine(node.x, area.w, Math.max(tl.w, bl.w), Math.max(tr.w, br.w)),
    y: clampLine(node.y, area.h, Math.max(tl.h, tr.h), Math.max(bl.h, br.h)),
  };
}

function binaryAreas(node, area) {
  const at = clampBinary(node, area);
  if (node.axis === 'x') {
    const w = Math.round(area.w * at);
    return { line: area.x + w, areas: [{ ...area, w }, { ...area, x: area.x + w, w: area.w - w }] };
  }
  const h = Math.round(area.h * at);
  return { line: area.y + h, areas: [{ ...area, h }, { ...area, y: area.y + h, h: area.h - h }] };
}

function quadAreas(node, area) {
  const { x, y } = clampQuad(node, area);
  const w = Math.round(area.w * x);
  const h = Math.round(area.h * y);
  const right = { x: area.x + w, w: area.w - w };
  const lower = { y: area.y + h, h: area.h - h };
  return {
    cross: { x: area.x + w, y: area.y + h },
    areas: [{ ...area, w, h }, { ...area, ...right, h }, { ...area, w, ...lower }, { ...area, ...right, ...lower }],
  };
}

// splitLayout lays a tree out over area. rects maps each window to its
// rectangle; lines are the split lines, each { path, area, axis, x, y, w, h }
// with axis as the node's, and crossings are the quads', { path, area, x, y }.
export function splitLayout(tree, area) {
  const rects = {};
  const lines = [];
  const crossings = [];
  (function walk(node, here, path) {
    if (isLeaf(node)) {
      rects[node.id] = here;
      return;
    }
    if (isQuad(node)) {
      const { cross, areas } = quadAreas(node, here);
      lines.push({ path, area: here, axis: 'x', x: cross.x, y: here.y, w: 0, h: here.h });
      lines.push({ path, area: here, axis: 'y', x: here.x, y: cross.y, w: here.w, h: 0 });
      crossings.push({ path, area: here, ...cross });
      node.cells.forEach((cell, index) => walk(cell, areas[index], [...path, index]));
      return;
    }
    const { line, areas } = binaryAreas(node, here);
    lines.push(node.axis === 'x'
      ? { path, area: here, axis: 'x', x: line, y: here.y, w: 0, h: here.h }
      : { path, area: here, axis: 'y', x: here.x, y: line, w: here.w, h: 0 });
    walk(node.a, areas[0], [...path, 0]);
    walk(node.b, areas[1], [...path, 1]);
  }(tree, area, []));
  return { rects, lines, crossings };
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

// collapseQuad is what a quad becomes without one cell. The cell's neighbour in
// its row takes the room, so the horizontal line stays where it is.
function collapseQuad(quad, gone) {
  const [tl, tr, bl, br] = quad.cells;
  const rows = [[tl, tr], [bl, br]];
  const row = (cells, index) => {
    const kept = cells.filter((_, i) => i !== index);
    return kept.length === 1 ? kept[0] : { axis: 'x', at: quad.x, a: cells[0], b: cells[1] };
  };
  return {
    axis: 'y', at: quad.y,
    a: row(rows[0], gone < 2 ? gone : -1),
    b: row(rows[1], gone >= 2 ? gone - 2 : -1),
  };
}

// withoutWindow takes a window out, and its sibling gets the room. Null when
// that leaves fewer than two windows, because one window is not a split.
export function withoutWindow(tree, id) {
  const path = tree && pathTo(tree, id);
  if (!path) return tree;
  if (!path.length) return null;
  const index = path[path.length - 1];
  const next = atPath(tree, path.slice(0, -1), (parent) => (isQuad(parent) ? collapseQuad(parent, index) : childrenOf(parent)[1 - index]));
  return isLeaf(next) ? null : next;
}

// keepingWindows is the part of a tree that is showing now. The saved tree keeps
// a minimized or closed window's place, for when it is back.
export function keepingWindows(tree, present) {
  return leavesOf(tree).reduce((kept, id) => (present.has(id) ? kept : withoutWindow(kept, id)), tree);
}

// whatever edge or corner of an area a zone names.
const sideOf = (zone) => (zone.includes('left') ? 'left' : zone.includes('right') ? 'right' : '');
const endOf = (zone) => (zone.includes('top') ? 'top' : zone.includes('bottom') ? 'bottom' : '');

function pairOf(axis, first, second) {
  return { axis, at: 0.5, a: first, b: second };
}

// roomPair is the tree two snapped windows make, or null where their zones do
// not face each other. Windows on opposite sides stand side by side, and two
// corners of one side stack: the pair fills the room rather than leave a hole.
export function roomPair(placed, zone, id, placedZone) {
  const [one, other] = [{ id }, { id: placed }];
  if (sideOf(zone) && sideOf(placedZone) && sideOf(zone) !== sideOf(placedZone)) {
    return sideOf(zone) === 'left' ? pairOf('x', one, other) : pairOf('x', other, one);
  }
  if (endOf(zone) && endOf(placedZone) && endOf(zone) !== endOf(placedZone)) {
    return endOf(zone) === 'top' ? pairOf('y', one, other) : pairOf('y', other, one);
  }
  return null;
}

// promoted makes a quad of a binary whose two children split across it: a corner
// drop that completes four windows shares one crossing.
function promoted(node) {
  if (isLeaf(node) || isQuad(node)) return node;
  const { a, b } = node;
  const across = (child) => !isLeaf(child) && !isQuad(child) && child.axis !== node.axis;
  if (!across(a) || !across(b)) return node;
  if (node.axis === 'x') return { x: node.at, y: a.at, cells: [a.a, b.a, a.b, b.b] };
  return { x: a.at, y: node.at, cells: [a.a, a.b, b.a, b.b] };
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
  return corner && path.length ? atPath(next, parentPath, promoted) : next;
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
  if (Array.isArray(raw.cells)) {
    const cells = raw.cells.map((cell) => validNode(cell, depth + 1));
    if (cells.length !== 4 || cells.some((cell) => !cell)) return null;
    return Number.isFinite(raw.x) && Number.isFinite(raw.y) ? { x: clampShare(raw.x), y: clampShare(raw.y), cells } : null;
  }
  const [a, b] = [validNode(raw.a, depth + 1), validNode(raw.b, depth + 1)];
  if (!a || !b || (raw.axis !== 'x' && raw.axis !== 'y') || !Number.isFinite(raw.at)) return null;
  return { axis: raw.axis, at: clampShare(raw.at), a, b };
}

// parseSplit reads a saved tree, or null. A window named twice cannot be laid out.
export function parseSplit(raw) {
  try {
    const tree = validNode(JSON.parse(raw || 'null'));
    if (!tree || isLeaf(tree)) return null;
    const ids = leavesOf(tree);
    return new Set(ids).size === ids.length ? tree : null;
  } catch {
    return null;
  }
}
