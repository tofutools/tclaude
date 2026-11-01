// board-overlay.js -- a board in a floating window of its own (tcl-h4y60w),
// apart from the companion window, so the operator can work in both at once.
// It floats over every dashboard view, and moves like the companion: drag by
// the title bar, resize from any edge or corner, stay reachable when the browser
// shrinks, and keep its place. It does not snap to edges. overlay-window.js is
// the window; this file is the board in it.

import { h, render } from 'preact';
import htm from 'htm';
import { BoardEditor, BoardsGate } from './board-island.js';
import { boardOverlay, boardOverlayRaise, returnBoardToTab, setBoardOverlayLabel } from './board-overlay-state.js';
import { OVERLAY_FOOTER_ROOM, clampPlacement } from './overlay-window-model.js';
import { openInCompanion } from './companion-facade.js';
import { isBoardId } from './board-model.js';
import { OverlayWindow, frontOverlayWindow, raiseOverlayWindow, useOverlayWindow } from './overlay-window.js';

const html = htm.bind(h);

export const BOARD_OVERLAY_PLACEMENT_PREF = 'tclaude.dash.board-overlay.placement';
// In session storage, as the companion's open state is: a reload brings the
// window back, another browser tab does not get a second one.
export const BOARD_OVERLAY_OPEN_KEY = 'tclaude.dash.board-overlay.open';
const OVERLAY_ID = 'board';
const DEFAULT_W = 760;
const DEFAULT_H = 540;

// The bottom left, clear of the companion's default bottom right.
export function defaultOverlayPlacement(viewport) {
  const w = Math.min(DEFAULT_W, viewport.width - 48);
  const h = Math.min(DEFAULT_H, viewport.height - 140);
  return clampPlacement({ x: 24, y: viewport.height - h - OVERLAY_FOOTER_ROOM - 56, w, h, snap: '' }, viewport);
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
  const overlay = useOverlayWindow({ win, prefs, prefKey: BOARD_OVERLAY_PLACEMENT_PREF, defaultPlacement: defaultOverlayPlacement });
  const title = html`<span class="board-overlay-title">✎ ${board.label}</span>
    <button type="button" class="board-overlay-action" title="Move this board back to a tab of the Collab view"
      onClick=${() => void returnBoardToTab()}>Back to a tab</button>
    <button type="button" class="board-overlay-close" title="Close this window" aria-label=${`Close ${board.label}`}
      onClick=${() => { boardOverlay.value = null; }}>×</button>`;
  return html`<${OverlayWindow} id=${OVERLAY_ID} window=${overlay} className="board-overlay" frameClass="board-overlay-resize-frame"
    titlebarClass="board-overlay-titlebar" label=${`Board ${board.label}`} title=${title}>
    <${BoardEditor} key=${board.id} boardId=${board.id} snapshot=${snapshot} win=${win} fetchImpl=${fetchImpl}
      notify=${notify} onTitle=${(title) => setBoardOverlayLabel(board.id, title)} onOpenAgent=${(agent) => openInCompanion(agent)} />
  <//>`;
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
  if (!boardOverlay.peek()) boardOverlay.value = readOpen(win);
  const stopRaise = boardOverlayRaise.subscribe((request) => { if (request) raiseOverlayWindow(OVERLAY_ID); });
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
    if (frontOverlayWindow.peek() === OVERLAY_ID) raiseOverlayWindow('');
  });
}
