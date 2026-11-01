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
  clampPlacement, fitPlacement, freeEdges, parsePlacement, resizedPlacement, savedAfterResize, snappedResize, snapZone, viewportOf, zoneRect,
} from './overlay-window-model.js';
import {
  coveringPath, detachSpot, dropZone, fits, keepingWindows, leavesOf, movedEdge, movedLine, nodeAt, parseSplit, rectZone, roomPair, roomTree, splitEdges, splitLayout, unlockedLine, withWindow, withinGap, withoutWindow,
} from './overlay-split-model.js';
import { WindowControls } from './overlay-window-controls.js';

const html = htm.bind(h);

// Corners come last: they are drawn over the edges they touch.
const RESIZE_ZONES = ['n', 's', 'e', 'w', 'nw', 'ne', 'sw', 'se'];
const TEAR_OFF_PX = 4;

// The ids of the windows, front last. A window joins as it mounts: floating at
// the front, snapped at the back, so a reload does not bury one behind a split.
// Levels come from it: every window is below what sits at 950, such as agent
// marks. A split moves as one, with its front window one above the rest, for
// when an unlocked line lets two overlap, and its lines above that: anything over
// the split covers them. Past the bottom level, windows share it and DOM order
// decides.
const OVERLAY_TOP_LEVEL = 947;
export const OVERLAY_BOTTOM_LEVEL = 930;
const overlayStack = signal([]);
export const frontOverlayWindow = computed(() => overlayStack.value[overlayStack.value.length - 1] || '');

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

// The saved tree of splits. It keeps the place of a window that is away.
const splitTree = signal(null);
// Every snapped window showing now: id -> { zone, rect(), release() }.
const snappedWindows = signal(new Map());
// The part of the tree showing now, or null.
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

// forgetOverlayWindow takes a closed window out of the stack and any split.
export function forgetOverlayWindow(id) {
  overlayStack.value = overlayStack.peek().filter((other) => other !== id);
  if (leavesOf(splitTree.peek()).includes(id)) saveSplit(splitLoadedFrom, withoutWindow(splitTree.peek(), id));
}

function loadSplit(prefs) {
  if (splitLoadedFrom === prefs) return;
  splitLoadedFrom = prefs;
  splitTree.value = parseSplit(readPref(prefs, OVERLAY_SPLIT_PREF));
}

function saveSplit(prefs, tree) {
  splitTree.value = tree;
  writePref(prefs, OVERLAY_SPLIT_PREF, tree ? JSON.stringify(tree) : '');
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
export function useOverlayWindow({ id = '', win, prefs, prefKey, defaultPlacement, snaps = false, open = true, spotted = null, onPlaced = null, onRefocus = null }) {
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
    if (!joins) return undefined;
    showWindow(id, { zone: joins, rect: () => shownRef.current, pin: () => pinPanelSize() });
    return () => hideWindow(id);
  }, [joins]);

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

  // A window out of a split goes back to its panel, so that takes the size it
  // has now. An unlocked window stays that size when the split later ends.
  function pinPanelSize() {
    const { snap, w, h } = shownRef.current;
    const current = saved();
    if (!snap || !current) return;
    savePlacement({ ...current, panels: { ...current.panels, [snap]: { w, h } } });
    setPlacement(snappedPlacement(snap));
  }

  // Where a drop at pointer lands: { zone, tree?, guide }, or null to float. In
  // a split's empty cell, the window takes it. On a window in a split or one
  // snapped alone, the drop splits that window. At the room's edge, windows
  // snapped alone make a tree together, each keeping its kind of snap. Otherwise
  // it just snaps.
  function dropPlan(raw) {
    const viewport = viewportOf(win);
    const room = snapZone(raw, viewport, snapTop());
    const bounds = splitArea();
    // The room's edge reaches past it, over the footer.
    const pointer = { x: Math.max(bounds.x, Math.min(bounds.x + bounds.w - 1, raw.x)), y: Math.max(bounds.y, Math.min(bounds.y + bounds.h - 1, raw.y)) };
    const plain = room ? { zone: room, guide: snappedPlacement(room) } : null;
    const savedTree = splitTree.peek();
    const others = [...snappedWindows.peek()].filter(([other]) => other !== id);
    const here = new Set([...others.map(([other]) => other), id]);
    const planned = (tree, zone = '') => {
      const next = tree && layoutOf(keepingWindows(tree, here));
      const rect = next?.rects[id];
      return rect ? { zone: zone === 'top' ? 'top-left' : zone || rectZone(rect, bounds), tree, guide: rect } : null;
    };
    const live = liveSplit.peek();
    // A gap is a path in the showing tree, which is the saved one with nobody away.
    if (live && live === savedTree) {
      const inside = ({ rect }) => pointer.x >= rect.x && pointer.x < rect.x + rect.w && pointer.y >= rect.y && pointer.y < rect.y + rect.h;
      const gap = layoutOf(live)?.gaps.find(inside);
      if (gap) return planned(withinGap(savedTree, gap, pointer, id, room)) || plain;
    }
    const inTree = (other) => leavesOf(savedTree).includes(other);
    // A window outside the split cannot start a tree of its own: there is one.
    const targets = others.filter(([other]) => !live || inTree(other));
    if (!room || live) {
      for (const [other, { rect }] of targets) {
        const zone = dropZone(pointer, rect());
        if (zone) return planned(withWindow(inTree(other) ? savedTree : { id: other }, other, zone, id), zone) || plain;
      }
    }
    if (live || !room) return plain;
    for (const [other, { zone }] of targets.filter(([other]) => inTree(other))) {
      if (roomPair(other, room, id, zone)) return planned(withWindow(savedTree, other, room, id), room) || plain;
    }
    const alone = targets.filter(([other]) => !inTree(other)).map(([other, { zone }]) => [other, zone]);
    return (alone.length && planned(roomTree([...alone, [id, room]]), room)) || plain;
  }

  function track(event, onMove, onEnd) {
    const handle = event.currentTarget;
    handle.setPointerCapture?.(event.pointerId);
    const stop = () => {
      handle.removeEventListener('pointermove', onMove);
      handle.removeEventListener('pointerup', up);
      handle.removeEventListener('pointercancel', up);
      gesture.current = null;
    };
    const up = () => {
      stop();
      onEnd();
    };
    handle.addEventListener('pointermove', onMove);
    handle.addEventListener('pointerup', up);
    handle.addEventListener('pointercancel', up);
    gesture.current = stop;
  }

  function onTitlePointerDown(event) {
    if (event.button !== 0 || event.target.closest?.('button, input, [role="tab"]')) return;
    if (!windowRef.current) return;
    event.preventDefault();
    let start = placement;
    const sx = event.clientX;
    const sy = event.clientY;
    let plan = null;
    let last = start;
    let moved = false;
    track(event, (m) => {
      if (start.snap) {
        // A click leaves a snapped window alone. Tearing it off takes a real
        // move, and restores a floating size under the pointer.
        if (Math.hypot(m.clientX - sx, m.clientY - sy) < TEAR_OFF_PX) return;
        const viewport = viewportOf(win);
        const floating = clampPlacement({ ...defaultPlacement(viewport), ...(saved() || {}), snap: '' }, viewport);
        start = { ...floating, x: sx - floating.w / 2, y: sy - 14, snap: '' };
        if (splitTree.peek() && leavesOf(splitTree.peek()).includes(id)) saveSplit(prefs, withoutWindow(splitTree.peek(), id));
      }
      moved = true;
      last = clampPlacement({ ...start, x: start.x + m.clientX - sx, y: start.y + m.clientY - sy, snap: '' }, viewportOf(win));
      setPlacement(last);
      if (!snaps) return;
      plan = dropPlan({ x: m.clientX, y: m.clientY });
      setGuide(plan?.guide || null);
    }, () => {
      setGuide(null);
      if (plan) {
        setPlacement(snappedPlacement(plan.zone));
        saveMovedWindow({ ...withSavedSize(last), snap: plan.zone });
        if (plan.tree) saveSplit(prefs, plan.tree);
      } else if (moved) {
        // A click shows the fitted placement, which is not the operator's to keep.
        saveMovedWindow(withSavedSize(last));
      }
    });
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

  // The saved node a path of the showing tree names. With nobody away they are
  // one tree, and the path says it exactly.
  function savedPathOf(path) {
    const live = liveSplit.peek();
    return live === splitTree.peek() ? path : coveringPath(splitTree.peek(), leavesOf(nodeAt(live, path)));
  }

  // A line moves live and is saved when it lands. It names a node of the showing
  // tree, and moves that split in the saved one, which keeps windows that are away.
  function onLinePointerDown(event, axes, path, area) {
    if (event.button !== 0) return;
    event.preventDefault();
    event.stopPropagation();
    const savedPath = savedPathOf(path);
    track(event, (m) => {
      const pointer = { x: m.clientX, y: m.clientY };
      splitTree.value = axes.reduce((tree, axis) => movedLine(tree, savedPath, axis, pointer, area), splitTree.peek());
    }, () => saveSplit(prefs, splitTree.peek()));
  }

  // A window's edge an unlocked line left it moves live, like a line.
  function resizeSplitEdges(event, zone) {
    const moves = [...zone].map((side) => layout.edges[id]?.[side]).filter(Boolean)
      .map((edge) => ({ edge, path: savedPathOf(edge.path) }));
    track(event, (m) => {
      const pointer = { x: m.clientX, y: m.clientY };
      splitTree.value = moves.reduce((tree, { edge, path }) => movedEdge(tree, path, edge, pointer), splitTree.peek());
    }, () => {
      saveSplit(prefs, splitTree.peek());
      pinPanelSize();
    });
  }

  // Detach unlocks one line. The windows stay snapped where they are, and each
  // side then resizes on its own: a space or an overlap can open, with no line.
  function detachLine({ path, axis, area }) {
    const savedPath = savedPathOf(path);
    for (const member of leavesOf(nodeAt(liveSplit.peek(), path))) snappedWindows.peek().get(member)?.pin();
    saveSplit(prefs, unlockedLine(splitTree.peek(), savedPath, axis, area));
  }

  // A mouse press leaves focus on the control, where the next key meant for the
  // window's content would press it again. Keyboard activation (detail 0) stays.
  function releaseFocus(event) {
    if (!event?.detail) return;
    event.currentTarget?.blur?.();
    onRefocus?.();
  }

  // A click on the title bar or other empty space leaves focus on the page.
  // Give it back to the content, so typing works and the border returns.
  // Controls keep their click, which may have opened a dialog, and a text
  // selection survives so it can be copied.
  function onWindowClick(event) {
    if (!onRefocus) return;
    if (event.target.closest?.('button, a, input, textarea, select, label')) return;
    if (win?.getSelection?.()?.isCollapsed === false) return;
    const active = win?.document?.activeElement;
    if (!active || active === win.document.body) onRefocus();
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
  return { placement: shown, edges, level: unitLevel + (splitFront ? 1 : 0), linesLevel: unitLevel + 2, splitLines, onLinePointerDown, detachLine, win, guide, windowRef, openFrom, onTitlePointerDown, onResizePointerDown, onWindowClick, releaseFocus, toggleMaximize, shrinkTo };
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
  return html`${spots.map(({ line, spot }, index) => html`<div key=${`${line.path.join()}:${line.axis}`} class=${`overlay-split-line ${line.axis}`} role="separator"
      aria-orientation=${line.axis === 'x' ? 'vertical' : 'horizontal'} style=${`left:${line.x}px;top:${line.y}px;width:${line.w}px;height:${line.h}px;z-index:${above}`}
      onPointerDown=${(event) => overlay.onLinePointerDown(event, [line.axis], line.path, line.area)}></div>`)}
    ${crossings.map((crossing) => html`<div key=${crossing.path.join()} class="overlay-split-cross" aria-hidden="true" style=${`left:${crossing.x}px;top:${crossing.y}px;z-index:${above}`}
      onPointerDown=${(event) => overlay.onLinePointerDown(event, ['x', 'y'], crossing.path, crossing.area)}></div>`)}
    ${spots.map(({ line, spot }, index) => html`<button type="button" key=${`detach:${line.path.join()}:${line.axis}`} class=${`overlay-split-detach${near === index ? ' near' : ''}`}
      title="Detach: each side then resizes on its own" aria-label="Unlock this split line" style=${`left:${spot.x}px;top:${spot.y}px;z-index:${above}`}
      onClick=${() => overlay.detachLine(line)}>⇱</button>`)}`;
}

// OverlayWindow draws what useOverlayWindow (the window prop) runs. id names it
// for stacking. mounted keeps a hidden window in the page, for content that must
// stay alive while minimized. lead and title are the title bar's content,
// and controls, { look, closeLabel, closeTitle, onMinimize, onClose }, adds the
// native buttons, which the classic look leaves to the content. A window with
// nowhere to minimize to leaves out onMinimize.
export function OverlayWindow({ id, window: overlay, open = true, mounted = open, className, frameClass, titlebarClass, label, lead = null, title, controls = null, onTitleWheel = null, children }) {
  const { placement, guide } = overlay;
  const front = frontOverlayWindow.value === id ? ' overlay-window-front' : '';
  const level = `--overlay-level:${overlay.level}`;
  const raise = () => raiseOverlayWindow(id);
  const { look, ...buttons } = controls || {};
  const windowControls = look && look !== 'classic' && html`<${WindowControls} look=${look} maximized=${placement.snap === 'max'}
    ...${buttons} onMaximize=${overlay.toggleMaximize} />`;
  return html`
    ${guide && html`<div class="overlay-snap-guide" aria-hidden="true" style=${`left:${guide.x}px;top:${guide.y}px;width:${guide.w}px;height:${guide.h}px`}></div>`}
    ${mounted && html`<section ref=${overlay.windowRef}
      class=${`overlay-window ${className}${placement.snap ? ` snapped snapped-${placement.snap}` : ''}${front}`}
      style=${`left:${placement.x}px;top:${placement.y}px;width:${placement.w}px;height:${placement.h}px;${level}`}
      role="dialog" aria-label=${label} aria-modal="false" aria-hidden=${open ? undefined : 'true'}
      onClick=${overlay.onWindowClick} onPointerDownCapture=${raise}>
      <header class=${titlebarClass} onPointerDown=${overlay.onTitlePointerDown} onWheel=${onTitleWheel}>
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
