// board-overlay.js -- boards in floating windows of their own (tcl-h4y60w), so
// the operator can work in several beside a chat. A board window floats over
// every dashboard view, and moves like every overlay window: drag by the title
// bar, resize from any edge or corner, snap to an edge or a corner, split the
// room, maximize, minimize to the dock, stay reachable when the browser shrinks,
// and keep its place. overlay-window.js is the window; this file is the board in
// it, and the host for the windows' dock.

import { h, render } from 'preact';
import { useEffect } from 'preact/hooks';
import htm from 'htm';
import { BoardEditor, BoardsGate } from './board-island.js';
import { onBoardDeleted } from './board-manage.js';
import { BOARD_WINDOW_KIND, boardOverlays, returnBoardToTab, setBoardOverlayLabel } from './board-overlay-state.js';
import { OVERLAY_FOOTER_ROOM, clampPlacement, parsePlacement } from './overlay-window-model.js';
import { companionAgents, companionStatus, decisionsFor, approvalsFor, isAgentId } from './companion-model.js';
import { openInCompanion } from './companion-facade.js';
import { isBoardId } from './board-model.js';
import { windowControlsLook } from './overlay-window-controls.js';
import { closeOverlayWindow, keepOverlayWindows, minimizeOverlayWindow, overlayWindows } from './overlay-instances.js';
import { CHAT_WINDOW_KIND } from './chat-window-model.js';
import { TERMINAL_WINDOW_KIND, validTerminalWindow } from './terminal-window-model.js';
import { OverlayWindow, overlayStackReset, readPref, useOverlayWindow } from './overlay-window.js';

const html = htm.bind(h);

// Where the one board window stood before there were several: what a window for
// a board not yet moved starts from.
export const BOARD_OVERLAY_PLACEMENT_PREF = 'tclaude.dash.board-overlay.placement';
const CASCADE_PX = 28;
const WINDOW_GLYPHS = { [BOARD_WINDOW_KIND]: '✎', [TERMINAL_WINDOW_KIND]: '⌨' };
const DEFAULT_W = 760;
const DEFAULT_H = 540;

// The bottom left, clear of the companion's default bottom right.
export function defaultOverlayPlacement(viewport) {
  const w = Math.min(DEFAULT_W, viewport.width - 48);
  const h = Math.min(DEFAULT_H, viewport.height - 140);
  return clampPlacement({ x: 24, y: viewport.height - h - OVERLAY_FOOTER_ROOM - 56, w, h, snap: '' }, viewport);
}

function BoardOverlayWindow({ instance, index, snapshot, fetchImpl, notify, prefs, win }) {
  const board = { id: instance.board, label: instance.label };
  const open = !instance.minimized;
  // Stepped clear of the windows already open.
  const startPlacement = (viewport) => {
    const base = parsePlacement(readPref(prefs, BOARD_OVERLAY_PLACEMENT_PREF)) || defaultOverlayPlacement(viewport);
    return clampPlacement({ ...base, x: base.x + index * CASCADE_PX, y: base.y + index * CASCADE_PX, snap: '' }, viewport);
  };
  const overlay = useOverlayWindow({ id: instance.id, win, prefs, prefKey: `${BOARD_OVERLAY_PLACEMENT_PREF}.${instance.board}`, defaultPlacement: startPlacement, snaps: true, open });
  const look = windowControlsLook(snapshot.value?.window_controls);
  const close = () => closeOverlayWindow(instance.id);
  useEffect(() => onBoardDeleted((id) => {
    if (id === instance.board) closeOverlayWindow(instance.id);
  }), [instance.id, instance.board]);
  // The title bar is outside the editor, whose Ctrl+wheel zooms the board.
  // Anywhere else in the window that gesture would zoom the whole page.
  function onTitleWheel(event) {
    if (!event.ctrlKey && !event.metaKey) return;
    event.preventDefault();
    const { deltaY, ctrlKey, metaKey } = event;
    overlay.windowRef.current?.querySelector?.('.board-editor')?.dispatchEvent(new win.WheelEvent('wheel', { bubbles: true, cancelable: true, deltaY, ctrlKey, metaKey }));
  }
  const title = html`<span class="board-overlay-title">✎ ${board.label}</span>
    <button type="button" class="board-overlay-action" title="Move this board back to a tab of the Collab view"
      onClick=${() => void returnBoardToTab(board.id)}>Back to a tab</button>
    ${look === 'classic' && html`<button type="button" class="board-overlay-close" title="Close this window" aria-label=${`Close ${board.label}`}
      onClick=${close}>×</button>`}`;
  return html`<${OverlayWindow} id=${instance.id} window=${overlay} open=${open} className="board-overlay" frameClass="board-overlay-resize-frame"
    titlebarClass=${`board-overlay-titlebar overlay-look-${look}`} label=${`Board ${board.label}`} title=${title}
    controls=${{ look, closeLabel: `Close ${board.label}`, closeTitle: 'Close this window', onClose: close, onMinimize: () => minimizeOverlayWindow(instance.id) }} onTitleWheel=${onTitleWheel}>
    <${BoardEditor} key=${board.id} boardId=${board.id} snapshot=${snapshot} win=${win} fetchImpl=${fetchImpl}
      notify=${notify} onTitle=${(title) => setBoardOverlayLabel(board.id, title)} onOpenAgent=${(agent) => openInCompanion(agent)} />
  <//>`;
}

// OverlayDock holds the minimized windows of every kind. A chat that needs the
// operator says so here, as a chip does, so it never looks silently busy.
function OverlayDock({ snapshot }) {
  const minimized = overlayWindows.value.filter((window) => window.minimized);
  if (!minimized.length) return null;
  const agents = companionAgents(snapshot.value);
  const needs = ({ kind, agent }) => {
    const row = kind === CHAT_WINDOW_KIND ? agents.get(agent) : null;
    return !!row && (companionStatus(row) === 'needs' || approvalsFor(snapshot.value, row).length + decisionsFor(snapshot.value, row).length > 0);
  };
  return html`<div class="overlay-dock" role="group" aria-label="Minimized windows">
    ${minimized.map((window) => html`<button type="button" key=${window.id} class=${`overlay-dock-item${needs(window) ? ' needs' : ''}`}
      title=${`Bring back ${window.label}`} onClick=${() => minimizeOverlayWindow(window.id, false)}>${WINDOW_GLYPHS[window.kind] || '▣'} ${window.label}</button>`)}
  </div>`;
}

export function BoardOverlay({ snapshot, ...props }) {
  const boards = boardOverlays.value;
  if (!boards.length && !overlayWindows.value.length) return null;
  return html`<${BoardsGate} snapshot=${snapshot}>
    ${boards.map((instance, index) => html`<${BoardOverlayWindow} key=${instance.id} instance=${instance} index=${index} snapshot=${snapshot} ...${props} />`)}
    <${OverlayDock} snapshot=${snapshot} />
  <//>`;
}

function validWindow(window) {
  if (window.kind === BOARD_WINDOW_KIND) return isBoardId(window.board);
  if (window.kind === TERMINAL_WINDOW_KIND) return validTerminalWindow(window);
  return window.kind === CHAT_WINDOW_KIND && isAgentId(window.agent);
}

export function mountBoardOverlayIsland({
  host, registerCleanup, snapshot, notify = () => {}, prefs = null,
  fetchImpl = (...args) => globalThis.fetch(...args), win = globalThis.window,
}) {
  const stopKeeping = keepOverlayWindows(win, validWindow);
  render(html`<${BoardOverlay} snapshot=${snapshot} fetchImpl=${fetchImpl} notify=${notify} prefs=${prefs} win=${win} />`, host);
  registerCleanup(() => {
    render(null, host);
    stopKeeping();
    overlayStackReset();
  });
}
