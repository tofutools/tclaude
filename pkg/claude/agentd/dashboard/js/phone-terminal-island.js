// phone-terminal-island.js -- the phone's terminal: one terminal at a time,
// docked full screen over the dashboard, never a Collab pane or a window.
//
// Every terminal the dashboard opens on a phone lands here (terminals-tab.js
// setPhoneTerminals): an agent's Open, a deep link, a shell. Only the terminal
// on screen is live: closing it, or the page going to the background, drops its
// socket, and coming back redials.
//
// A phone has no keyboard to spare, so the terminal does not raise one. Input
// depends on what runs in it (terminalKind). An agent's terminal has the
// agent's message sheet (✉ Message, the desktop's Ctrl+M) and a key strip for
// the answers a harness asks for (1 2 3, the arrows, Enter, Esc, ^C). A
// shell's (a group shell, or an agent of a command-input harness) has a sheet
// that types straight into it, with no message header, and a strip of the
// keys a command line wants (the four arrows, Tab, Space, Enter, Esc, ^C).
// Both have ⌨ for raw typing. The bar shows the agent's state and, behind ⋯,
// what else a phone can do to it (the Agents view's sheet). A pinch sets the
// text size, which the pinch chip shows and resets, and the grid reflows to
// fit. A
// one-finger swipe scrolls exactly as the desktop's mouse wheel does, so each
// harness scrolls its own way: Claude Code its own history, Codex and shells
// tmux's. Where the wheel scrolls tmux copy-mode (Codex, shells), ↓ Live (or
// any key) scrolls back down first, so copy-mode never takes a key.

import { h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';
import { readTerminalFont, useTerminalFont } from './terminal-zoom.js';
import { announceOverlay, registerOverlayRoute } from './overlay-routes.js';
import { VARIANT_LABELS } from './phone-agents-model.js';
import { IconButton, StatePill } from './phone-ui.js';
import { TerminalText, indexAt, linkAt, openLinkSheet, screenLines, useTerminalTouch, wordAt } from './phone-terminal-text.js';
import { writeClipboard } from './insecure-origin.js';
import { shellToast } from './shell-state.js';
import { openTypeSheet } from './phone-terminal-type.js';
import { isAgentTerminalPath } from './terminal-interactions.js';
import { NavToggle, terminalDock } from './phone-nav-toggle.js';

const html = htm.bind(h);

// The open terminal's seed (terminals-core.js normalizeSeed), or null.
export const phoneTerminal = signal(null);

const keyOf = (seed) => (seed ? seed.key || seed.ws : '');

// wheelCopyMode says whether a terminal's wheel may scroll tmux copy-mode,
// which takes keys (tmux mouse on: Codex, shells): then a swipe back shows
// ↓ Live, and a key scrolls back down first. A harness that renders its own
// scrollback (the catalog's tmux_scrollback false: Claude Code) scrolls its
// own history and takes keys while scrolled back, as on a desktop. An agent's
// terminal whose seed names no harness takes it from the agent, else Claude
// Code (the default harness, as harnessCanRename assumes); a terminal of no
// agent (a shell, a group terminal) may scroll copy-mode.
export function wheelCopyMode(seed, { harnesses = [], agent = () => null } = {}) {
  const harness = terminalHarness(seed, agent);
  if (!harness) return true;
  const known = (Array.isArray(harnesses) ? harnesses : []).find((h) => h?.name === harness);
  return known?.tmux_scrollback !== false;
}

// terminalHarness is the harness a terminal's session runs, as wheelCopyMode
// reads it, or '' for a terminal of no agent. Without known, an agent's
// terminal of no known harness is Claude Code's, the default harness.
function terminalHarness(seed, agent = () => null, known = false) {
  const agentId = seed?.agent || seed?.remote?.agent || '';
  let harness = seed?.harness || '';
  if (!harness && seed?.agent) harness = agent(seed.agent)?.harness || '';
  if (!harness && agentId && !known) harness = 'claude';
  return harness;
}

// terminalKind is what a terminal's input is for, by the pane first: any
// pane but an agent's own session (isAgentTerminalPath: a group's shell, an
// agent's directory shell, a peer's shell) is a 'shell'. An agent's session
// is a 'shell' too when its harness takes command input (the catalog's
// command_input: --harness shell), else an 'agent', as is one of a harness
// the catalog does not (yet) know.
export function terminalKind(seed, { harnesses = [], agent = () => null } = {}) {
  if (!isAgentTerminalPath(seed?.ws || '')) return 'shell';
  const harness = terminalHarness(seed, agent, true);
  const known = (Array.isArray(harnesses) ? harnesses : []).find((h) => h?.name === harness);
  return known?.command_input === true ? 'shell' : 'agent';
}

// swipeWheel is how far a terminal's session scrolls a wheel notch, for the
// swipe (terminals-core.js): Claude Code's own measured acceleration, else
// five lines a notch, which tmux copy-mode scrolls. Only a harness known to
// be Claude Code's: a notch a row over tmux copy-mode would run five times
// ahead of the finger, where the other way round only lags.
export function swipeWheel(seed, { agent = () => null } = {}) {
  return terminalHarness(seed, agent, true) === 'claude' ? 'claude' : 'tmux';
}

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

// The key strips, by terminalKind: each key's name for widget.key(), its
// label and its spoken name. Copy follows each. An agent's answers a
// harness's numbered prompts; a shell's moves along and through a command
// line.
const strip = (name, label, spoken = label) => Object.freeze({ name, label, spoken });
export const KEY_STRIPS = Object.freeze({
  agent: Object.freeze([
    strip('1', '1'), strip('2', '2'), strip('3', '3'),
    strip('up', '↑', 'Up'), strip('down', '↓', 'Down'), strip('enter', '⏎', 'Enter'),
    strip('esc', 'Esc', 'Escape'), strip('ctrl-c', '^C', 'Control C'),
  ]),
  shell: Object.freeze([
    strip('left', '←', 'Left'), strip('right', '→', 'Right'), strip('up', '↑', 'Up'), strip('down', '↓', 'Down'),
    strip('tab', '⇥', 'Tab'), strip('space', '␣', 'Space'), strip('enter', '⏎', 'Enter'),
    strip('esc', 'Esc', 'Escape'), strip('ctrl-c', '^C', 'Control C'),
  ]),
});
export const KEY_STRIP = KEY_STRIPS.agent;

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
function TerminalSurface({ seed, deps, kindRef, labelRef, onWidget, onFontChange, onStatus, onWaitingChange, onReconnectChange, onScrollBackChange, onTap, onHold }) {
  const host = useRef(null);
  // A tap on a link, or a still hold, on the terminal itself
  // (phone-terminal-text.js); the zoom chip and Live are not the terminal.
  useTerminalTouch(host, { onTap, onHold });
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
      // Both read at each notch: on a cold link the agent's harness, and the
      // harness catalog, are known only once the snapshot arrives.
      wheelCopyMode: () => wheelCopyMode(seed, { harnesses: deps.harnesses?.() || [], agent: deps.agent || (() => null) }),
      swipeWheel: () => swipeWheel(seed, { agent: deps.agent || (() => null) }),
      onScrollBackChange,
      softKeyboard: false,
      initialFontSize: readTerminalFont(FONT_SURFACE, key, deps.prefs) ?? PHONE_TERMINAL_FONT,
      onFontChange,
      onStatus,
      onWaitingChange,
      onReconnectChange,
      // An agent's session only; its kind and the bar's name read when
      // pressed: the widget keeps its options, and a cold link learns the
      // harness (a shell's, maybe) only once the snapshot arrives.
      onComposeMessage: isAgentTerminalPath(seed.ws || '') && seed.agent && deps.onComposeMessage
        ? () => { if (kindRef.current === 'agent') deps.onComposeMessage({ agent: seed.agent, label: labelRef.current }); }
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

// zoomPercent is the text size against the phone's own, as the pinch chip
// shows it.
export function zoomPercent(size) {
  return Math.round((size / PHONE_TERMINAL_FONT) * 100);
}

// The bar's title: the agent's name, its group muted after it.
function Title({ label, group }) {
  return html`<h1 class="phone-bar-title">${label || 'Terminal'}${group && html`<small>${group}</small>`}</h1>`;
}

// DIR_SHELL_WHERE says which of an agent's directories its shell is in, when
// the path itself is not known here (the current one is the daemon's).
const DIR_SHELL_WHERE = { worktree: 'its worktree', start: 'its launch directory', current: 'where it last edited' };

// dirShellTitle is the bar of an agent's directory shell (a term-ws pane,
// key term:<agent>:<which>:<group>): it is a shell of the operator's own in
// the agent's directory, not the agent, so it is named a shell, with the
// directory under it, and carries no agent state. null for any other
// terminal.
export function dirShellTitle(seed, row) {
  if (!seed?.agent || !String(seed.ws || '').startsWith('/api/term-ws/')) return null;
  const which = String(seed.key || '').split(':')[2] || 'current';
  const name = row?.name || String(seed.label || '').replace(/^shell · /, '') || 'the agent';
  const member = row?.member || {};
  const path = which === 'worktree' ? member.current_dir || member.startup_dir
    : which === 'start' ? member.startup_dir || member.state?.cwd : '';
  return { label: `›_ Shell · ${name}'s dir`, sub: path || DIR_SHELL_WHERE[which] || '' };
}

export function PhoneTerminal({ seed, deps, onClose }) {
  const documentRef = deps.documentRef || globalThis.document;
  const visible = useDocumentVisible(documentRef);
  const viewport = useVisualViewport(deps.windowRef || globalThis.window);
  const key = seed.key || seed.ws;
  const [font, setFont] = useTerminalFont(FONT_SURFACE, key, deps.prefs);
  // The pinch chip waits for the widget's size: until then the hook holds
  // the desktop's default.
  const [sized, setSized] = useState(false);
  const onFontChange = (next) => { setSized(true); setFont(next); };
  const [runtime, setRuntime] = useState(deps.loadRuntime ? 'loading' : 'ready');
  const [status, setStatus] = useState('connecting');
  const [waiting, setWaiting] = useState(null);
  const [reconnect, setReconnect] = useState(false);
  const [keyboard, setKeyboard] = useState(false);
  const [scrolledBack, setScrolledBack] = useState(false);
  // select: Select's lines and the word picked, while it is open.
  const [select, setSelect] = useState(null);
  const widget = useRef(null);
  const copy = deps.copy || (async (text, what) => {
    if (await writeClipboard(text)) shellToast(`Copied ${what}`);
    else shellToast(`Could not copy ${what}`, true);
  });
  const lines = () => screenLines(widget.current?.screen?.() || []);
  const showLink = (link) => openLinkSheet(link, { widget: widget.current, copy });
  const onTap = (x, y) => {
    const now = lines();
    const link = linkAt(now, indexAt(now, widget.current?.cellAt?.(x, y)));
    if (link) showLink(link);
    return !!link;
  };
  // openSelect opens Select on the screen as it is, the word at a point
  // (a hold's) picked.
  const openSelect = (x = null, y = null) => {
    const now = lines();
    const at = x === null ? null : indexAt(now, widget.current?.cellAt?.(x, y));
    const word = at && wordAt(now[at.line].text, at.index);
    setSelect({ lines: now, picked: word ? [at.line, ...word] : null });
  };

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

  // The phone shell shows the bottom nav under the terminal (or not, by the
  // ▾/▴ toggle's choice), and a view picked there closes the terminal.
  const closeRef = useRef(onClose);
  closeRef.current = onClose;
  // Select covers the whole terminal, its Copy buttons at the bottom, so
  // the nav stays away while it is open, as over the keyboard.
  const covered = !!select;
  useEffect(() => {
    terminalDock.value = { keyboard: keyboard || covered, close: () => closeRef.current?.() };
  }, [keyboard, covered]);
  useEffect(() => () => { terminalDock.value = null; }, []);

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
  // The agent behind the terminal, live from the snapshot: its name (a link
  // opened before the snapshot came knows only its id), its state for the
  // pill, its group for the title, and ⋯ for what else a phone can do to it.
  const found = seed.agent ? deps.agentRow?.(seed.agent) || null : null;
  const row = found?.row || null;
  const label = row?.name || seed.label || '';
  const labelRef = useRef(label);
  labelRef.current = label;
  // What runs in it, read each render: on a cold link an agent's harness
  // is known only once the snapshot arrives.
  const kind = terminalKind(seed, { harnesses: deps.harnesses?.() || [], agent: deps.agent || (() => null) });
  const keys = KEY_STRIPS[kind];
  const kindRef = useRef(kind);
  kindRef.current = kind;
  const message = kind === 'agent' && seed.agent && deps.onComposeMessage
    ? () => deps.onComposeMessage({ agent: seed.agent, label })
    : null;
  // A shell's text goes in as typed: the widget's paste, the path its own
  // keys take, then Enter when asked.
  // An agent's directory shell is named for itself, not for the agent.
  const typeInto = (isAgentTerminalPath(seed.ws || '') ? label : seed.label || label) || 'the terminal';
  const type = kind === 'shell'
    ? () => openTypeSheet({
      name: typeInto,
      type: (text, enter) => {
        const term = widget.current;
        if (!term) return false;
        if (text && !term.paste(text)) return false;
        return enter ? term.key('enter') !== false : true;
      },
    })
    : null;
  const live = status === 'connected';
  const note = runtime === 'failed' ? 'The terminal could not load.'
    : runtime === 'loading' ? 'Loading…'
      : waiting?.text || (status === 'connected' || status === 'connecting' ? '' : status);
  const pct = zoomPercent(font.size);
  const name = label || 'the agent';
  const dirShell = dirShellTitle(seed, row);

  const style = viewport ? `height:${viewport.height}px;top:${viewport.top}px` : undefined;
  return html`<div class="phone-term" role="dialog" aria-modal="true" aria-label=${dirShell ? dirShell.label : label ? `Terminal: ${label}` : 'Terminal'} style=${style}>
    <header class="phone-term-bar">
      <button data-ui-exempt type="button" class="phone-bar-back" aria-label="Back" onClick=${onClose}>‹</button>
      ${dirShell
        ? html`<h1 class="phone-bar-title phone-term-dir-title"><span>${dirShell.label}</span>${dirShell.sub && html`<small><bdi>${dirShell.sub}</bdi></small>`}</h1>`
        : html`<${Title} label=${label} group=${row?.group || ''} />`}
      ${row && !dirShell
        ? html`<${StatePill} state=${row.variant} title=${live ? undefined : `Terminal: ${status}`}>${row.waking ? 'waking' : VARIANT_LABELS[row.variant]}<//>`
        : html`<span class=${`phone-bar-live ${live ? 'connected' : status === 'disconnected' ? 'disconnected' : ''}`} role="status"
          aria-label=${live ? 'Connected' : `Terminal: ${status}`} title=${status}></span>`}
      ${row && deps.agentMenu && html`<${IconButton} icon="⋯" label=${`${row.name}: more`} onClick=${() => deps.agentMenu(row, { selectText: () => openSelect() })} />`}
    </header>
    <div class="phone-term-body">
      ${runtime === 'ready' && visible && html`<${TerminalSurface} key=${key} seed=${seed} deps=${deps} kindRef=${kindRef} labelRef=${labelRef} onWidget=${onWidget}
        onFontChange=${onFontChange} onStatus=${setStatus} onWaitingChange=${setWaiting} onReconnectChange=${setReconnect}
        onScrollBackChange=${setScrolledBack} onTap=${onTap} onHold=${openSelect} />`}
      ${sized && html`<button data-ui-exempt type="button" class="phone-term-zoom" onMouseDown=${keepFocus}
        aria-label=${pct === 100 ? 'Text size 100%: pinch to zoom' : `Text size ${pct}%: reset to 100%`}
        title="Pinch the terminal to zoom it" disabled=${pct === 100}
        onClick=${() => widget.current?.zoomBy?.(PHONE_TERMINAL_FONT - font.size)}>pinch · ${pct}%</button>`}
      ${scrolledBack && html`<button data-ui-exempt type="button" class="phone-term-live" onMouseDown=${keepFocus}
        onClick=${() => widget.current?.toLive()} aria-label="Back to live">↓ Live</button>`}
      ${(note || reconnect) && html`<div class="phone-term-note" role="status">
        ${note && html`<span>${note}</span>`}
        ${reconnect && html`<button data-ui-exempt type="button" class="phone-term-reconnect" onClick=${() => void widget.current?.connect()}>Reconnect</button>`}
      </div>`}
    </div>
    <div class=${`phone-term-keys phone-term-keys-${kind}`} role="group" aria-label="Keys" style=${`--phone-term-keys:${keys.length + 1}`}>
      ${keys.map(({ name: k, label, spoken }) => html`<button data-ui-exempt key=${k} type="button" class="phone-term-key"
        aria-label=${spoken} onMouseDown=${keepFocus} onClick=${() => widget.current?.key(k)}>${label}</button>`)}
      <button data-ui-exempt type="button" class="phone-term-key phone-term-copy-key" aria-label="Select and copy text"
        onMouseDown=${keepFocus} onClick=${() => openSelect()}>Copy</button>
    </div>
    <div class="phone-term-input">
      ${!keyboard && html`<${NavToggle} kind="terminal" prefs=${deps.prefs} onMouseDown=${keepFocus} />`}
      ${message && !keyboard && html`<button data-ui-exempt type="button" class="phone-term-compose" onClick=${message}>✉ Message ${name}…</button>`}
      ${type && !keyboard && html`<button data-ui-exempt type="button" class="phone-term-compose phone-term-type" onClick=${type}>› Type into ${typeInto}…</button>`}
      <button data-ui-exempt type="button" class=${`phone-term-kbd${keyboard ? ' on' : ''}`} aria-pressed=${keyboard ? 'true' : 'false'}
        aria-label="Keyboard" title="Type into the terminal" onMouseDown=${keepFocus} onClick=${toggleKeyboard}>⌨</button>
    </div>
    ${select && html`<${TerminalText} lines=${select.lines} picked=${select.picked} onLink=${showLink} copy=${copy} onClose=${() => setSelect(null)} />`}
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
