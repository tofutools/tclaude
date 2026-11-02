// overlay-tab-window.js -- the one place every overlay window is shown from (tcl-e4i44z). This runs the window and its tab strip; the active
// tab's kind brings what is in it, title bar tools included, so the tools always
// belong to the tab showing. kinds holds what each kind is shown with. A kind that
// fails to draw takes down its own tab, not the window beside it.

import { Component, h } from 'preact';
import { useRef } from 'preact/hooks';
import htm from 'htm';
import { BoardOverlayWindow, boardStartPlacement } from './board-overlay.js';
import { WINDOW_GLYPHS } from './overlay-dock.js';
import { BOARD_WINDOW_KIND, boardOverlays } from './board-overlay-state.js';
import { PROCESS_WINDOW_KIND } from './process-editor-place.js';
import { ProcessOverlayWindow, processStartPlacement } from './process-overlay.js';
import { ChatWindow } from './chat-window.js';
import { CHAT_WINDOW_KIND } from './chat-window-model.js';
import { defaultPlacement } from './companion-model.js';
import { askOverlayClose, closeEveryTab, useDropPoint } from './overlay-instances.js';
import { tabJoin } from './overlay-tab-drag.js';
import { activeTabOf } from './overlay-tab-model.js';
import { tabMoves } from './overlay-tab-moves.js';
import { OverlayTabStrip } from './overlay-tab-strip.js';
import { windowControlsLook } from './overlay-window-controls.js';
import { OverlayWindow, useOverlayWindow } from './overlay-window.js';
import { TerminalWindow } from './terminal-window.js';
import { TERMINAL_WINDOW_KIND } from './terminal-window-model.js';

const html = htm.bind(h);

const TAB_BODIES = Object.freeze({
  [CHAT_WINDOW_KIND]: ChatWindow,
  [TERMINAL_WINDOW_KIND]: TerminalWindow,
  [BOARD_WINDOW_KIND]: BoardOverlayWindow,
  [PROCESS_WINDOW_KIND]: ProcessOverlayWindow,
});

// TabBoundary holds what a tab's kind threw, so the dashboard around it lives on.
// Its key is the tab, so showing another tab starts clean.
export class TabBoundary extends Component {
  state = { failed: false };

  componentDidCatch(error) {
    // A fallback that fails too is not this boundary's to hold: it would catch
    // its own error for ever.
    if (this.state.failed) throw error;
    globalThis.console?.error?.('overlay tab failed', error);
    this.setState({ failed: true });
  }

  render({ children, fallback }, { failed }) {
    return failed ? fallback(() => this.setState({ failed: false })) : children;
  }
}

export const plainView = ({ kind, label }) => ({ label: label || 'Terminal', lead: html`<span aria-hidden="true">${WINDOW_GLYPHS[kind] || '▣'}</span>`, title: label });

// tabViewOf is how a tab shows, { label, lead, title }, by its kind.
export const tabViewOf = (kinds) => (tab) => (kinds[tab.kind]?.view || plainView)(tab);

const closeTitle = (window, stewardInside) => {
  if (stewardInside) return window.tabs.length > 1 ? 'Close: other tabs leave and keep running, and the Steward shuts down, after asking' : 'Close: shut it down, after asking';
  return window.tabs.length > 1 ? 'Close: every tab leaves and keeps running in the background' : 'Close: it leaves and keeps running in the background';
};

export function OverlayTabWindow({ window, snapshot, prefs, win, kinds, home = null, add = null }) {
  const tab = activeTabOf(window);
  const start = tab.kind === BOARD_WINDOW_KIND
    ? boardStartPlacement(prefs, boardOverlays.value.findIndex((board) => board.id === tab.id))
    : tab.kind === PROCESS_WINDOW_KIND ? processStartPlacement : defaultPlacement;
  useDropPoint(tab, { win, prefs, prefKey: window.place, start });
  const focusRef = useRef(null);
  const overlay = useOverlayWindow({
    id: window.id, win, prefs, prefKey: window.place, defaultPlacement: start, snaps: true, open: !window.minimized,
    onRefocus: () => focusRef.current?.(),
    ...(window.place === home?.place && { spotted: home.spotted, onPlaced: home.onPlaced }),
    join: window.tabs.length === 1 ? tabJoin(win, tab.id, window.id) : null,
  });
  const asking = window.tabs.find((candidate) => kinds[candidate.kind]?.asksToClose?.(candidate));
  const close = {
    title: closeTitle(window, !!asking),
    onClose: () => {
      closeEveryTab(window.id, { asking: asking?.id, closeOf: (candidate) => tabMoves(candidate).close });
      if (asking) askOverlayClose(asking.id);
    },
  };
  const viewOf = tabViewOf(kinds);
  const strip = html`<${OverlayTabStrip} window=${window} overlay=${overlay} win=${win} viewOf=${viewOf} />${add?.(window)}`;
  const Body = kinds[tab.kind]?.Body || TAB_BODIES[tab.kind];
  const minimize = home?.shrinks?.(window, overlay);
  const fallback = (retry) => {
    const look = windowControlsLook(snapshot.value?.window_controls);
    return html`<${OverlayWindow} id=${window.id} window=${overlay} open=${!window.minimized} className="companion-window chat-window" frameClass="companion-resize-frame"
      titlebarClass=${`board-overlay-titlebar overlay-look-${look}`} label=${`Failed: ${tab.label}`} title=${strip}
      controls=${{ look, closeLabel: `Close ${tab.label}`, closeTitle: close.title, onClose: close.onClose }}>
      <div class="companion-empty" role="alert"><p><b>${tab.label}</b> stopped drawing. Nothing it runs has stopped.</p>
        <button type="button" class="companion-primary" onClick=${retry}>Try again</button></div>
    <//>`;
  };
  return html`<${TabBoundary} key=${tab.id} fallback=${fallback}>
    <${Body} instance=${tab} overlay=${overlay} focusRef=${focusRef} strip=${strip} close=${close} minimize=${minimize} tabCount=${window.tabs.length} ...${kinds[tab.kind]} />
  <//>`;
}
