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
import { PROCESS_WINDOW_KIND } from './process-editor-place.js';
import { validProcessTab } from './process-overlay.js';
import { PAGE_WINDOW_KIND } from './page-pane-model.js';
import { validPageTab } from './page-view.js';
import { boardPaneSeed, isBoardId } from './board-model.js';
import { OVERLAY_FOOTER_ROOM, clampPlacement, parsePlacement } from './overlay-window-model.js';
import { isAgentId } from './companion-model.js';
import { openInCompanion } from './companion-facade.js';
import { windowControlsLook } from './overlay-window-controls.js';
import { closeOverlayTab, keepOverlayWindows, minimizeOverlayWindow, minimizedOverlayTabs, overlayWindows } from './overlay-instances.js';
import { CHAT_WINDOW_KIND, DECIDE_WINDOW_KIND, START_WINDOW_KIND } from './chat-window-model.js';
import { TERMINAL_WINDOW_KIND, validTerminalWindow } from './terminal-window-model.js';
import { OverlayWindow, overlayStackReset, readPref } from './overlay-window.js';
import { BOARD_OVERLAY_PLACEMENT_PREF } from './overlay-tab-model.js';
import { elementScreenPoint } from './terminal-drag-out.js';
import { detachSeedWindow } from './terminals-tab.js';
import { POPUP_BLOCKED, TO_COLLAB_LABEL, TO_WINDOW_LABEL, registerTabMoves, tabMoves } from './overlay-tab-moves.js';
import { buildOverlayBulkCommands } from './overlay-window-bulk.js';
import { OverlayExpose, installOverlayExposeKey } from './overlay-expose.js';
import { FooterDock, OverlayDock, OverlayDockPopup } from './overlay-dock.js';
import { registerCommandProvider } from './command-registry.js';
import { Button } from './ui-components.js';

const html = htm.bind(h);

// BOARD_OVERLAY_PLACEMENT_PREF alone is where the one board window stood before
// there were several: what a window for a board not yet moved starts from.
export { BOARD_OVERLAY_PLACEMENT_PREF };
const CASCADE_PX = 28;
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
  toCollab: () => returnBoardToTab(tab.board),
  toWindow: ({ rect, win, notify }) => {
    if (detachSeedWindow({ ...boardPaneSeed(tab.board, tab.label), floats: true }, { size: rect, at: elementScreenPoint(win, rect) })) closeOverlayTab(tab.id);
    else notify?.(POPUP_BLOCKED, true);
  },
}));

export function BoardOverlayWindow({ instance, overlay, strip, more = null, close, snapshot, fetchImpl, notify, win }) {
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
    ${look !== 'classic' ? more : html`
    <${Button} size="sm" class="board-overlay-action" title="Move this board to a tab of the Collab view" onClick=${toCollab}>${TO_COLLAB_LABEL}<//>
    <${Button} size="sm" class="board-overlay-action" title="Move this board to a browser window of its own" onClick=${toSeparateWindow}>${TO_WINDOW_LABEL}<//>`}
    ${look === 'classic' && html`<button data-ui-exempt type="button" class="board-overlay-close" title="Minimize" aria-label=${`Minimize ${board.label}`}
      onClick=${() => minimizeOverlayWindow(instance.id)}>–</button>`}
    ${look === 'classic' && html`<button data-ui-exempt type="button" class="board-overlay-close" title=${close.title} aria-label=${`Close ${board.label}`}
      onClick=${close.onClose}>×</button>`}`;
  return html`<${OverlayWindow} id=${instance.windowId} window=${overlay} open=${open} className="board-overlay" frameClass="board-overlay-resize-frame"
    titlebarClass=${`board-overlay-titlebar overlay-look-${look}`} label=${`Board ${board.label}`} title=${title}
    controls=${{ look, closeLabel: `Close ${board.label}`, closeTitle: close.title, onClose: close.onClose, onMinimize: () => minimizeOverlayWindow(instance.id), onCollab: toCollab }} onTitleWheel=${onTitleWheel}>
    <${BoardEditor} key=${board.id} boardId=${board.id} snapshot=${snapshot} win=${win} fetchImpl=${fetchImpl}
      notify=${notify} onTitle=${(title) => setBoardOverlayLabel(board.id, title)} onOpenAgent=${(agent) => openInCompanion(agent)} />
  <//>`;
}

// BoardOverlay hosts what belongs to every window: the dock, unless it rests in
// the footer (inFooter), its menus, and Show all windows.
export function BoardOverlay({ snapshot, win = globalThis.window, inFooter = false }) {
  if (!overlayWindows.value.length && !minimizedOverlayTabs.value.length) return null;
  return html`<${BoardsGate}>
    ${!inFooter && html`<${OverlayDock} snapshot=${snapshot} />`}<${OverlayDockPopup} snapshot=${snapshot} win=${win} /><${OverlayExpose} win=${win} />
  <//>`;
}

function FooterOverlayDock({ snapshot, win }) {
  if (!overlayWindows.value.length && !minimizedOverlayTabs.value.length) return null;
  return html`<${BoardsGate}><${FooterDock} snapshot=${snapshot} win=${win} /><//>`;
}

function validWindow(window) {
  if (window.kind === BOARD_WINDOW_KIND) return isBoardId(window.board);
  if (window.kind === PROCESS_WINDOW_KIND) return validProcessTab(window);
  if (window.kind === PAGE_WINDOW_KIND) return validPageTab(window);
  if (window.kind === TERMINAL_WINDOW_KIND) return validTerminalWindow(window);
  if (window.kind === START_WINDOW_KIND || window.kind === DECIDE_WINDOW_KIND) return true;
  return window.kind === CHAT_WINDOW_KIND && isAgentId(window.agent);
}

export function mountBoardOverlayIsland({ host, registerCleanup, snapshot, win = globalThis.window }) {
  const stopKeeping = keepOverlayWindows(win, validWindow);
  const stopCommands = registerCommandProvider('overlay-windows', buildOverlayBulkCommands);
  const stopExposeKey = installOverlayExposeKey(win);
  // The dock rests in the footer where the page has one (tcl-9qsgqs).
  const dockHost = win?.document?.getElementById?.('overlay-dock-root');
  render(html`<${BoardOverlay} snapshot=${snapshot} win=${win} inFooter=${!!dockHost} />`, host);
  if (dockHost) render(html`<${FooterOverlayDock} snapshot=${snapshot} win=${win} />`, dockHost);
  registerCleanup(() => {
    render(null, host);
    if (dockHost) render(null, dockHost);
    stopCommands();
    stopExposeKey();
    stopKeeping();
    overlayStackReset();
  });
}
