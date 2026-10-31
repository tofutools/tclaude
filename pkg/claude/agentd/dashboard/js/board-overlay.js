// board-overlay.js -- a board in a floating window of its own (tcl-h4y60w),
// apart from the companion window, so the operator can work in both at once.
// It floats over every dashboard view, and moves like the companion: drag by
// the title bar, resize from any edge or corner, stay reachable when the browser
// shrinks, and keep its place. It does not snap to edges.
//
// The two share the companion's z-index layer: the one last clicked is on top
// (board-overlay-front on the body, see board.css). Equal layers fall back to
// DOM order, which leaves the companion in front until then.

import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { BoardEditor, BoardsGate } from './board-island.js';
import { boardOverlay, boardOverlayRaise, returnBoardToTab, setBoardOverlayLabel } from './board-overlay-state.js';
import { COMPANION_FOOTER_ROOM, clampPlacement, fitPlacement, parsePlacement, viewportOf } from './companion-model.js';
import { openInCompanion } from './companion-facade.js';
import { isBoardId } from './board-model.js';
import { ResizeFrame, resizedPlacement, savedAfterResize } from './window-resize.js';

const html = htm.bind(h);

export const BOARD_OVERLAY_PLACEMENT_PREF = 'tclaude.dash.board-overlay.placement';
// In session storage, as the companion's open state is: a reload brings the
// window back, another browser tab does not get a second one.
export const BOARD_OVERLAY_OPEN_KEY = 'tclaude.dash.board-overlay.open';
const DEFAULT_W = 760;
const DEFAULT_H = 540;

// The bottom left, clear of the companion's default bottom right.
export function defaultOverlayPlacement(viewport) {
  const w = Math.min(DEFAULT_W, viewport.width - 48);
  const h = Math.min(DEFAULT_H, viewport.height - 140);
  return clampPlacement({ x: 24, y: viewport.height - h - COMPANION_FOOTER_ROOM - 56, w, h, snap: '' }, viewport);
}

function sessionStore(win) {
  try { return win?.sessionStorage ?? null; } catch { return null; }
}

function readOpen(win) {
  try {
    const saved = JSON.parse(sessionStore(win)?.getItem?.(BOARD_OVERLAY_OPEN_KEY) || 'null');
    return isBoardId(saved?.id) ? { id: saved.id, label: typeof saved.label === 'string' ? saved.label : saved.id } : null;
  } catch {
    return null;
  }
}

function BoardOverlayWindow({ board, snapshot, fetchImpl, notify, prefs, win }) {
  const savedPlacement = () => {
    const viewport = viewportOf(win);
    const saved = parsePlacement(prefs?.getItem?.(BOARD_OVERLAY_PLACEMENT_PREF));
    return saved ? fitPlacement({ ...saved, snap: '' }, viewport) : defaultOverlayPlacement(viewport);
  };
  const [placement, setPlacement] = useState(savedPlacement);
  const gesture = useRef(null);

  useEffect(() => {
    const onResize = () => setPlacement(savedPlacement);
    win?.addEventListener?.('resize', onResize);
    return () => {
      win?.removeEventListener?.('resize', onResize);
      gesture.current?.();
    };
  }, [win]);

  function drag(event, next, saved = (p) => p) {
    event.preventDefault();
    const handle = event.currentTarget;
    handle.setPointerCapture?.(event.pointerId);
    const sx = event.clientX;
    const sy = event.clientY;
    let last = placement;
    let moved = false;
    const move = (m) => {
      moved = true;
      last = next(m.clientX - sx, m.clientY - sy, viewportOf(win));
      setPlacement(last);
    };
    const stop = () => {
      handle.removeEventListener('pointermove', move);
      handle.removeEventListener('pointerup', up);
      handle.removeEventListener('pointercancel', up);
      gesture.current = null;
    };
    const up = () => {
      stop();
      // A click shows the fitted placement, which is not the operator's to keep.
      if (!moved) return;
      try { prefs?.setItem?.(BOARD_OVERLAY_PLACEMENT_PREF, JSON.stringify(saved(last))); } catch { /* the window stays where it is */ }
    };
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', up);
    handle.addEventListener('pointercancel', up);
    gesture.current = stop;
  }

  function onTitlePointerDown(event) {
    if (event.button !== 0 || event.target.closest?.('button')) return;
    const start = placement;
    // A move keeps the saved size: the window may only be small for this viewport.
    const size = parsePlacement(prefs?.getItem?.(BOARD_OVERLAY_PLACEMENT_PREF));
    drag(event, (dx, dy, viewport) => clampPlacement({ ...start, x: start.x + dx, y: start.y + dy }, viewport),
      (p) => (size ? { ...p, w: size.w, h: size.h } : p));
  }

  function onResizePointerDown(event, zone) {
    if (event.button !== 0) return;
    event.stopPropagation();
    const start = placement;
    const size = parsePlacement(prefs?.getItem?.(BOARD_OVERLAY_PLACEMENT_PREF));
    drag(event, (dx, dy, viewport) => resizedPlacement(start, zone, dx, dy, viewport),
      (p) => savedAfterResize(p, zone, size));
  }

  return html`<div class="board-overlay" role="dialog" aria-label=${`Board ${board.label}`}
    style=${`left:${placement.x}px;top:${placement.y}px;width:${placement.w}px;height:${placement.h}px`}>
    <header class="board-overlay-titlebar" onPointerDown=${onTitlePointerDown}>
      <span class="board-overlay-title">✎ ${board.label}</span>
      <button type="button" class="board-overlay-action" title="Move this board back to a tab of the Collab view"
        onClick=${() => void returnBoardToTab()}>Back to a tab</button>
      <button type="button" class="board-overlay-close" title="Close this window" aria-label=${`Close ${board.label}`}
        onClick=${() => { boardOverlay.value = null; }}>×</button>
    </header>
    <${BoardEditor} key=${board.id} boardId=${board.id} snapshot=${snapshot} win=${win} fetchImpl=${fetchImpl}
      notify=${notify} onTitle=${(title) => setBoardOverlayLabel(board.id, title)} onOpenAgent=${(agent) => openInCompanion(agent)} />
  </div>
  <${ResizeFrame} placement=${placement} className="board-overlay-resize-frame" onResizeStart=${onResizePointerDown} />`;
}

export function BoardOverlay({ snapshot, ...props }) {
  const board = boardOverlay.value;
  if (!board) return null;
  return html`<${BoardsGate} snapshot=${snapshot}>
    <${BoardOverlayWindow} board=${board} snapshot=${snapshot} ...${props} />
  <//>`;
}

export function mountBoardOverlayIsland({
  host, registerCleanup, snapshot, notify = () => {}, prefs = null,
  fetchImpl = (...args) => globalThis.fetch(...args), win = globalThis.window,
}) {
  const doc = win?.document;
  if (!boardOverlay.peek()) boardOverlay.value = readOpen(win);
  const front = (on) => doc?.body?.classList?.toggle('board-overlay-front', on);
  // Clicking either window brings it forward; clicking the page changes nothing.
  const onPointerDown = (event) => {
    const target = event.target;
    if (target?.closest?.('.board-overlay, .board-overlay-resize-frame')) front(true);
    else if (target?.closest?.('.companion-window, .companion-resize-frame')) front(false);
  };
  doc?.addEventListener?.('pointerdown', onPointerDown, true);
  const stopRaise = boardOverlayRaise.subscribe((request) => { if (request) front(true); });
  const stopSave = boardOverlay.subscribe((board) => {
    try {
      if (board) sessionStore(win)?.setItem?.(BOARD_OVERLAY_OPEN_KEY, JSON.stringify(board));
      else sessionStore(win)?.removeItem?.(BOARD_OVERLAY_OPEN_KEY);
    } catch { /* it just does not come back after a reload */ }
  });
  render(html`<${BoardOverlay} snapshot=${snapshot} fetchImpl=${fetchImpl} notify=${notify} prefs=${prefs} win=${win} />`, host);
  registerCleanup(() => {
    render(null, host);
    stopSave();
    stopRaise();
    doc?.removeEventListener?.('pointerdown', onPointerDown, true);
    doc?.body?.classList?.remove('board-overlay-front');
  });
}
