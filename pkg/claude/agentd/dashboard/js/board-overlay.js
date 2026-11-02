// board-overlay.js -- boards in floating windows of their own (tcl-h4y60w), so
// the operator can work in several beside a chat. A board window floats over
// every dashboard view, and moves like every overlay window: drag by the title
// bar, resize from any edge or corner, snap to an edge or a corner, split the
// room, maximize, minimize to the dock, stay reachable when the browser shrinks,
// and keep its place. overlay-window.js is the window; this file is the board in
// it, which overlay-tab-window.js shows, and the host for the windows' dock and
// the saved set of windows.

import { h, render } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { BoardEditor, BoardsGate } from './board-island.js';
import { onBoardDeleted } from './board-manage.js';
import { BOARD_WINDOW_KIND, returnBoardToTab, setBoardOverlayLabel } from './board-overlay-state.js';
import { boardPaneSeed, isBoardId } from './board-model.js';
import { OVERLAY_FOOTER_ROOM, clampPlacement, parsePlacement } from './overlay-window-model.js';
import { companionAgents, companionStatus, decisionsFor, approvalsFor, isAgentId, stewardAgent } from './companion-model.js';
import { endCompanionPeek, openInCompanion, peekInCompanion } from './companion-facade.js';
import { windowControlsLook } from './overlay-window-controls.js';
import { closeOverlayTab, focusOverlayWindow, keepOverlayWindows, minimizeOverlayWindow, minimizedOverlayTabs, openOverlayWindow, overlayHostUp, overlayWindows } from './overlay-instances.js';
import { CHAT_WINDOW_KIND, DECIDE_WINDOW_KIND, START_WINDOW_KIND } from './chat-window-model.js';
import { TERMINAL_WINDOW_KIND, validTerminalWindow } from './terminal-window-model.js';
import { OverlayWindow, overlayStackReset, readPref } from './overlay-window.js';
import { BOARD_OVERLAY_PLACEMENT_PREF, dockEntriesOf } from './overlay-tab-model.js';
import { elementScreenPoint } from './terminal-drag-out.js';
import { detachSeedWindow } from './terminals-tab.js';
import { POPUP_BLOCKED, TO_COLLAB_LABEL, TO_WINDOW_LABEL, registerTabMoves, tabMoves } from './overlay-tab-moves.js';
import { buildOverlayBulkCommands, overlayBulkActions } from './overlay-window-bulk.js';
import { registerCommandProvider } from './command-registry.js';

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

// A board goes back to its tab of the Collab view, or out to a window of its own;
// a blocked pop-up leaves it where it is.
registerTabMoves(BOARD_WINDOW_KIND, (tab) => ({
  toCollab: () => void returnBoardToTab(tab.board),
  toWindow: ({ rect, win, notify }) => {
    if (detachSeedWindow({ ...boardPaneSeed(tab.board, tab.label), floats: true }, { size: rect, at: elementScreenPoint(win, rect) })) closeOverlayTab(tab.id);
    else notify?.(POPUP_BLOCKED, true);
  },
}));

export function BoardOverlayWindow({ instance, overlay, strip, close, snapshot, fetchImpl, notify, win }) {
  const board = { id: instance.board, label: instance.label };
  const open = !instance.minimized;
  const look = windowControlsLook(snapshot.value?.window_controls);
  const leave = () => closeOverlayTab(instance.id);
  const { toCollab, toWindow } = tabMoves(instance);
  const toSeparateWindow = () => toWindow({ rect: overlay.windowRef.current?.getBoundingClientRect?.(), win, notify });
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
    <button type="button" class="board-overlay-action" title="Move this board to a tab of the Collab view" onClick=${toCollab}>${TO_COLLAB_LABEL}</button>
    <button type="button" class="board-overlay-action" title="Move this board to a browser window of its own" onClick=${toSeparateWindow}>${TO_WINDOW_LABEL}</button>
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
  const agents = companionAgents(snapshot.value);
  // The chip stands for the seated Steward's window, and for the page summoning one.
  const seated = stewardAgent(agents)?.agentId;
  const onChip = ({ kind, agent }) => kind === START_WINDOW_KIND || (kind === CHAT_WINDOW_KIND && agent === seated);
  const entries = dockEntriesOf(overlayWindows.value, minimizedOverlayTabs.value).filter(({ tabs }) => !(overlayHostUp.value && (tabs.every(onChip) || tabs.some(({ kind, agent }) => kind === CHAT_WINDOW_KIND && agent === seated))));
  const actions = overlayBulkActions.value;
  const [menuAt, setMenuAt] = useState(null);
  const closeMenu = useCallback(() => setMenuAt(null), []);
  if (!entries.length && !actions.length) return null;
  const needs = ({ kind, agent }) => {
    const row = kind === CHAT_WINDOW_KIND ? agents.get(agent) : null;
    return !!row && (companionStatus(row) === 'needs' || approvalsFor(snapshot.value, row).length + decisionsFor(snapshot.value, row).length > 0);
  };
  return html`<div class="overlay-dock" role="group" aria-label="Minimized windows">
    ${actions.length > 0 && html`<${OverlayDockMenu} open=${!!menuAt} onOpen=${setMenuAt} />`}
    ${entries.map(({ key, tab, tabs }) => {
      const removes = tabs.length === 1 && !onChip(tab) && tab.kind !== DECIDE_WINDOW_KIND && REMOVE_TITLES[tab.kind];
      const peeks = tab.kind === CHAT_WINDOW_KIND ? {
        onMouseEnter: (event) => peekInCompanion(tab.agent, event.currentTarget),
        onMouseLeave: endCompanionPeek,
        onFocus: (event) => peekInCompanion(tab.agent, event.currentTarget),
        onBlur: endCompanionPeek,
      } : null;
      return html`<span key=${key} class=${`overlay-dock-pill${tabs.some(needs) ? ' needs' : ''}`}>
        <button type="button" class="overlay-dock-item" title=${`Bring back ${tab.label}`} ...${peeks}
          onClick=${() => { endCompanionPeek(); openOverlayWindow(tab); focusOverlayWindow(tab.id); }}>${WINDOW_GLYPHS[tab.kind] || '▣'} ${tab.label}${tabs.length > 1 && html` <span class="overlay-dock-count" aria-label=${`${tabs.length} tabs`}>${tabs.length}</span>`}</button>
        ${removes && html`<button type="button" class="overlay-dock-remove" aria-label=${`${removes[0]} ${tab.label}`} title=${removes[1]} onClick=${() => closeOverlayTab(tab.id)}>×</button>`}
      </span>`;
    })}
  </div>
  ${menuAt && actions.length > 0 && html`<${OverlayDockActions} at=${menuAt} actions=${actions} onClose=${closeMenu} />`}`;
}

// OverlayDockMenu is the dock's first button: actions on every window at once,
// in the palette's words. Where its menu opens, from the button, is up to it.
function OverlayDockMenu({ open, onOpen }) {
  return html`<span class="overlay-dock-menu">
    <button type="button" class="overlay-dock-menu-button" aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
      title="All windows" aria-label="All windows" onClick=${(event) => {
        if (open) { onOpen(null); return; }
        const rect = event.currentTarget.getBoundingClientRect();
        const height = event.currentTarget.ownerDocument.defaultView?.innerHeight || 0;
        onOpen({ left: rect.left, bottom: height - rect.top + 6 });
      }}>☰</button>
  </span>`;
}

// OverlayDockActions is that menu. It renders beside the dock, not in it: the
// dock's transform would place a fixed menu by the dock, and its scrolling clip it.
function OverlayDockActions({ at, actions, onClose }) {
  const root = useRef(null);
  useEffect(() => {
    const doc = root.current?.ownerDocument;
    const away = (event) => { if (!event.target?.closest?.('.overlay-dock-menu, .overlay-dock-actions')) onClose(); };
    const key = (event) => { if (event.key === 'Escape') onClose(); };
    doc?.addEventListener('pointerdown', away, true);
    doc?.addEventListener('keydown', key, true);
    return () => {
      doc?.removeEventListener('pointerdown', away, true);
      doc?.removeEventListener('keydown', key, true);
    };
  }, [onClose]);
  return html`<div ref=${root} class="overlay-dock-actions" role="menu" style=${`left:${at.left}px;bottom:${at.bottom}px`}>
    ${actions.map(({ key, icon, label, hint, run }) => html`<button key=${key} type="button" role="menuitem" class=${`overlay-dock-action overlay-dock-action-${key}`}
      title=${hint} onClick=${() => { onClose(); run(); }}>${icon} ${label}</button>`)}
  </div>`;
}

export function BoardOverlay({ snapshot }) {
  if (!overlayWindows.value.length && !minimizedOverlayTabs.value.length) return null;
  return html`<${BoardsGate} snapshot=${snapshot}><${OverlayDock} snapshot=${snapshot} /><//>`;
}

function validWindow(window) {
  if (window.kind === BOARD_WINDOW_KIND) return isBoardId(window.board);
  if (window.kind === TERMINAL_WINDOW_KIND) return validTerminalWindow(window);
  if (window.kind === START_WINDOW_KIND || window.kind === DECIDE_WINDOW_KIND) return true;
  return window.kind === CHAT_WINDOW_KIND && isAgentId(window.agent);
}

export function mountBoardOverlayIsland({ host, registerCleanup, snapshot, win = globalThis.window }) {
  const stopKeeping = keepOverlayWindows(win, validWindow);
  const stopCommands = registerCommandProvider('overlay-windows', buildOverlayBulkCommands);
  render(html`<${BoardOverlay} snapshot=${snapshot} />`, host);
  registerCleanup(() => {
    render(null, host);
    stopCommands();
    stopKeeping();
    overlayStackReset();
  });
}
