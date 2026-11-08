// terminal-window.js -- a directory or group shell in an overlay window of its
// own (tcl-0qd62y). It attaches to the same tmux session its tab did, so moving
// it loses nothing, and closing it leaves the shell running like closing the tab.

import { h } from 'preact';
import { computed } from '@preact/signals';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { closeOverlayTab, focusOverlayWindow, minimizeOverlayWindow, openOverlayWindow, overlayHostUp, overlayTabs } from './overlay-instances.js';
import { windowControlsLook } from './overlay-window-controls.js';
import { OverlayWindow } from './overlay-window.js';
import { useLiveTerminal } from './overlay-window-motion.js';
import { PaneDictation } from './terminal-dictation.js';
import { TerminalZoom, readTerminalFont, useTerminalFont } from './terminal-zoom.js';
import { TERMINAL_WINDOW_KIND, paneSeedOf } from './terminal-window-model.js';
import { elementScreenPoint } from './terminal-drag-out.js';
import { detachSeedWindow, openTerminalPane, setFloatingTerminals } from './terminals-tab.js';
import { POPUP_BLOCKED, TO_COLLAB_LABEL, TO_WINDOW_LABEL, registerTabMoves, tabMoves } from './overlay-tab-moves.js';
import { Button } from './ui-components.js';

const html = htm.bind(h);

export const terminalOverlays = computed(() => overlayTabs.value.filter((tab) => tab.kind === TERMINAL_WINDOW_KIND));

// terminalWindowsShowing reports whether any terminal floats where it can be
// seen. Without an overlay host the saved set is still there but nothing shows
// it, so it must not answer for a terminal.
export const terminalWindowsShowing = () => overlayHostUp.peek() && terminalOverlays.peek().length > 0;

// A terminal opens in one place at a time: one floating comes forward instead.
// One that cannot show gives up its place, and the terminal opens as a tab.
setFloatingTerminals((key) => {
  const floating = terminalOverlays.peek().find((window) => window.paneKey === key);
  if (!floating) return false;
  if (overlayHostUp.peek()) {
    openOverlayWindow(floating);
    focusOverlayWindow(floating.id);
  } else closeOverlayTab(floating.id);
  return overlayHostUp.peek();
});

// A shell goes back to the Terminals pane it came from, or out to a window of its
// own; a blocked pop-up leaves it where it is.
registerTabMoves(TERMINAL_WINDOW_KIND, (tab) => ({
  toCollab: () => {
    closeOverlayTab(tab.id);
    return openTerminalPane(paneSeedOf(tab));
  },
  toWindow: ({ rect, win, notify }) => {
    if (detachSeedWindow({ ...paneSeedOf(tab), floats: true }, { size: rect, at: elementScreenPoint(win, rect) })) closeOverlayTab(tab.id);
    else notify?.(POPUP_BLOCKED, true);
  },
}));

export function TerminalWindow({ instance, overlay, focusRef, strip, more = null, close, snapshot, prefs, win, notify, widgetFactory, loadRuntime, Terminal }) {
  const name = instance.label || 'Terminal';
  const open = !instance.minimized;
  const [focusToken, setFocusToken] = useState(1);
  focusRef.current = () => setFocusToken((n) => n + 1);
  const [status, setStatus] = useState('');
  const [canReconnect, setCanReconnect] = useState(false);
  const [generation, setGeneration] = useState(0);
  const widgetRef = useRef(null);
  const [font, onFontChange] = useTerminalFont('companion', instance.paneKey, prefs);
  useEffect(() => { if (instance.focusAsked) setFocusToken((n) => n + 1); }, [instance.focusAsked]);
  const pane = { key: instance.id, id: instance.id, label: name, seed: {} };
  const actions = { widgetFor: () => widgetRef.current, activatePane: () => setFocusToken((n) => n + 1) };
  const look = windowControlsLook(snapshot.value?.window_controls);
  const leave = () => closeOverlayTab(instance.id);
  // Its terminal is what a minimize or restore shows, live or its last frame.
  const onStatus = useLiveTerminal({
    windowId: open ? instance.windowId : '', tabId: instance.id, widgetRef, win, setStatus,
    host: () => overlay.windowRef.current?.querySelector?.('.companion-term-host') || null,
  });
  const { toCollab, toWindow } = tabMoves(instance);
  const toSeparateWindow = () => toWindow({ rect: overlay.windowRef.current?.getBoundingClientRect?.(), win, notify });
  const title = html`${strip}
    ${look !== 'classic' ? more : html`
    <${Button} size="sm" class="drawing-board-overlay-action" title="Move this terminal to a tab of the Collab view" onClick=${toCollab}>${TO_COLLAB_LABEL}<//>
    <${Button} size="sm" class="drawing-board-overlay-action" title="Move this terminal to a browser window of its own" onClick=${toSeparateWindow}>${TO_WINDOW_LABEL}<//>`}
    ${look === 'classic' && html`<button data-ui-exempt type="button" class="drawing-board-overlay-close" title="Minimize" aria-label=${`Minimize ${name}`} onClick=${() => minimizeOverlayWindow(instance.id)}>–</button>`}
    ${look === 'classic' && html`<button data-ui-exempt type="button" class="drawing-board-overlay-close" title=${close.title} aria-label=${`Close ${name}`} onClick=${close.onClose}>×</button>`}`;
  return html`<${OverlayWindow} id=${instance.windowId} window=${overlay} open=${open} className="companion-window chat-window" frameClass="companion-resize-frame"
    titlebarClass=${`drawing-board-overlay-titlebar overlay-look-${look}`} label=${`Terminal ${name}`} title=${title}
    controls=${{ look, closeLabel: `Close ${name}`, closeTitle: close.title, onClose: close.onClose, onMinimize: () => minimizeOverlayWindow(instance.id), onCollab: toCollab }}>
    <div class="companion-body" id=${instance.id}>
      ${open && html`
        <${PaneDictation} pane=${pane} active=${true} solo=${true} status=${status} canType=${true} target=${null} snapshot=${snapshot} actions=${actions} win=${win} notify=${notify}>
          <${TerminalZoom} font=${font} widget=${() => widgetRef.current} />
        <//>
        ${status !== 'connected' && html`<div class="companion-floating-anchor"><div class="companion-term-status" role="status">${canReconnect
          ? html`${status || 'disconnected'}. <${Button} size="sm" variant="link" onClick=${() => setGeneration((n) => n + 1)}>Reconnect<//>`
          : status}</div></div>`}
        <${Terminal} key=${instance.paneKey} agent=${{ agentId: instance.paneKey, harness: '' }} wsPath=${instance.ws} generation=${generation}
          widgetFactory=${widgetFactory} loadRuntime=${loadRuntime} focusToken=${focusToken} widgetRef=${widgetRef}
          fontSize=${() => readTerminalFont('companion', instance.paneKey, prefs)} onFontChange=${onFontChange}
          onStatus=${onStatus} onReconnectChange=${setCanReconnect} onDisconnect=${(info) => { if (info?.closeTab) leave(); }} />`}
    </div>
  <//>`;
}
