// phone-terminal-island.js -- the phone's terminal: one terminal at a time,
// docked full screen over the dashboard, never a Collab pane or a window.
//
// Every terminal the dashboard opens on a phone lands here (terminals-tab.js
// setPhoneTerminals): an agent's Open, a deep link, a shell. Only the terminal
// on screen is live: closing it, or the page going to the background, drops its
// socket, and coming back redials.
//
// A phone has no keyboard to spare, so the terminal does not raise one. Input
// is the agent's message sheet (the desktop's Ctrl+M), a key strip for the
// answers a harness asks for (1 2 3, the arrows, Enter, Esc, ^C), and ⌨ for
// raw typing. A pinch sets the text size and the grid reflows to fit. A
// one-finger swipe scrolls back through the session's history in tmux
// copy-mode (as the wheel does on a desktop where tmux takes the mouse, and
// through agentd where it does not, as for Claude Code), and ↓ Live (or any
// key) comes back.

import { h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';
import { readTerminalFont, useTerminalFont } from './terminal-zoom.js';
import { announceOverlay, registerOverlayRoute } from './overlay-routes.js';

const html = htm.bind(h);

// The open terminal's seed (terminals-core.js normalizeSeed), or null.
export const phoneTerminal = signal(null);

const keyOf = (seed) => (seed ? seed.key || seed.ws : '');

// The terminals this page opened, by key, so Back and Forward can bring one
// back. A link from elsewhere can only name an agent's own terminal.
const opened = new Map();
const AGENT_TERMINAL = /^window:([A-Za-z0-9_-]{1,128})$/;

// An open terminal is part of the location (the terminal overlay param,
// overlay-routes.js): opening one is a history step, so the phone's Back
// closes it, and closing it steps back.
export function openPhoneTerminal(seed) {
  const key = keyOf(seed);
  const shown = keyOf(phoneTerminal.value);
  opened.set(key, seed);
  phoneTerminal.value = seed;
  if (shown !== key) announceOverlay({ replace: !!shown });
  return { key, phone: true };
}

export function closePhoneTerminal() {
  if (!phoneTerminal.value) return;
  phoneTerminal.value = null;
  announceOverlay({ closed: true });
}

// seedFor is the terminal a location names: one this page opened, or an
// agent's own terminal (agent(id) gives its { label, harness }).
function seedFor(key, agent) {
  if (opened.has(key)) return opened.get(key);
  const match = AGENT_TERMINAL.exec(key);
  if (!match) return null;
  const id = match[1];
  const { label = '', harness = '' } = agent?.(id) || {};
  return {
    ws: `/api/open-window-ws/${encodeURIComponent(id)}`, key, label: label || id, agent: id, hideConv: id, harness,
    initialRetry: true,
  };
}

export function registerPhoneTerminalRoute(agent) {
  return registerOverlayRoute('terminal', {
    value: () => keyOf(phoneTerminal.value),
    restore: (key) => {
      const seed = key ? seedFor(key, agent) : null;
      phoneTerminal.value = seed;
      // A key nothing can open leaves the location.
      if (key && !seed) announceOverlay({ replace: true });
    },
  });
}

// The phone's text size until the operator pinches: small enough for a
// harness's layout to fit a phone's width.
export const PHONE_TERMINAL_FONT = 11;
const FONT_SURFACE = 'phone';

// The key strip: each key's name for widget.key(), its label and its
// spoken name.
const strip = (name, label, spoken = label) => Object.freeze({ name, label, spoken });
export const KEY_STRIP = Object.freeze([
  strip('1', '1'), strip('2', '2'), strip('3', '3'),
  strip('up', '↑', 'Up'), strip('down', '↓', 'Down'), strip('enter', '⏎', 'Enter'),
  strip('esc', 'Esc', 'Escape'), strip('ctrl-c', '^C', 'Control C'),
]);

function useDocumentVisible(documentRef) {
  const [visible, setVisible] = useState(documentRef.visibilityState !== 'hidden');
  useEffect(() => {
    const sync = () => setVisible(documentRef.visibilityState !== 'hidden');
    documentRef.addEventListener('visibilitychange', sync);
    return () => documentRef.removeEventListener('visibilitychange', sync);
  }, [documentRef]);
  return visible;
}

// The part of the screen the on-screen keyboard leaves, so the key strip
// rides above the keyboard while ⌨ is on.
function useVisualViewport(windowRef) {
  const viewport = windowRef?.visualViewport;
  const read = () => (viewport ? { height: viewport.height, top: viewport.offsetTop } : null);
  const [box, setBox] = useState(read);
  useEffect(() => {
    if (!viewport) return undefined;
    const sync = () => setBox(read());
    viewport.addEventListener('resize', sync);
    viewport.addEventListener('scroll', sync);
    return () => {
      viewport.removeEventListener('resize', sync);
      viewport.removeEventListener('scroll', sync);
    };
  }, [viewport]);
  return box;
}

// TerminalSurface owns the widget. Preact renders the host div and never a
// child into it: the widget alone owns what is inside.
function TerminalSurface({ seed, deps, onWidget, onFontChange, onStatus, onWaitingChange, onReconnectChange, onScrollBackChange }) {
  const host = useRef(null);
  useLayoutEffect(() => {
    const key = seed.key || seed.ws;
    const widget = deps.widgetFactory({
      ...deps.widgetOptions(seed),
      host: host.current,
      wsPath: seed.ws,
      active: true,
      agentLabel: seed.label || '',
      touchZoom: true,
      touchScroll: true,
      onScrollBackChange,
      softKeyboard: false,
      initialFontSize: readTerminalFont(FONT_SURFACE, key, deps.prefs) ?? PHONE_TERMINAL_FONT,
      onFontChange,
      onStatus,
      onWaitingChange,
      onReconnectChange,
      onComposeMessage: seed.agent && deps.onComposeMessage
        ? () => deps.onComposeMessage({ agent: seed.agent, label: seed.label || '' })
        : null,
      applicationClipboardShortcuts: seed.harness === 'copilot',
      initialRetry: seed.initialRetry === true,
    });
    onWidget(widget);
    void widget.connect();
    return () => {
      widget.dispose();
      onWidget(null);
    };
  }, []);
  return html`<div ref=${host} class="phone-term-host"></div>`;
}

// A key strip press must not take focus from the terminal: that would drop
// a raised keyboard, or raise one.
const keepFocus = (event) => event.preventDefault();

export function PhoneTerminal({ seed, deps, onClose }) {
  const documentRef = deps.documentRef || globalThis.document;
  const visible = useDocumentVisible(documentRef);
  const viewport = useVisualViewport(deps.windowRef || globalThis.window);
  const key = seed.key || seed.ws;
  const [, onFontChange] = useTerminalFont(FONT_SURFACE, key, deps.prefs);
  const [runtime, setRuntime] = useState(deps.loadRuntime ? 'loading' : 'ready');
  const [status, setStatus] = useState('connecting');
  const [waiting, setWaiting] = useState(null);
  const [reconnect, setReconnect] = useState(false);
  const [keyboard, setKeyboard] = useState(false);
  const [scrolledBack, setScrolledBack] = useState(false);
  const widget = useRef(null);

  useEffect(() => {
    if (!deps.loadRuntime) return undefined;
    let live = true;
    deps.loadRuntime().then(() => { if (live) setRuntime('ready'); }, () => { if (live) setRuntime('failed'); });
    return () => { live = false; };
  }, []);

  // The page behind does not scroll under the terminal.
  useEffect(() => {
    const root = documentRef.documentElement;
    root.dataset.phoneTerminal = 'open';
    return () => { delete root.dataset.phoneTerminal; };
  }, []);

  const toggleKeyboard = () => {
    const next = !keyboard;
    setKeyboard(next);
    widget.current?.setSoftKeyboard(next);
  };
  const onWidget = (next) => {
    widget.current = next;
    if (next && keyboard) next.setSoftKeyboard(true);
    if (!next) setScrolledBack(false);
  };
  const message = seed.agent && deps.onComposeMessage
    ? () => deps.onComposeMessage({ agent: seed.agent, label: seed.label || '' })
    : null;
  const live = status === 'connected';
  const note = runtime === 'failed' ? 'The terminal could not load.'
    : runtime === 'loading' ? 'Loading…'
      : waiting?.text || (status === 'connected' || status === 'connecting' ? '' : status);

  const style = viewport ? `height:${viewport.height}px;top:${viewport.top}px` : undefined;
  return html`<div class="phone-term" role="dialog" aria-modal="true" aria-label=${seed.label ? `Terminal: ${seed.label}` : 'Terminal'} style=${style}>
    <header class="phone-term-bar">
      <button data-ui-exempt type="button" class="phone-bar-back" aria-label="Back" onClick=${onClose}>‹</button>
      <h1 class="phone-bar-title">${seed.label || 'Terminal'}</h1>
      <span class=${`phone-bar-live ${live ? 'connected' : status === 'disconnected' ? 'disconnected' : ''}`} role="status"
        aria-label=${live ? 'Connected' : `Terminal: ${status}`} title=${status}></span>
      <button data-ui-exempt type="button" class=${`phone-bar-act${keyboard ? ' on' : ''}`} aria-pressed=${keyboard ? 'true' : 'false'}
        aria-label="Keyboard" title="Type into the terminal" onMouseDown=${keepFocus} onClick=${toggleKeyboard}>⌨</button>
    </header>
    <div class="phone-term-body">
      ${runtime === 'ready' && visible && html`<${TerminalSurface} key=${key} seed=${seed} deps=${deps} onWidget=${onWidget}
        onFontChange=${onFontChange} onStatus=${setStatus} onWaitingChange=${setWaiting} onReconnectChange=${setReconnect}
        onScrollBackChange=${setScrolledBack} />`}
      ${scrolledBack && html`<button data-ui-exempt type="button" class="phone-term-live" onMouseDown=${keepFocus}
        onClick=${() => widget.current?.toLive()} aria-label="Back to live">↓ Live</button>`}
      ${(note || reconnect) && html`<div class="phone-term-note" role="status">
        ${note && html`<span>${note}</span>`}
        ${reconnect && html`<button data-ui-exempt type="button" class="phone-term-reconnect" onClick=${() => void widget.current?.connect()}>Reconnect</button>`}
      </div>`}
    </div>
    <div class="phone-term-keys" role="group" aria-label="Keys">
      ${KEY_STRIP.map(({ name, label, spoken }) => html`<button data-ui-exempt key=${name} type="button" class="phone-term-key"
        aria-label=${spoken} onMouseDown=${keepFocus} onClick=${() => widget.current?.key(name)}>${label}</button>`)}
    </div>
    ${message && !keyboard && html`<button data-ui-exempt type="button" class="phone-term-compose" onClick=${message}>
      Message ${seed.label || 'the agent'}…</button>`}
  </div>`;
}

export function PhoneTerminalHost({ deps }) {
  const seed = phoneTerminal.value;
  if (!seed) return null;
  return html`<${PhoneTerminal} key=${seed.key || seed.ws} seed=${seed} deps=${deps} onClose=${closePhoneTerminal} />`;
}

export function mountPhoneTerminalIsland({ host, deps, register, registerCleanup }) {
  const unregister = register(openPhoneTerminal);
  const unroute = registerPhoneTerminalRoute(deps.agent);
  render(html`<${PhoneTerminalHost} deps=${deps} />`, host);
  registerCleanup(() => {
    unregister();
    unroute();
    render(null, host);
    phoneTerminal.value = null;
  });
}
