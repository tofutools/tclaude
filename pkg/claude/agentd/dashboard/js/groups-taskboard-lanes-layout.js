// groups-taskboard-lanes-layout.js -- the lanes view's own layout, kept per
// group and per viewer in dashPrefs (docs/taskboard.md): the column widths
// (drag a column head's edge; double-click it to reset), the order of the
// lanes and of their headings (drag a lane head or a heading), and which
// cells are open past "+ n more". It is a view preference, not board state:
// no write reaches the board.

import { useState } from 'preact/hooks';
import { dashPrefs } from './prefs.js';

// The grid's six columns: the lane heads, then the five stages. A board
// with custom columns (In review, …) has more stages; the width functions
// take how many.
export const LAYOUT_COLUMNS = ['lane', 'ideas', 'next', 'progress', 'blocked', 'done'];
const STAGES = LAYOUT_COLUMNS.length - 1;
export const DEFAULT_LANE_PX = 170;
// MIN_PX keeps a column wide enough to read a card title or a lane name.
export const MIN_PX = { lane: 110, stage: 90 };

const WIDTHS_PREF = (group) => `tclaude.dash.groups.taskboard.lanes-widths.${group}`;
const ORDER_PREF = (group) => `tclaude.dash.groups.taskboard.lanes-order.${group}`;
const OPEN_PREF = (group, view) => `tclaude.dash.groups.taskboard.cells-open.${view}.${group}`;

function readJSON(key) {
  try {
    const raw = dashPrefs.getItem(key);
    return raw ? JSON.parse(raw) : null;
  } catch (_) {
    return null;
  }
}

// --- column widths ---

// laneWidths reads the stored widths: { lane: px, stages: [n weights] }, or
// null for the default grid. A malformed value, or one stored for another
// number of stages (a custom column added or removed since), reads as the
// default.
export function laneWidths(group, stages = STAGES) {
  const n = stages;
  const stored = readJSON(WIDTHS_PREF(group));
  const lane = Number(stored?.lane);
  const weights = Array.isArray(stored?.stages) ? stored.stages.map(Number) : null;
  if (!Number.isFinite(lane) || lane < MIN_PX.lane || !weights || weights.length !== n || weights.some((w) => !Number.isFinite(w) || w <= 0)) return null;
  return { lane: Math.round(lane), stages: weights };
}

export function setLaneWidths(group, widths) {
  if (!widths) dashPrefs.removeItem(WIDTHS_PREF(group));
  else dashPrefs.setItem(WIDTHS_PREF(group), JSON.stringify({ lane: Math.round(widths.lane), stages: widths.stages.map((w) => Math.round(w)) }));
}

// gridColumns is the grid-template-columns for widths: the lane column in
// pixels, the stages as weights of what is left (so the grid still fills
// the card at any width), each with its minimum.
export function gridColumns(widths, stages = STAGES) {
  if (!widths) return stages === STAGES ? '' : `${DEFAULT_LANE_PX}px repeat(${stages}, minmax(${MIN_PX.stage}px, 1fr))`;
  return `${widths.lane}px ${widths.stages.map((w) => `minmax(${MIN_PX.stage}px, ${w}fr)`).join(' ')}`;
}

// gridMinWidth is the narrowest the grid gets, in pixels: the lane column
// plus each stage at its minimum. Each row is at least this wide, so a row
// spans every column it draws even when the grid scrolls sideways, and the
// pinned lane column stays in view across the whole scroll.
export function gridMinWidth(widths, stages = STAGES) {
  return (widths ? widths.lane : DEFAULT_LANE_PX) + (widths ? widths.stages.length : stages) * MIN_PX.stage;
}

// resizeColumns moves the edge on the right of column `index` (0 is the
// lane column) by dx pixels, from the columns' measured pixel widths
// `measured` (six numbers). The lane column grows or shrinks against the
// stages; a stage edge trades width with its right-hand neighbour, so the
// total stays. Minimums hold. Returns the new widths, stages in pixels (they
// are weights; pixels are as good as any). measured holds the lane column
// and every stage, however many.
export function resizeColumns(measured, index, dx) {
  const px = measured.map((w) => Math.max(0, Number(w) || 0));
  const n = px.length - 1;
  if (index === 0) {
    const stagesTotal = px.slice(1).reduce((a, b) => a + b, 0);
    const room = Math.max(0, stagesTotal - n * MIN_PX.stage);
    const lane = Math.min(Math.max(MIN_PX.lane, px[0] + dx), px[0] + room);
    return { lane, stages: px.slice(1).map((w) => w || MIN_PX.stage) };
  }
  if (index < 1 || index > n - 1) return { lane: px[0], stages: px.slice(1) };
  const left = px[index];
  const right = px[index + 1];
  const move = Math.min(Math.max(dx, MIN_PX.stage - left), right - MIN_PX.stage);
  const next = px.slice();
  if (left + right >= 2 * MIN_PX.stage) {
    next[index] = left + move;
    next[index + 1] = right - move;
  }
  return { lane: next[0], stages: next.slice(1) };
}

// --- lane order ---

// laneOrder reads the stored order: { lanes: { by: [agent ids] }, heads: {
// by: [heading keys] } }, each kept per "lanes by" choice, so arranging the
// lanes by role leaves the order by agent alone. Lanes and headings it does
// not list keep their place after the listed ones, so a new agent appends at
// the end.
export function laneOrder(group) {
  const stored = readJSON(ORDER_PREF(group));
  const byKey = (value, ok) => {
    if (!value || typeof value !== 'object' || Array.isArray(value)) return {};
    return Object.fromEntries(Object.entries(value).filter(([, keys]) => Array.isArray(keys))
      .map(([by, keys]) => [by, keys.filter(ok)]));
  };
  const isString = (key) => typeof key === 'string';
  return { lanes: byKey(stored?.lanes, isString), heads: byKey(stored?.heads, isString) };
}

const anyListed = (byKeys) => Object.values(byKeys || {}).some((keys) => Array.isArray(keys) && keys.length > 0);

export function setLaneOrder(group, order) {
  if (!order || (!anyListed(order.lanes) && !anyListed(order.heads))) dashPrefs.removeItem(ORDER_PREF(group));
  else dashPrefs.setItem(ORDER_PREF(group), JSON.stringify(order));
}

export function hasCustomOrder(group) {
  const order = laneOrder(group);
  return anyListed(order.lanes) || anyListed(order.heads);
}

// sortBy orders items by their key's place in `keys`; the unlisted keep
// their own order after the listed.
export function sortBy(items, keys, keyOf) {
  const rank = new Map((keys || []).map((key, i) => [key, i]));
  return items.map((item, i) => [item, i]).sort(([a, i], [b, j]) => {
    const ra = rank.has(keyOf(a)) ? rank.get(keyOf(a)) : Infinity;
    const rb = rank.has(keyOf(b)) ? rank.get(keyOf(b)) : Infinity;
    return ra === rb ? i - j : ra - rb;
  }).map(([item]) => item);
}

// applyLaneOrder sorts the lane groups (laneModel's groups) by the stored
// order: the headings for `by`, then the lanes within each heading. The
// former members' group stays last, where it always is.
export function applyLaneOrder(groups, order, by) {
  const former = groups.filter((g) => g.key === 'former');
  const rest = groups.filter((g) => g.key !== 'former');
  const sorted = sortBy(rest, order.heads?.[by], (g) => g.key);
  return [...sorted, ...former].map((g) => ({ ...g, lanes: sortBy(g.lanes, order.lanes?.[by], (lane) => lane.id) }));
}

// headedGroups are the lane groups whose heading is drawn and may move:
// those with a label, but never the former members'.
export function headedGroups(groups) {
  return groups.filter((g) => g.key !== 'former' && g.label);
}

// moveBefore returns keys (the current visible order) with `moving` placed
// just before `target`, or after it when `after` is set.
export function moveBefore(keys, moving, target, after = false) {
  if (moving === target || !keys.includes(moving) || !keys.includes(target)) return keys;
  const out = keys.filter((key) => key !== moving);
  const at = out.indexOf(target) + (after ? 1 : 0);
  out.splice(at, 0, moving);
  return out;
}

// moveLane stores a lane moved before (or after) another lane in the same
// heading, for `by`. `groups` is the visible lane groups; the stored lane
// order is the whole visible order of lanes, so every lane keeps the place
// it showed in.
export function moveLane(group, groups, by, moving, target, after = false) {
  const visible = groups.flatMap((g) => g.lanes.map((lane) => lane.id));
  const order = laneOrder(group);
  setLaneOrder(group, { ...order, lanes: { ...order.lanes, [by]: moveBefore(visible, moving, target, after) } });
}

// moveHead stores a heading moved before (or after) another one, for `by`.
export function moveHead(group, groups, by, moving, target, after = false) {
  const visible = headedGroups(groups).map((g) => g.key);
  const order = laneOrder(group);
  setLaneOrder(group, { ...order, heads: { ...order.heads, [by]: moveBefore(visible, moving, target, after) } });
}

export function resetLaneOrder(group) {
  setLaneOrder(group, null);
}

// --- open cells ---

// openCells reads the cells opened past "+ n more" in one view of a group:
// { "<lane>|<column>": true }. A malformed value reads as none open.
export function openCells(group, view) {
  const stored = readJSON(OPEN_PREF(group, view));
  if (!stored || typeof stored !== 'object' || Array.isArray(stored)) return {};
  return Object.fromEntries(Object.entries(stored).filter(([, on]) => on === true));
}

export function setOpenCells(group, view, open) {
  const on = Object.keys(open || {}).filter((key) => open[key]);
  if (!on.length) dashPrefs.removeItem(OPEN_PREF(group, view));
  else dashPrefs.setItem(OPEN_PREF(group, view), JSON.stringify(Object.fromEntries(on.map((key) => [key, true]))));
}

// useOpenCells is a view's open cells as [open, setOpen], stored per group
// and view, so a cell stays open until "Show less" closes it: opening a
// task's editor, which unmounts the board, or a reload keeps it. setOpen
// takes a value or an updater, as useState's does.
export function useOpenCells(group, view) {
  const [state, setState] = useState(() => ({ group, view, open: openCells(group, view) }));
  const current = (s) => (s.group === group && s.view === view ? s.open : openCells(group, view));
  const setOpen = (next) => setState((s) => {
    const now = current(s);
    const open = typeof next === 'function' ? next(now) : next;
    if (open !== now) setOpenCells(group, view, open);
    return { group, view, open };
  });
  return [current(state), setOpen];
}
