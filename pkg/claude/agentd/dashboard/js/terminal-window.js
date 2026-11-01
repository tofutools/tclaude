// terminal-window.js -- a directory or group shell in an overlay window of its
// own (tcl-0qd62y). It attaches to the same tmux session its tab did, so moving
// it loses nothing, and closing it leaves the shell running like closing the tab.

import { h } from 'preact';
import { computed } from '@preact/signals';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { defaultPlacement } from './companion-model.js';
import { closeOverlayWindow, focusOverlayWindow, minimizeOverlayWindow, openOverlayWindow, overlayWindows } from './overlay-instances.js';
import { windowControlsLook } from './overlay-window-controls.js';
import { OverlayWindow, useOverlayWindow } from './overlay-window.js';
import { PaneDictation } from './terminal-dictation.js';
import { TerminalZoom, readTerminalFont, useTerminalFont } from './terminal-zoom.js';
import { TERMINAL_WINDOW_KIND, paneSeedOf } from './terminal-window-model.js';
import { openTerminalPane, setFloatingTerminals } from './terminals-tab.js';

const html = htm.bind(h);

export const TERMINAL_WINDOW_PREF = 'tclaude.dash.terminal-window.placement';

export const terminalOverlays = computed(() => overlayWindows.value.filter((window) => window.kind === TERMINAL_WINDOW_KIND));

// The windows really in the page. With Collab off the saved set is still there
// but nothing shows it, so it must not answer for a terminal.
const showing = new Set();

// terminalWindowsShowing reports whether any terminal floats where it can be seen.
export const terminalWindowsShowing = () => showing.size > 0;

// A terminal opens in one place at a time: one floating comes forward instead.
// One that cannot show gives up its place, and the terminal opens as a tab.
setFloatingTerminals((key) => {
  const floating = terminalOverlays.peek().find((window) => window.paneKey === key);
  if (!floating) return false;
  if (showing.has(floating.id)) {
    openOverlayWindow(floating);
    focusOverlayWindow(floating.id);
  } else closeOverlayWindow(floating.id);
  return showing.has(floating.id);
});

export function TerminalWindow({ instance, snapshot, prefs, win, notify, widgetFactory, loadRuntime, Terminal }) {
  const name = instance.label || 'Terminal';
  const open = !instance.minimized;
  const [focusToken, setFocusToken] = useState(1);
  const overlay = useOverlayWindow({
    id: instance.id, win, prefs, prefKey: `${TERMINAL_WINDOW_PREF}.${instance.paneKey}`, defaultPlacement, snaps: true, open,
    onRefocus: () => setFocusToken((n) => n + 1),
  });
  useLayoutEffect(() => {
    showing.add(instance.id);
    return () => showing.delete(instance.id);
  }, [instance.id]);
  const [status, setStatus] = useState('');
  const [canReconnect, setCanReconnect] = useState(false);
  const [generation, setGeneration] = useState(0);
  const widgetRef = useRef(null);
  const [font, onFontChange] = useTerminalFont('companion', instance.paneKey, prefs);
  useEffect(() => { if (instance.focusAsked) setFocusToken((n) => n + 1); }, [instance.focusAsked]);
  const pane = { key: instance.id, id: instance.id, label: name, seed: {} };
  const actions = { widgetFor: () => widgetRef.current, activatePane: () => setFocusToken((n) => n + 1) };
  const look = windowControlsLook(snapshot.value?.window_controls);
  const close = () => closeOverlayWindow(instance.id);
  const backToTab = () => {
    close();
    void openTerminalPane(paneSeedOf(instance));
  };
  const title = html`<span class="chat-window-title" title=${name}>⌨ ${name}</span>
    <button type="button" class="board-overlay-action" title="Move this terminal back to a tab" onClick=${backToTab}>Back to a tab</button>
    ${look === 'classic' && html`<button type="button" class="board-overlay-close" title="Close (the shell keeps running)" aria-label=${`Close ${name}`} onClick=${close}>×</button>`}`;
  return html`<${OverlayWindow} id=${instance.id} window=${overlay} open=${open} className="companion-window chat-window" frameClass="companion-resize-frame"
    titlebarClass=${`board-overlay-titlebar overlay-look-${look}`} label=${`Terminal ${name}`} title=${title}
    controls=${{ look, closeLabel: `Close ${name}`, closeTitle: 'Close (the shell keeps running)', onClose: close, onMinimize: () => minimizeOverlayWindow(instance.id) }}>
    <div class="companion-body" id=${instance.id}>
      ${open && html`
        <${PaneDictation} pane=${pane} active=${true} solo=${true} status=${status} canType=${true} target=${null} snapshot=${snapshot} actions=${actions} win=${win} notify=${notify}>
          <${TerminalZoom} font=${font} widget=${() => widgetRef.current} />
        <//>
        ${status !== 'connected' && html`<div class="companion-floating-anchor"><div class="companion-term-status" role="status">${canReconnect
          ? html`${status || 'disconnected'}. <button type="button" onClick=${() => setGeneration((n) => n + 1)}>Reconnect</button>`
          : status}</div></div>`}
        <${Terminal} key=${instance.paneKey} agent=${{ agentId: instance.paneKey, harness: '' }} wsPath=${instance.ws} generation=${generation}
          widgetFactory=${widgetFactory} loadRuntime=${loadRuntime} focusToken=${focusToken} widgetRef=${widgetRef}
          fontSize=${() => readTerminalFont('companion', instance.paneKey, prefs)} onFontChange=${onFontChange}
          onStatus=${setStatus} onReconnectChange=${setCanReconnect} onDisconnect=${(info) => { if (info?.closeTab) close(); }} />`}
    </div>
  <//>`;
}
