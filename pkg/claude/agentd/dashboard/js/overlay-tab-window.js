// overlay-tab-window.js -- the one place every overlay window but the companion
// is shown from (tcl-e4i44z). This runs the window; the active tab's kind brings
// what is in it, title bar tools included, so the tools always belong to the tab
// showing. kinds holds what each kind is shown with.

import { h } from 'preact';
import { useRef } from 'preact/hooks';
import htm from 'htm';
import { BoardOverlayWindow, boardStartPlacement } from './board-overlay.js';
import { BOARD_WINDOW_KIND, boardOverlays } from './board-overlay-state.js';
import { ChatWindow } from './chat-window.js';
import { CHAT_WINDOW_KIND } from './chat-window-model.js';
import { defaultPlacement } from './companion-model.js';
import { useDropPoint } from './overlay-instances.js';
import { activeTabOf } from './overlay-tab-model.js';
import { useOverlayWindow } from './overlay-window.js';
import { TerminalWindow } from './terminal-window.js';
import { TERMINAL_WINDOW_KIND } from './terminal-window-model.js';

const html = htm.bind(h);

const TAB_BODIES = Object.freeze({
  [CHAT_WINDOW_KIND]: ChatWindow,
  [TERMINAL_WINDOW_KIND]: TerminalWindow,
  [BOARD_WINDOW_KIND]: BoardOverlayWindow,
});

export function OverlayTabWindow({ window, prefs, win, kinds }) {
  const tab = activeTabOf(window);
  const start = tab.kind === BOARD_WINDOW_KIND
    ? boardStartPlacement(prefs, boardOverlays.value.findIndex((board) => board.id === tab.id))
    : defaultPlacement;
  useDropPoint(tab, { win, prefs, prefKey: window.place, start });
  const focusRef = useRef(null);
  const overlay = useOverlayWindow({
    id: window.id, win, prefs, prefKey: window.place, defaultPlacement: start, snaps: true, open: !window.minimized,
    onRefocus: () => focusRef.current?.(),
  });
  const Body = TAB_BODIES[tab.kind];
  return html`<${Body} key=${tab.id} instance=${tab} overlay=${overlay} focusRef=${focusRef} ...${kinds[tab.kind]} />`;
}
