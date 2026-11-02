// board-overlay.js -- boards in floating windows of their own (tcl-h4y60w), so
// the operator can work in several beside a chat. A board window floats over
// every dashboard view, and moves like every overlay window: drag by the title
// bar, resize from any edge or corner, snap to an edge or a corner, split the
// room, maximize, minimize to the dock, stay reachable when the browser shrinks,
// and keep its place. overlay-window.js is the window; this file is the board in
// it, which overlay-tab-window.js shows, and the host for the windows' dock and
// the saved set of windows.

import { h, render } from 'preact';
import { useEffect } from 'preact/hooks';
import htm from 'htm';
import { BoardEditor, BoardsGate } from './board-island.js';
import { onBoardDeleted } from './board-manage.js';
import { BOARD_WINDOW_KIND, returnBoardToTab, setBoardOverlayLabel } from './board-overlay-state.js';
import { boardPaneSeed, isBoardId } from './board-model.js';
import { OVERLAY_FOOTER_ROOM, clampPlacement, parsePlacement } from './overlay-window-model.js';
import { companionAgents, companionStatus, decisionsFor, approvalsFor, isAgentId } from './companion-model.js';
import { openInCompanion } from './companion-facade.js';
import { windowControlsLook } from './overlay-window-controls.js';
import { closeOverlayTab, focusOverlayWindow, keepOverlayWindows, minimizeOverlayWindow, minimizedOverlayTabs, openOverlayWindow, overlayWindows } from './overlay-instances.js';
import { CHAT_WINDOW_KIND } from './chat-window-model.js';
import { TERMINAL_WINDOW_KIND, validTerminalWindow } from './terminal-window-model.js';
import { OverlayWindow, overlayStackReset, readPref } from './overlay-window.js';
import { BOARD_OVERLAY_PLACEMENT_PREF, dockEntriesOf } from './overlay-tab-model.js';
import { elementScreenPoint } from './terminal-drag-out.js';
import { detachSeedWindow } from './terminals-tab.js';

const html = htm.bind(h);

// BOARD_OVERLAY_PLACEMENT_PREF alone is where the one board window stood before
// there were several: what a window for a board not yet moved starts from.
export { BOARD_OVERLAY_PLACEMENT_PREF };
const CASCADE_PX = 28;
export const WINDOW_GLYPHS = { [BOARD_WINDOW_KIND]: '✎', [TERMINAL_WINDOW_KIND]: '⌨' };
const DEFAULT_W = 760;
const DEFAULT_H = 540;

// The bottom left, clear of the companion's default bottom right.
export function defaultOverlayPlacement(viewport) {
  const w = Math.min(DEFAULT_W, viewport.width - 48);
  const h = Math.min(DEFAULT_H, viewport.height - 140);
  return clampPlacement({ x: 24, y: viewport.height - h - OVERLAY_FOOTER_ROOM - 56, w, h, snap: '' }, viewport);
}

// boardStartPlacement is where the index-th board window opens, stepped clear of
// the windows already open.
export function boardStartPlacement(prefs, index) {
  return (viewport) => {
    const base = parsePlacement(readPref(prefs, BOARD_OVERLAY_PLACEMENT_PREF)) || defaultOverlayPlacement(viewport);
    return clampPlacement({ ...base, x: base.x + index * CASCADE_PX, y: base.y + index * CASCADE_PX, snap: '' }, viewport);
  };
}

export function BoardOverlayWindow({ instance, overlay, strip, close, snapshot, fetchImpl, notify, win }) {
  const board = { id: instance.board, label: instance.label };
  const open = !instance.minimized;
  const look = windowControlsLook(snapshot.value?.window_controls);
  const leave = () => closeOverlayTab(instance.id);
  // A blocked pop-up leaves it where it is.
  const toSeparateWindow = () => {
    const rect = overlay.windowRef.current?.getBoundingClientRect?.();
    if (detachSeedWindow({ ...boardPaneSeed(board.id, board.label), floats: true }, { size: rect, at: elementScreenPoint(win, rect) })) leave();
    else notify('Could not open a separate window: allow pop-ups for this dashboard.', true);
  };
  useEffect(() => onBoardDeleted((id) => {
    if (id === instance.board) leave();
  }), [instance.id, instance.board]);
  // The title bar is outside the editor, whose Ctrl+wheel zooms the board.
  // Anywhere else in the window that gesture would zoom the whole page.
  function onTitleWheel(event) {
    if (!event.ctrlKey && !event.metaKey) return;
    event.preventDefault();
    const { deltaY, ctrlKey, metaKey } = event;
    overlay.windowRef.current?.querySelector?.('.board-editor')?.dispatchEvent(new win.WheelEvent('wheel', { bubbles: true, cancelable: true, deltaY, ctrlKey, metaKey }));
  }
  const title = html`${strip}
    <button type="button" class="board-overlay-action" title="Move this board back to a tab of the Collab view"
      onClick=${() => void returnBoardToTab(board.id)}>Back to a tab</button>
    <button type="button" class="board-overlay-action" title="Move this board to a browser window of its own" onClick=${toSeparateWindow}>Open in separate window</button>
    ${look === 'classic' && html`<button type="button" class="board-overlay-close" title=${close.title} aria-label=${`Close ${board.label}`}
      onClick=${close.onClose}>×</button>`}`;
  return html`<${OverlayWindow} id=${instance.windowId} window=${overlay} open=${open} className="board-overlay" frameClass="board-overlay-resize-frame"
    titlebarClass=${`board-overlay-titlebar overlay-look-${look}`} label=${`Board ${board.label}`} title=${title}
    controls=${{ look, closeLabel: `Close ${board.label}`, closeTitle: close.title, onClose: close.onClose, onMinimize: () => minimizeOverlayWindow(instance.id) }} onTitleWheel=${onTitleWheel}>
    <${BoardEditor} key=${board.id} boardId=${board.id} snapshot=${snapshot} win=${win} fetchImpl=${fetchImpl}
      notify=${notify} onTitle=${(title) => setBoardOverlayLabel(board.id, title)} onOpenAgent=${(agent) => openInCompanion(agent)} />
  <//>`;
}

// What taking a pill off for good does, by kind. A Steward has none: only its ⚙
// shuts it down.
const REMOVE_TITLES = {
  [CHAT_WINDOW_KIND]: ['Unpin', 'Unpin (the agent keeps running)'],
  [TERMINAL_WINDOW_KIND]: ['Close', 'Close (the shell keeps running)'],
  [BOARD_WINDOW_KIND]: ['Close', 'Close this board\u2019s window'],
};

// OverlayDock holds the minimized windows and tabs of every kind: a window shows
// its active tab and how many it holds, and a tab comes back from here to the
// window it left. A chat that needs the operator says so here, as a chip does, so
// it never looks silently busy.
function OverlayDock({ snapshot }) {
  const entries = dockEntriesOf(overlayWindows.value, minimizedOverlayTabs.value);
  if (!entries.length) return null;
  const agents = companionAgents(snapshot.value);
  const needs = ({ kind, agent }) => {
    const row = kind === CHAT_WINDOW_KIND ? agents.get(agent) : null;
    return !!row && (companionStatus(row) === 'needs' || approvalsFor(snapshot.value, row).length + decisionsFor(snapshot.value, row).length > 0);
  };
  return html`<div class="overlay-dock" role="group" aria-label="Minimized windows">
    ${entries.map(({ key, tab, tabs }) => {
      const removes = tabs.length === 1 && !(tab.kind === CHAT_WINDOW_KIND && agents.get(tab.agent)?.steward) && REMOVE_TITLES[tab.kind];
      return html`<span key=${key} class=${`overlay-dock-pill${tabs.some(needs) ? ' needs' : ''}`}>
        <button type="button" class="overlay-dock-item" title=${`Bring back ${tab.label}`}
          onClick=${() => { openOverlayWindow(tab); focusOverlayWindow(tab.id); }}>${WINDOW_GLYPHS[tab.kind] || '▣'} ${tab.label}${tabs.length > 1 && html` <span class="overlay-dock-count" aria-label=${`${tabs.length} tabs`}>${tabs.length}</span>`}</button>
        ${removes && html`<button type="button" class="overlay-dock-remove" aria-label=${`${removes[0]} ${tab.label}`} title=${removes[1]} onClick=${() => closeOverlayTab(tab.id)}>×</button>`}
      </span>`;
    })}
  </div>`;
}

export function BoardOverlay({ snapshot }) {
  if (!overlayWindows.value.length && !minimizedOverlayTabs.value.length) return null;
  return html`<${BoardsGate} snapshot=${snapshot}><${OverlayDock} snapshot=${snapshot} /><//>`;
}

function validWindow(window) {
  if (window.kind === BOARD_WINDOW_KIND) return isBoardId(window.board);
  if (window.kind === TERMINAL_WINDOW_KIND) return validTerminalWindow(window);
  return window.kind === CHAT_WINDOW_KIND && isAgentId(window.agent);
}

export function mountBoardOverlayIsland({ host, registerCleanup, snapshot, win = globalThis.window }) {
  const stopKeeping = keepOverlayWindows(win, validWindow);
  render(html`<${BoardOverlay} snapshot=${snapshot} />`, host);
  registerCleanup(() => {
    render(null, host);
    stopKeeping();
    overlayStackReset();
  });
}
