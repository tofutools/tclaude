// overlay-window.js -- the one window every floating dashboard window sits in.
// The companion and the board overlay bring their content; this owns the rest:
// where the window is (placement and persistence), dragging, resizing from any
// edge or corner, edge snapping and maximize, minimize and open animations, the
// native-look controls, which window is in front, and handing focus back after
// a click on its chrome.
//
//   useOverlayWindow   the state and the gestures, one call per window
//   OverlayWindow      the DOM: snap guide, window, title bar, resize frame
//
// See overlay-window-model.js for the geometry, and overlay-split-model.js for
// the tree of splits windows share the room through. A window in a split keeps
// its own placement, and goes back to it when the split no longer holds it.
//
// The resize zones sit in a frame beside the window rather than inside it. A
// window clips its content to its rounded corners, which would cut off the half
// of each zone that straddles the edge. The frame covers only those strips and
// lets every other pointer through to the window.

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { computed, signal } from '@preact/signals';
import htm from 'htm';
import {
  clampPlacement, fitPlacement, freeEdges, parsePlacement, resizedPlacement, savedAfterResize, snappedResize, snapReach, snapZoneRects, viewportOf, zoneRect,
} from './overlay-window-model.js';
import {
  detachSpot, dragHit, dropTargets, edgeMeets, edgeTargets, fits, keepingWindows, leavesOf, lockedEdge, movedEdge, movedLine, nodeAt, normalized, parseSplit, rectZone, roomTree, splitEdges, splitLayout, targetAt, unlockedLine, withWindow, withinGap, withoutWindow,
} from './overlay-split-model.js';
import { WindowControls } from './overlay-window-controls.js';

const html = htm.bind(h);

// Corners come last: they are drawn over the edges they touch.
const RESIZE_ZONES = ['n', 's', 'e', 'w', 'nw', 'ne', 'sw', 'se'];
const TEAR_OFF_PX = 4;

// The ids of the windows, front last. A window joins as it mounts: floating at
// the front, snapped at the back, so a reload does not bury one behind a split.
// Levels come from it, inside the window layer (--z-window in ui.css, 1000;
// Snap Assist at 1089 and the snap guide at 1090 sit over the front split's
// lines, and the chrome layer starts at 2000), so whatever the operator opens
// paints over every window and raising one never leaves the band. A split moves
// as one, with its front window one above the rest, for when an unlocked line
// lets two overlap, and its lines above that: anything over the split covers
// them. Past the bottom level, windows share it and DOM order decides.
export const OVERLAY_TOP_LEVEL = 1084;
export const OVERLAY_BOTTOM_LEVEL = 1000;
const overlayStack = signal([]);
export const frontOverlayWindow = computed(() => overlayStack.value[overlayStack.value.length - 1] || '');

// overlayWindowsByStack is every window id that has been shown, the front last.
export const overlayWindowsByStack = () => overlayStack.peek();

export function raiseOverlayWindow(id) {
  overlayStack.value = [...overlayStack.peek().filter((other) => other !== id), ...(id ? [id] : [])];
}

function joinOverlayStack(id, snapped) {
  if (!id || overlayStack.peek().includes(id)) return;
  overlayStack.value = snapped ? [id, ...overlayStack.peek()] : [...overlayStack.peek(), id];
}

export function overlayStackReset() {
  overlayStack.value = [];
}

export const OVERLAY_SPLIT_PREF = 'tclaude.dash.overlay-split';
// How near a line's detach spot the pointer brings its button out.
const DETACH_REACH = { along: 72, across: 40 };

// The saved tree of splits. A window that is away keeps its cell.
const splitTree = signal(null);
// Every snapped window showing now: id -> { zone, rect(), release() }.
const snappedWindows = signal(new Map());
// Every window that can come back to a cell, as it is open or minimized. A cell
// held for any other is one this tab can't fill, such as after a closed browser.
const aliveWindows = new Set();
// The tree as it shows now, or null.
const liveSplit = computed(() => keepingWindows(splitTree.value, new Set(snappedWindows.value.keys())));
let splitLoadedFrom = null;

const SPLIT_UNIT = '\0split';

// The stack as levels, front first, with a split as one entry.
const stackUnits = computed(() => {
  const split = new Set(leavesOf(liveSplit.value));
  return [...new Set(overlayStack.value.map((id) => (split.has(id) ? SPLIT_UNIT : id)).reverse())];
});

function levelOf(unit) {
  const depth = stackUnits.value.indexOf(unit);
  return depth < 0 ? OVERLAY_BOTTOM_LEVEL : Math.max(OVERLAY_BOTTOM_LEVEL, OVERLAY_TOP_LEVEL - 3 * depth);
}

// How many windows are tiled now: in the split or snapped to an edge alone.
export const tiledOverlayWindowCount = computed(() => snappedWindows.value.size);

// untileOverlayWindows floats every tiled window where it shows and takes it out
// of the split. A minimized window keeps its cell, and a maximized window is not
// tiled and stays as it is.
export function untileOverlayWindows() {
  const tiled = [...snappedWindows.peek()];
  if (!tiled.length) return false;
  for (const [, entry] of tiled) entry.unsnap();
  const tree = splitTree.peek();
  const next = tiled.reduce((rest, [id]) => withoutWindow(rest, id), tree);
  if (next !== tree) saveSplit(splitLoadedFrom, next);
  return true;
}

// forgetOverlayWindow takes a closed window out of the stack and any split. A
// window gone to the dock keeps its cell (keepCell), for when it comes back.
export function forgetOverlayWindow(id, keepCell = false) {
  overlayStack.value = overlayStack.peek().filter((other) => other !== id);
  if (!keepCell && leavesOf(splitTree.peek()).includes(id)) saveSplit(splitLoadedFrom, withoutWindow(splitTree.peek(), id));
}

// Whether a window not mounted now is on the dock, and will come back to its
// cell. overlay-instances.js, which keeps the dock, answers it.
let onDock = () => false;
export function setOverlayDockCheck(check) {
  onDock = check;
}

// A window keeps its cell while it is open, minimized in place or on the dock.
const keepsCell = { has: (id) => aliveWindows.has(id) || onDock(id) };

function loadSplit(prefs) {
  if (splitLoadedFrom === prefs) return;
  splitLoadedFrom = prefs;
  splitTree.value = parseSplit(readPref(prefs, OVERLAY_SPLIT_PREF));
}

// Every operation saves the tree normalized, so what it shows is all that
// decides what can be done with it next.
function saveSplit(prefs, made) {
  const tree = normalized(made);
  splitTree.value = tree;
  writePref(prefs, OVERLAY_SPLIT_PREF, tree ? JSON.stringify(tree) : '');
}

// Snap Assist (tcl-aahter): after the operator snaps a window, the largest cell
// left empty offers the windows outside the tiling, one cell at a time.
// { tree, gap: { path, rect }, from, base } while it asks, or null. tree is the
// split the cell is in, which only becomes the saved one when a window is placed,
// and base the saved split it was built on: if that changed meanwhile, the
// question is stale.
export const snapAssist = signal(null);
let snapAssistOn = true;
// Each window that snaps, by id: how to snap it to a zone of the room.
const assistPlacers = new Map();

export function setSnapAssistEnabled(on) {
  snapAssistOn = !!on;
  if (!snapAssistOn) snapAssist.value = null;
}

export function endSnapAssist() {
  if (snapAssist.peek()) snapAssist.value = null;
}

// tiledOverlayIds is every window in the split or snapped alone.
export function tiledOverlayIds() {
  return new Set([...leavesOf(splitTree.value), ...snappedWindows.value.keys()]);
}

// offerSnapAssist asks for the largest empty cell of tree, if there is one. A
// cell kept for a window that no longer exists is empty to it too.
function offerSnapAssist(tree, area, from) {
  const shown = tree && keepingWindows(tree, keepsCell);
  const cells = snapAssistOn && shown && fits(shown, area) ? splitLayout(shown, area).gaps : [];
  const size = ({ rect }) => rect.w * rect.h;
  const gap = cells.sort((a, b) => size(b) - size(a))[0];
  snapAssist.value = gap ? { tree, gap, area, from, base: splitTree.peek() } : null;
}

// placeSnapAssisted puts a window in the cell asking, and asks for the next.
export function placeSnapAssisted(windowId) {
  const assist = snapAssist.peek();
  const place = assistPlacers.get(windowId);
  if (!assist || !place) return false;
  if (splitTree.peek() !== assist.base) {
    endSnapAssist();
    return false;
  }
  const { tree, gap, area, from } = assist;
  const centre = { x: gap.rect.x + gap.rect.w / 2, y: gap.rect.y + gap.rect.h / 2 };
  const next = withinGap(tree, gap, centre, windowId, '', area);
  saveSplit(splitLoadedFrom, next);
  place(rectZone(gap.rect, area));
  offerSnapAssist(next, area, from);
  return true;
}

function showWindow(id, entry) {
  snappedWindows.value = new Map(snappedWindows.peek()).set(id, entry);
}

function hideWindow(id) {
  const windows = new Map(snappedWindows.peek());
  windows.delete(id);
  snappedWindows.value = windows;
}

// snapTopOf is where a snapped side panel starts: just below the nav bar.
function snapTopOf(win) {
  try {
    const bottom = win?.document?.querySelector?.('nav')?.getBoundingClientRect?.().bottom;
    return Number.isFinite(bottom) && bottom > 0 ? Math.round(bottom) : 0;
  } catch {
    return 0;
  }
}

export function reducedMotion(win) {
  try {
    return !!win?.matchMedia?.('(prefers-reduced-motion: reduce)')?.matches;
  } catch {
    return false;
  }
}

export function readPref(prefs, key) {
  try {
    return prefs?.getItem?.(key) ?? null;
  } catch {
    return null;
  }
}

export function writePref(prefs, key, value) {
  try {
    prefs?.setItem?.(key, value);
  } catch { /* prefs unavailable: the session keeps working */ }
}

// useOverlayWindow runs one window. prefKey is where its placement is saved and
// defaultPlacement(viewport) is where it goes before anything is. Without snaps
// it only floats. spotted(base, viewport, top) may name a place a floating
// window opens at instead of its saved one, onPlaced hears that the operator
// placed the window themselves, and onRefocus is asked to take the keyboard
// back after a click on the window's chrome. open is false while the window is
// minimized. id names it in a split, which only a window that snaps can join.
// join, from tabJoin, lets a drag of the window end by joining another's tabs.
export function useOverlayWindow({ id = '', win, prefs, prefKey, defaultPlacement, snaps = false, open = true, spotted = null, onPlaced = null, onRefocus = null, join = null }) {
  const windowRef = useRef(null);
  // The rectangle the window grows out of when it next opens.
  const openFrom = useRef(null);
  // The cleanup of an in-flight drag or resize, run if the window unmounts.
  const gesture = useRef(null);
  const saved = () => parsePlacement(readPref(prefs, prefKey));
  const snapTop = () => snapTopOf(win);
  // A snapped window takes the size the operator last gave that zone.
  const snappedPlacement = (zone) => zoneRect(zone, viewportOf(win), snapTop(), saved()?.panels?.[zone]);

  function floatingPlacement() {
    const viewport = viewportOf(win);
    const base = { ...(saved() || defaultPlacement(viewport)), snap: '' };
    return spotted?.(base, viewport, snapTop()) || fitPlacement(base, viewport);
  }

  const [placement, setPlacement] = useState(() => {
    const zone = snaps ? saved()?.snap : '';
    return zone ? snappedPlacement(zone) : floatingPlacement();
  });
  const [guide, setGuide] = useState(null);
  // The drop targets a drag shows, and the one under the pointer: { edges, gaps,
  // markers, hot }, or null with no drag.
  const [targets, setTargets] = useState(null);
  // A window being dragged that would float where it is let go.
  const [floating, setFloating] = useState(false);
  useEffect(() => () => gesture.current?.(), []);
  useState(() => {
    loadSplit(prefs);
    joinOverlayStack(id, !!placement.snap);
  });

  const splitArea = () => {
    const viewport = viewportOf(win);
    const y = snapTop();
    return { x: 0, y, w: viewport.width, h: zoneRect('max', viewport, y).h };
  };
  const layoutOf = (tree) => {
    if (!tree) return null;
    const area = splitArea();
    return fits(tree, area) ? splitLayout(tree, area) : null;
  };
  const layout = layoutOf(liveSplit.value);
  const cell = layout?.rects[id];
  const shown = cell ? { ...cell, snap: placement.snap } : placement;
  const shownRef = useRef(shown);
  shownRef.current = shown;

  // In the page for a split: a window maximized or away gives up its place.
  const joins = id && snaps && open && placement.snap && placement.snap !== 'max' ? placement.snap : '';
  useLayoutEffect(() => {
    if (!id || !snaps) return undefined;
    aliveWindows.add(id);
    return () => aliveWindows.delete(id);
  }, [id, snaps]);
  useLayoutEffect(() => {
    if (!joins) return undefined;
    showWindow(id, { zone: joins, rect: () => shownRef.current, pin: () => pinPanelSize(), unsnap: () => unsnapInPlace() });
    return () => hideWindow(id);
  }, [joins]);

  // Snap Assist snaps this window into the cell it was picked for.
  const placeIn = useRef(null);
  placeIn.current = (zone) => {
    setPlacement(snappedPlacement(zone));
    saveMovedWindow({ ...withSavedSize(shownRef.current), snap: zone });
  };
  useLayoutEffect(() => {
    if (!id || !snaps) return undefined;
    const place = (zone) => placeIn.current(zone);
    assistPlacers.set(id, place);
    return () => {
      if (assistPlacers.get(id) === place) assistPlacers.delete(id);
    };
  }, [id, snaps]);

  // Reopen where the operator left it, if that still fits.
  useLayoutEffect(() => {
    if (open) setPlacement((p) => (p.snap ? p : floatingPlacement()));
  }, [open]);

  // Grow the window out of the rectangle it was opened from.
  useLayoutEffect(() => {
    if (!open) return;
    const from = openFrom.current;
    openFrom.current = null;
    const node = windowRef.current;
    if (!from || !node || reducedMotion(win) || typeof node.animate !== 'function') return;
    // The effect above only queued the move, so the node isn't there yet.
    const target = placement.snap ? placement : floatingPlacement();
    const to = { left: target.x, top: target.y, width: target.w, height: target.h };
    if (!to.width || !to.height) return;
    node.animate([
      { transform: `translate(${from.left - to.left}px, ${from.top - to.top}px) scale(${from.width / to.width}, ${from.height / to.height})`, opacity: 0.5, borderRadius: '20px' },
      { transform: 'none', opacity: 1, borderRadius: '10px' },
    ], { duration: 180, easing: 'cubic-bezier(.2,.8,.2,1)' });
  }, [open]);

  // Keep a floating window on screen when the browser resizes, working from the
  // saved placement so a larger browser restores what a smaller one shrank.
  useEffect(() => {
    const onResize = () => setPlacement((p) => (p.snap ? snappedPlacement(p.snap) : floatingPlacement()));
    win?.addEventListener?.('resize', onResize);
    return () => win?.removeEventListener?.('resize', onResize);
  }, [win]);

  // A move keeps the saved size: the window may only be small for this viewport.
  function withSavedSize(p) {
    const size = saved();
    return size ? { ...p, w: size.w, h: size.h } : p;
  }

  // Only a resize of a snapped window changes panels; every other save keeps them.
  function savePlacement(p) {
    const panels = p.panels || saved()?.panels;
    writePref(prefs, prefKey, JSON.stringify(panels ? { ...p, panels } : p));
  }

  function saveMovedWindow(p) {
    savePlacement(p);
    onPlaced?.();
  }

  // Untiling floats the window where it shows now, so nothing moves on screen.
  function unsnapInPlace() {
    const { x, y, w, h } = shownRef.current;
    const floated = { x, y, w, h, snap: '' };
    setPlacement(floated);
    savePlacement(floated);
  }

  // A window out of a split goes back to its panel, so that takes the size it
  // has now. An unlocked window stays that size when the split later ends.
  function pinPanelSize() {
    const { snap, w, h } = shownRef.current;
    const current = saved();
    if (!snap || !current) return;
    savePlacement({ ...current, panels: { ...current.panels, [snap]: { w, h } } });
    setPlacement(snappedPlacement(snap));
  }

  // The targets a drag of this window shows, the same for the whole drag
  // (tcl-7uk9l6): the room's edge and corner strips, a box at the centre of each
  // empty cell and a marker on each free edge of a tiled window. tree is the
  // split those cells and windows are in. With no split, the windows snapped
  // alone make one for it, as Snap Assist sees the room around them.
  // vacated is the cell this gesture tore the window out of: it offers no box.
  function dragTargets(vacated = null) {
    const viewport = viewportOf(win);
    const bounds = splitArea();
    const { edge: reach } = snapReach(viewport);
    const zones = snapZoneRects(viewport, snapTop());
    const savedTree = splitTree.peek();
    const shownNow = new Set(snappedWindows.peek().keys());
    if (liveSplit.peek()) {
      const edges = edgeTargets(savedTree, bounds, id, { here: shownNow, alive: keepsCell, vacated, zones });
      return { edges, split: true, tree: savedTree, ...dropTargets(savedTree, bounds, id, { here: shownNow, alive: keepsCell, vacated, reach }) };
    }
    // Windows snapped alone are side panels, not a tiling: the room's own
    // strips snap there, each with its kind of snap.
    const edges = edgeTargets(null, bounds, id, { zones });
    const inTree = (other) => leavesOf(savedTree).includes(other);
    const loners = [...snappedWindows.peek()].filter(([other]) => other !== id && !inTree(other));
    const tree = loners.length ? roomTree(loners.map(([other, { zone }]) => [other, zone])) : null;
    const drawn = new Map(loners.map(([other, { rect }]) => [other, rect()]));
    return { edges, tree, ...(tree ? dropTargets(tree, bounds, id, { here: shownNow, vacated, reach, drawn }) : { gaps: [], markers: [] }) };
  }

  const hitAt = (raw, targets) => dragHit(raw, targets, splitArea());

  // Where a drop at pointer lands: { zone, tree?, guide }, or null to float.
  // Over a target, a box fills its empty cell and a marker splits its window on
  // that side; they come first, over the room's edge too. On the room's edge it
  // snaps there: over a split, as the piece of the strip there says (edgeTargets),
  // or with windows snapped alone making a tree together, each keeping its kind
  // of snap. Anywhere else it floats: nearness to an empty cell or a window does
  // nothing.
  function dropPlan(raw, targets, vacated = null) {
    const bounds = splitArea();
    const hit = hitAt(raw, targets);
    const room = hit?.edge?.zone || '';
    const plain = room ? { zone: room, guide: snappedPlacement(room) } : null;
    const savedTree = splitTree.peek();
    const others = [...snappedWindows.peek()].filter(([other]) => other !== id);
    const here = new Set([...others.map(([other]) => other), id]);
    const planned = (made, zone = '') => {
      const tree = normalized(made);
      const next = tree && layoutOf(keepingWindows(tree, here));
      const rect = next?.rects[id];
      return rect ? { zone: zone === 'top' ? 'top-left' : zone || rectZone(rect, bounds), tree, guide: rect } : null;
    };
    const target = hit?.target;
    if (target?.gap) {
      const { rect } = target.gap;
      return planned(withinGap(targets.tree, target.gap, { x: rect.x + rect.w / 2, y: rect.y + rect.h / 2 }, id, '', bounds));
    }
    if (target) return planned(withWindow(targets.tree, target.window, target.zone, id), target.zone);
    if (!room) return null;
    // A window outside the split cannot start a tree of its own: there is one.
    if (targets.split) {
      const { edge } = hit;
      return (edge.tree && planned(edge.tree, edge.window ? edge.zone : '')) || plain;
    }
    // Still over the cell it left, even at the room's edge, the window floats.
    const pointer = { x: Math.max(bounds.x, Math.min(bounds.x + bounds.w - 1, raw.x)), y: Math.max(bounds.y, Math.min(bounds.y + bounds.h - 1, raw.y)) };
    const inVacated = vacated && pointer.x >= vacated.x && pointer.x < vacated.x + vacated.w && pointer.y >= vacated.y && pointer.y < vacated.y + vacated.h;
    if (inVacated) return null;
    // Windows snapped alone are side panels, not a tiling: only their edges and
    // the room's make one.
    const inTree = (other) => leavesOf(savedTree).includes(other);
    const alone = others.filter(([other]) => !inTree(other)).map(([other, { zone }]) => [other, zone]);
    return (alone.length && planned(roomTree([...alone, [id, room]]), room)) || plain;
  }

  // With onCancel, a cancelled pointer ends the gesture through it, and
  // otherwise like a release. Escape ends it through onEscape, which is onCancel
  // unless given, and with neither does nothing.
  function track(event, onMove, onEnd, onCancel = null, onEscape = onCancel) {
    const handle = event.currentTarget;
    handle.setPointerCapture?.(event.pointerId);
    const stop = () => {
      handle.removeEventListener('pointermove', onMove);
      handle.removeEventListener('pointerup', up);
      handle.removeEventListener('pointercancel', cancelled);
      win?.removeEventListener?.('keydown', onKey, true);
      gesture.current = null;
    };
    const up = () => {
      stop();
      onEnd();
    };
    const cancel = () => {
      stop();
      onCancel();
    };
    const cancelled = onCancel ? cancel : up;
    const onKey = (key) => {
      if (key.key !== 'Escape') return;
      key.preventDefault();
      key.stopImmediatePropagation();
      stop();
      onEscape();
    };
    handle.addEventListener('pointermove', onMove);
    handle.addEventListener('pointerup', up);
    handle.addEventListener('pointercancel', cancelled);
    if (onEscape) win?.addEventListener?.('keydown', onKey, true);
    gesture.current = stop;
  }

  function onTitlePointerDown(event) {
    if (event.target.closest?.('button, input, [role="tab"]')) return;
    dragWindow(event);
  }

  function onTitleDoubleClick(event) {
    if (event.target.closest?.('button, input, select, a, [role="tab"], [role="tablist"] *')) return;
    toggleMaximize();
  }

  function dragWindow(event) {
    if (event.button !== 0 || !windowRef.current) return;
    event.preventDefault();
    endSnapAssist();
    let start = placement;
    const sx = event.clientX;
    const sy = event.clientY;
    let plan = null;
    let last = start;
    let moved = false;
    let vacated = null;
    const release = () => {
      setGuide(null);
      setTargets(null);
      setFloating(false);
      if (join?.drop()) return;
      if (plan) {
        setPlacement(snappedPlacement(plan.zone));
        saveMovedWindow({ ...withSavedSize(last), snap: plan.zone });
        if (plan.tree) saveSplit(prefs, plan.tree);
        // Snapped alone into an empty room, the room around it is the cells it
        // left. A saved split, even one all away, or another window snapped alone
        // has its own say over the room.
        const alone = !splitTree.peek() && ![...snappedWindows.peek().keys()].some((other) => other !== id);
        offerSnapAssist(plan.tree || (alone ? roomTree([[id, plan.zone]]) : null), splitArea(), id);
      } else if (moved) {
        // A click shows the fitted placement, which is not the operator's to keep.
        saveMovedWindow(withSavedSize(last));
      }
    };
    // A cancelled join stays a move: the window stops where it was let go.
    const cancelJoin = join && (() => {
      join.cancel();
      release();
    });
    // Escape calls the whole move off: the window goes back where it was, into
    // the cell it was torn out of, and nothing is saved.
    const original = placement;
    let tornFrom = null;
    const putBack = () => {
      join?.cancel();
      setGuide(null);
      setTargets(null);
      setFloating(false);
      setPlacement(original);
      if (tornFrom) saveSplit(prefs, tornFrom);
    };
    track(event, (m) => {
      if (start.snap) {
        // A click leaves a snapped window alone. Tearing it off takes a real
        // move, and restores a floating size under the pointer, held at the same
        // share of its width, so a tab near an edge doesn't put half of it off screen.
        if (Math.hypot(m.clientX - sx, m.clientY - sy) < TEAR_OFF_PX) return;
        const viewport = viewportOf(win);
        const floating = clampPlacement({ ...defaultPlacement(viewport), ...(saved() || {}), snap: '' }, viewport);
        const share = start.w > 0 ? Math.max(0, Math.min(1, (sx - start.x) / start.w)) : 0.5;
        start = { ...floating, x: sx - floating.w * share, y: sy - 14, snap: '' };
        vacated = cell || null;
        if (splitTree.peek() && leavesOf(splitTree.peek()).includes(id)) {
          tornFrom = splitTree.peek();
          saveSplit(prefs, withoutWindow(tornFrom, id));
        }
      }
      moved = true;
      last = clampPlacement({ ...start, x: start.x + m.clientX - sx, y: start.y + m.clientY - sy, snap: '' }, viewportOf(win));
      setPlacement(last);
      const pointer = { x: m.clientX, y: m.clientY };
      const shownTargets = snaps ? dragTargets(vacated) : null;
      // A box or a marker under the pointer takes the drop over a tab strip
      // there, the Collab view's included: they sit on top of everything.
      const onTarget = !!shownTargets && !!targetAt(pointer, shownTargets);
      if (onTarget) join?.cancel();
      else if (join?.over(pointer)) {
        plan = null;
        setGuide(null);
        setTargets(null);
        setFloating(false);
        return;
      }
      if (!snaps) return;
      plan = dropPlan(pointer, shownTargets, vacated);
      const hit = hitAt(pointer, shownTargets);
      setTargets({ ...shownTargets, hot: hit?.target || (hit?.edge && { edge: hit.edge.key }) || null });
      setGuide(plan?.guide || null);
      setFloating(!plan);
    }, release, cancelJoin, putBack);
  }

  function onResizePointerDown(event, zone = '') {
    if (event.button !== 0) return;
    event.preventDefault();
    event.stopPropagation();
    if (cell) {
      resizeSplitEdges(event, zone);
      return;
    }
    const start = placement;
    const sx = event.clientX;
    const sy = event.clientY;
    let last = start;
    let moved = false;
    track(event, (m) => {
      moved = true;
      const dx = m.clientX - sx;
      const dy = m.clientY - sy;
      // A snapped window resizes along its free edges only.
      last = start.snap ? snappedResize(start, zone, dx, dy, viewportOf(win), snapTop()) : resizedPlacement(start, zone, dx, dy, viewportOf(win));
      setPlacement(last);
    }, () => {
      if (!moved) return;
      if (!start.snap) {
        saveMovedWindow(savedAfterResize(last, zone, saved()));
        return;
      }
      const current = saved();
      if (current) savePlacement({ ...current, panels: { ...current.panels, [start.snap]: { w: last.w, h: last.h } } });
    });
  }

  // A line moves live and is saved when it lands.
  function onLinePointerDown(event, axes, path, area) {
    if (event.button !== 0) return;
    event.preventDefault();
    event.stopPropagation();
    track(event, (m) => {
      const pointer = { x: m.clientX, y: m.clientY };
      splitTree.value = axes.reduce((tree, axis) => movedLine(tree, path, axis, pointer, area), splitTree.peek());
    }, () => saveSplit(prefs, splitTree.peek()));
  }

  // A window's edge an unlocked line left it moves live, like a line. Brought
  // near the edge across its line it shows the snap, and released there the line
  // is shared again. Only that edge's line: a corner may leave the other unlocked.
  function resizeSplitEdges(event, zone) {
    const before = splitTree.peek();
    const moves = [...zone].map((side) => layout.edges[id]?.[side]).filter(Boolean)
      .map((edge) => ({ edge, path: edge.path }));
    let locking = [];
    const relocked = () => locking.reduce((tree, { edge, path }) => lockedEdge(tree, path, edge), splitTree.peek());
    track(event, (m) => {
      const pointer = { x: m.clientX, y: m.clientY };
      splitTree.value = moves.reduce((tree, { edge, path }) => movedEdge(tree, path, edge, pointer), splitTree.peek());
      const { edge: reach } = snapReach(viewportOf(win));
      locking = moves.filter(({ edge }) => edgeMeets(edge, pointer, reach));
      const next = locking.length ? layoutOf(keepingWindows(relocked(), new Set(snappedWindows.peek().keys()))) : null;
      setGuide(next?.rects[id] || null);
    }, () => {
      setGuide(null);
      saveSplit(prefs, relocked());
      if (locking.length < moves.length) pinPanelSize();
    }, () => {
      setGuide(null);
      splitTree.value = before;
    });
  }

  // Detach unlocks one line. The windows stay snapped where they are, and each
  // side then resizes on its own: a space or an overlap can open, with no line.
  function detachLine({ path, axis, area }) {
    for (const member of leavesOf(nodeAt(liveSplit.peek(), path))) snappedWindows.peek().get(member)?.pin();
    saveSplit(prefs, unlockedLine(splitTree.peek(), path, axis, area));
  }

  // A mouse press leaves focus on the control, where the next key meant for the
  // window's content would press it again. Keyboard activation (detail 0) stays.
  function releaseFocus(event) {
    if (!event?.detail) return;
    event.currentTarget?.blur?.();
    onRefocus?.();
  }

  // A click on the title bar or other empty space leaves focus where it was,
  // maybe in another window. Bring it here, so the border shows on this one, and
  // to the content when it wants it, so typing works. Controls keep their click,
  // which may have opened a dialog, and a text selection survives so it can be copied.
  function onWindowClick(event) {
    if (event.target.closest?.('button, a, input, textarea, select, label')) return;
    const node = windowRef.current;
    const selection = win?.getSelection?.();
    if (selection?.isCollapsed === false && node?.contains?.(selection.anchorNode)) return;
    // The window itself holds focus after a press on its content: that is not
    // the content having it.
    const active = win?.document?.activeElement;
    if (active !== node && node?.contains?.(active)) return;
    if (active !== node) node?.focus?.({ preventScroll: true });
    onRefocus?.();
  }

  // Restore goes back to the panel the window was maximized from, or floating.
  function toggleMaximize(event) {
    const { restoreSnap = '', ...rest } = saved() || placement;
    if (placement.snap === 'max') {
      setPlacement(restoreSnap ? snappedPlacement(restoreSnap) : floatingPlacement());
      savePlacement({ ...rest, snap: restoreSnap });
    } else {
      setPlacement(snappedPlacement('max'));
      savePlacement({ ...rest, snap: 'max', ...(placement.snap && { restoreSnap: placement.snap }) });
    }
    releaseFocus(event);
  }

  // shrinkTo plays the window down into a rectangle, then calls done. With
  // nowhere to go, or no animation wanted, it just calls done.
  function shrinkTo(rect, done) {
    const node = windowRef.current;
    if (!node || !rect || reducedMotion(win) || typeof node.animate !== 'function') {
      done();
      return;
    }
    const from = node.getBoundingClientRect();
    const animation = node.animate([
      { transform: 'none', opacity: 1 },
      { transform: `translate(${rect.left - from.left}px, ${rect.top - from.top}px) scale(${rect.width / from.width}, ${rect.height / from.height})`, opacity: 0.4 },
    ], { duration: 160, easing: 'cubic-bezier(.4,0,.8,.4)', fill: 'forwards' });
    animation.onfinish = () => { done(); animation.cancel(); };
  }

  const splitLines = cell && leavesOf(liveSplit.value)[0] === id ? layout : null;
  const edges = cell ? splitEdges(layout.edges[id]) : freeEdges(placement.snap);
  const unitLevel = levelOf(cell ? SPLIT_UNIT : id);
  const splitFront = cell && overlayStack.value.findLast((other) => layout.rects[other]) === id;
  return { placement: shown, floating, targets, edges, level: unitLevel + (splitFront ? 1 : 0), linesLevel: unitLevel + 2, splitLines, onLinePointerDown, detachLine, win, guide, windowRef, openFrom, onTitlePointerDown, onTitleDoubleClick, onResizePointerDown, onWindowClick, releaseFocus, toggleMaximize, shrinkTo, dragWindow, cancelGesture: () => gesture.current?.() };
}

// SnapTargets are the drop targets of a window being dragged, drawn over every
// window, the dragged one included, so a window passing over one never hides
// it: the room's edge strips, a box in each empty cell and the split markers on
// tiled windows. The one under the pointer is hot.
function SnapTargets({ targets }) {
  const { edges, gaps, markers, hot } = targets;
  const at = ({ x, y, w, h }) => `left:${x}px;top:${y}px;width:${w}px;height:${h}px`;
  const same = (a, b) => !!a && !!b && a.path.join() === b.path.join();
  return html`<div class="overlay-snap-targets" aria-hidden="true">
    ${edges.map(({ key, zone, rect }, index) => html`<div key=${`edge:${key}:${index}`}
      class=${`overlay-snap-edge${hot?.edge === key ? ' hot' : ''}`} data-snap-zone=${zone} style=${at(rect)}></div>`)}
    ${gaps.map(({ gap, rect }) => html`<div key=${`gap:${gap.path.join()}`}
      class=${`overlay-snap-target gap${same(hot?.gap, gap) ? ' hot' : ''}`} data-snap-gap=${gap.path.join('.')} style=${at(rect)}></div>`)}
    ${markers.map(({ window, zone, rect }) => html`<div key=${`split:${window}:${zone}`}
      class=${`overlay-snap-target split ${zone}${hot?.window === window && hot?.zone === zone ? ' hot' : ''}`} data-snap-window=${window} data-snap-side=${zone} style=${at(rect)}></div>`)}
  </div>`;
}

export function ResizeFrame({ placement, level, className, onResizeStart, onPointerDownCapture }) {
  return html`<div class=${`window-resize-frame ${className}`} aria-hidden="true" onPointerDownCapture=${onPointerDownCapture}
    style=${`left:${placement.x}px;top:${placement.y}px;width:${placement.w}px;height:${placement.h}px;${level}`}>
    ${RESIZE_ZONES.map((zone) => html`<div key=${zone} class=${`window-resize-zone ${zone}`}
      onPointerDown=${(event) => onResizeStart(event, zone)}></div>`)}
  </div>`;
}

// SplitLines are a split's shared edges: each drags to redistribute only its own
// node's children, and a quad's crossing moves both of its lines. A line's detach
// button comes out as the pointer nears the line's centre, so nothing waits over
// the windows' content, drawn last because another line can end on that spot.
// One window draws them all, as they belong to none.
function SplitLines({ overlay }) {
  const { lines, crossings } = overlay.splitLines;
  const above = overlay.linesLevel;
  const crossingOf = (line) => crossings.find((crossing) => crossing.path.join() === line.path.join()) || null;
  const spots = lines.map((line) => ({ line, spot: detachSpot(line, crossingOf(line)) }));
  const [near, setNear] = useState(-1);
  const spotsRef = useRef(spots);
  spotsRef.current = spots;
  useEffect(() => {
    const onMove = (event) => setNear(spotsRef.current.findIndex(({ line, spot }) => {
      const along = line.axis === 'x' ? event.clientY - spot.y : event.clientX - spot.x;
      const across = line.axis === 'x' ? event.clientX - spot.x : event.clientY - spot.y;
      return Math.abs(along) <= DETACH_REACH.along && Math.abs(across) <= DETACH_REACH.across;
    }));
    overlay.win?.addEventListener?.('pointermove', onMove, true);
    return () => overlay.win?.removeEventListener?.('pointermove', onMove, true);
  }, [overlay.win]);
  const { gaps } = overlay.splitLines;
  return html`${gaps.map(({ path, rect }) => html`<div key=${`gap:${path.join()}`} class="overlay-split-gap" aria-hidden="true"
      style=${`left:${rect.x + 4}px;top:${rect.y + 4}px;width:${Math.max(0, rect.w - 8)}px;height:${Math.max(0, rect.h - 8)}px;z-index:${above}`}></div>`)}
    ${spots.map(({ line, spot }, index) => html`<div key=${`${line.path.join()}:${line.axis}`} class=${`overlay-split-line ${line.axis}`} role="separator"
      aria-orientation=${line.axis === 'x' ? 'vertical' : 'horizontal'} style=${`left:${line.x}px;top:${line.y}px;width:${line.w}px;height:${line.h}px;z-index:${above}`}
      onPointerDown=${(event) => overlay.onLinePointerDown(event, [line.axis], line.path, line.area)}></div>`)}
    ${crossings.map((crossing) => html`<div key=${crossing.path.join()} class="overlay-split-cross" aria-hidden="true" style=${`left:${crossing.x}px;top:${crossing.y}px;z-index:${above}`}
      onPointerDown=${(event) => overlay.onLinePointerDown(event, ['x', 'y'], crossing.path, crossing.area)}></div>`)}
    ${spots.map(({ line, spot }, index) => html`<button data-ui-exempt type="button" key=${`detach:${line.path.join()}:${line.axis}`} class=${`overlay-split-detach${near === index ? ' near' : ''}`}
      title="Detach: each side then resizes on its own" aria-label="Unlock this split line" style=${`left:${spot.x}px;top:${spot.y}px;z-index:${above}`}
      onClick=${() => overlay.detachLine(line)}>⇱</button>`)}`;
}

// OverlayWindow draws what useOverlayWindow (the window prop) runs. id names it
// for stacking. mounted keeps a hidden window in the page, for content that must
// stay alive while minimized. lead and title are the title bar's content,
// and controls, { look, closeLabel, closeTitle, onMinimize, onCollab, onClose },
// adds the title bar's buttons, which the classic look leaves to the content. A
// window with nowhere to minimize to leaves out onMinimize, and one that cannot
// go to the Collab tab now leaves out onCollab. A double click on the bar's
// empty space maximizes or restores, whatever the look.
export function OverlayWindow({ id, window: overlay, open = true, mounted = open, className, frameClass, titlebarClass, label, lead = null, title, controls = null, onTitleWheel = null, children }) {
  const { placement, guide } = overlay;
  const front = frontOverlayWindow.value === id ? ' overlay-window-front' : '';
  const level = `--overlay-level:${overlay.level}`;
  const raise = () => raiseOverlayWindow(id);
  const { look, ...buttons } = controls || {};
  const windowControls = look && look !== 'classic' && html`<${WindowControls} look=${look} maximized=${placement.snap === 'max'}
    ...${buttons} onMaximize=${overlay.toggleMaximize} />`;
  return html`
    ${overlay.targets && html`<${SnapTargets} targets=${overlay.targets} />`}
    ${guide && html`<div class="overlay-snap-guide" aria-hidden="true" style=${`left:${guide.x}px;top:${guide.y}px;width:${guide.w}px;height:${guide.h}px`}></div>`}
    ${mounted && html`<section ref=${overlay.windowRef} data-overlay-window=${id}
      class=${`overlay-window ${className}${placement.snap ? ` snapped snapped-${placement.snap}` : ''}${overlay.floating ? ' overlay-window-floating' : ''}${front}`}
      style=${`left:${placement.x}px;top:${placement.y}px;width:${placement.w}px;height:${placement.h}px;${level}`}
      role="dialog" tabindex="-1" aria-label=${label} aria-modal="false" aria-hidden=${open ? undefined : 'true'}
      onClick=${overlay.onWindowClick} onPointerDownCapture=${raise}>
      <header class=${titlebarClass} onPointerDown=${overlay.onTitlePointerDown} onDblClick=${overlay.onTitleDoubleClick} onWheel=${onTitleWheel}>
        ${look === 'macos' ? windowControls : lead}
        ${title}
        ${look !== 'macos' && windowControls}
      </header>
      ${children}
      ${overlay.edges.map((edge) => html`<div key=${edge} class=${`overlay-window-edge ${edge}`} aria-hidden="true"
        onPointerDown=${(event) => overlay.onResizePointerDown(event, edge)}></div>`)}
    </section>`}
    ${open && !placement.snap && html`<${ResizeFrame} placement=${placement} level=${level} className=${`${frameClass}${front}`}
      onResizeStart=${overlay.onResizePointerDown} onPointerDownCapture=${raise} />`}
    ${overlay.splitLines && html`<${SplitLines} overlay=${overlay} />`}`;
}
