// terminals-core.js — the reusable xterm-over-WebSocket pane multiplexer.
//
// Extracted from the standalone terminals page so the SAME pane machinery
// backs two mounts:
//   * the in-dashboard "Terminals" tab (js/terminals-tab.js) — the default
//     surface for "web term" / "web window", a nav tab that appears only while
//     ≥1 terminal is open;
//   * the standalone /terminals page (js/terminals.js) — now used only for the
//     per-terminal "⧉ tab" pop-out (?solo=1: one terminal in its own window).
//
// Each pane streams over a WebSocket to a real PTY on the agentd host — the
// same /api/term-ws/{conv} and /api/open-window-ws/{conv} endpoints the
// Preact-owned in-dashboard modal connects to. Background panes stay live:
// their socket keeps writing into the off-screen xterm buffer, so switching to
// a pane (or back to the tab) shows the up-to-date terminal.
//
// Terminal / FitAddon / WebLinksAddon are globals from the vendored classic xterm scripts
// loaded before the module graph (both dashboard.html and terminals.html load
// them). The core imports nothing from the dashboard SPA, so the standalone
// page stays free of dashboard.css / helpers.js.

import { attachTerminalInteractions } from './terminal-interactions.js';
import {
  arcanePaletteEnabled, setArcanePaletteEnabled, terminalThemeFor,
} from './terminal-theme.js';

function seedKey(seed) {
  return seed.key || seed.ws;
}

// departedAgentSelectors returns every stable agent-id / conversation-id that
// belonged to the previous active roster but not the next one. Keeping this as
// a roster transition (instead of treating every selector absent from one
// snapshot as retired) makes the first dashboard snapshot a harmless baseline.
// Both identities are included because pane seeds prefer agent_id but retain a
// conv-id fallback for older / partially migrated rows.
export function departedAgentSelectors(previousAgents, nextAgents) {
  if (!Array.isArray(previousAgents) || !Array.isArray(nextAgents)) return [];
  const selectors = (agents) => {
    const out = new Set();
    for (const agent of agents) {
      if (!agent || typeof agent !== 'object') continue;
      if (typeof agent.agent_id === 'string' && agent.agent_id) out.add(agent.agent_id);
      if (typeof agent.conv_id === 'string' && agent.conv_id) out.add(agent.conv_id);
    }
    return out;
  };
  const before = selectors(previousAgents);
  const after = selectors(nextAgents);
  return [...before].filter(selector => !after.has(selector));
}

// createAgentRosterReconciler keeps the last AUTHORITATIVE active roster and
// returns selectors that departed on the next authoritative observation.
// Degraded snapshots are ignored without replacing the baseline, so a
// transient server-side roster read failure neither closes panes spuriously
// nor consumes a real retirement that becomes visible on the following poll.
export function createAgentRosterReconciler() {
  let previous = null;
  return (nextAgents, authoritative) => {
    if (!authoritative || !Array.isArray(nextAgents)) return [];
    const departed = previous === null ? [] : departedAgentSelectors(previous, nextAgents);
    previous = nextAgents;
    return departed;
  };
}

// normalizeSeed accepts a seed only if its ws is a same-origin absolute path
// (leading "/"), so neither a crafted hash nor a caller can point the socket at
// an arbitrary host. Returns the seed or null.
export function normalizeSeed(seed) {
  return (seed && typeof seed.ws === 'string' && seed.ws.startsWith('/')) ? seed : null;
}

// mountTerminalWidget is the opaque lifecycle boundary between component-owned
// terminal chrome and xterm's imperative subtree. The caller owns `host` but
// must never render children into it: xterm alone creates and mutates those
// descendants until dispose(). Status, reconnect visibility and selection are
// reported as data so a shell can render its controls without reaching back
// into the widget DOM.
//
// Every asynchronous edge is generation-guarded. dispose() aborts an auth
// preflight, detaches socket handlers before close, disconnects the observer,
// removes document theme listeners, disposes interaction/xterm subscriptions,
// and makes every late callback inert. It is deliberately repeat-safe because
// an explicit close and a component unmount can converge on the same widget.
export function mountTerminalWidget({
  host,
  wsPath,
  authenticate = true,
  active = true,
  onStatus = () => {},
  onReconnectChange = () => {},
  onSelectionChange = () => {},
  onDisconnect = () => {},
  fetchImpl = globalThis.fetch,
  TerminalCtor = globalThis.Terminal,
  FitAddonCtor = globalThis.FitAddon && globalThis.FitAddon.FitAddon,
  WebSocketCtor = globalThis.WebSocket,
  ResizeObserverCtor = globalThis.ResizeObserver,
  locationRef = globalThis.location,
  documentRef = host && host.ownerDocument || globalThis.document,
  interactionsFactory = attachTerminalInteractions,
} = {}) {
  if (!host) throw new TypeError('terminal widget requires a host');
  if (typeof wsPath !== 'string' || !wsPath.startsWith('/')) {
    throw new TypeError('terminal widget requires a same-origin WebSocket path');
  }
  if (typeof TerminalCtor !== 'function' || typeof FitAddonCtor !== 'function') {
    throw new TypeError('terminal widget requires xterm and FitAddon constructors');
  }

  let disposed = false;
  let generation = 0;
  let ws = null;
  let authController = null;
  let isActive = !!active;
  let status = 'disconnected';
  let reconnectAvailable = false;
  const disposables = [];

  const term = new TerminalCtor({
    cursorBlink: true,
    fontSize: 13,
    // The harness owns history: Claude Code renders its own off-screen
    // content, while Codex scrolling is handled by tmux. A second xterm
    // scroll buffer only adds redundant chrome and state.
    scrollback: 0,
    fontFamily: 'ui-monospace, "SF Mono", Menlo, Consolas, monospace',
    theme: terminalThemeFor(documentRef.body.classList.contains('wizard')),
    allowProposedApi: true,
    macOptionClickForcesSelection: true,
  });
  const fitAddon = new FitAddonCtor();
  term.loadAddon(fitAddon);
  term.open(host);

  function setStatus(next) {
    if (disposed) return;
    status = next;
    onStatus(next);
  }

  function setReconnectAvailable(next) {
    if (disposed || reconnectAvailable === !!next) return;
    reconnectAvailable = !!next;
    onReconnectChange(reconnectAvailable);
  }

  function syncTheme() {
    if (disposed) return;
    term.options.theme = terminalThemeFor(documentRef.body.classList.contains('wizard'));
  }
  documentRef.addEventListener('tclaude:wizard', syncTheme);
  documentRef.addEventListener('tclaude:terminal-palette', syncTheme);

  function fit() {
    if (disposed) return;
    try { fitAddon.fit(); } catch (_) { /* host may not be laid out yet */ }
  }

  function focus() {
    if (!disposed) term.focus();
  }

  function sendResize() {
    if (!disposed && ws && ws.readyState === WebSocketCtor.OPEN) {
      ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
    }
  }

  function closeSocket() {
    if (!ws) return;
    const old = ws;
    ws = null;
    old.onclose = null;
    old.onerror = null;
    old.onopen = null;
    old.onmessage = null;
    try { old.close(); } catch (_) { /* already closed */ }
  }

  function abortAuth() {
    if (!authController) return;
    authController.abort();
    authController = null;
  }

  async function connect() {
    if (disposed) return false;
    generation += 1;
    const mine = generation;
    abortAuth();
    closeSocket();
    setReconnectAvailable(false);

    if (authenticate) {
      setStatus('authenticating…');
      authController = new AbortController();
      const controller = authController;
      try {
        const auth = await fetchImpl('/api/auth/session', {
          credentials: 'same-origin', cache: 'no-store', signal: controller.signal,
        });
        if (disposed || mine !== generation) return false;
        if (!auth.ok) {
          setStatus('authentication required');
          setReconnectAvailable(true);
          return false;
        }
      } catch (error) {
        if (disposed || mine !== generation || controller.signal.aborted) return false;
        setStatus('disconnected');
        setReconnectAvailable(true);
        return false;
      } finally {
        if (authController === controller) authController = null;
      }
    }

    if (disposed || mine !== generation) return false;
    const proto = locationRef.protocol === 'https:' ? 'wss:' : 'ws:';
    const socket = new WebSocketCtor(proto + '//' + locationRef.host + wsPath);
    socket.binaryType = 'arraybuffer';
    ws = socket;
    setStatus('connecting…');
    socket.onopen = () => {
      if (disposed || mine !== generation || ws !== socket) return;
      setStatus('connected');
      setReconnectAvailable(false);
      if (isActive) fit();
      sendResize();
    };
    socket.onmessage = (event) => {
      if (disposed || mine !== generation || ws !== socket) return;
      term.write(event.data instanceof ArrayBuffer ? new Uint8Array(event.data) : event.data);
    };
    socket.onclose = () => {
      if (disposed || mine !== generation || ws !== socket) return;
      setStatus('disconnected');
      setReconnectAvailable(true);
      onDisconnect();
    };
    socket.onerror = () => {
      if (disposed || mine !== generation || ws !== socket) return;
      try { socket.close(); } catch (_) { /* onclose handles it */ }
    };
    return true;
  }

  const interactions = interactionsFactory({
    term,
    host,
    copyButton: null,
    setStatus,
    baseStatus: () => ws && ws.readyState === WebSocketCtor.OPEN ? 'connected' : 'disconnected',
    onSelectionChange: (selected) => { if (!disposed) onSelectionChange(selected); },
  });

  disposables.push(term.onData((data) => {
    if (!disposed && ws && ws.readyState === WebSocketCtor.OPEN) {
      ws.send(new TextEncoder().encode(data));
    }
  }));
  disposables.push(term.onResize(sendResize));

  const observer = typeof ResizeObserverCtor === 'function'
    ? new ResizeObserverCtor(() => { if (!disposed && isActive) fit(); })
    : null;
  observer?.observe(host);

  return Object.freeze({
    connect,
    fit,
    focus,
    sendResize,
    copy: () => disposed ? Promise.resolve() : interactions.copySelection(),
    setActive(next) {
      if (disposed) return;
      isActive = !!next;
      if (isActive) {
        fit();
        focus();
        sendResize();
      }
    },
    status: () => status,
    reconnectAvailable: () => reconnectAvailable,
    isDisposed: () => disposed,
    dispose() {
      if (disposed) return;
      disposed = true;
      generation += 1;
      abortAuth();
      closeSocket();
      documentRef.removeEventListener('tclaude:wizard', syncTheme);
      documentRef.removeEventListener('tclaude:terminal-palette', syncTheme);
      try { observer?.disconnect(); } catch (_) { /* already disconnected */ }
      try { interactions.dispose(); } catch (_) { /* already disposed */ }
      for (const disposable of disposables) {
        try { disposable?.dispose(); } catch (_) { /* xterm may own it too */ }
      }
      try { term.dispose(); } catch (_) { /* already disposed */ }
    },
  });
}

// mountMux wires a multiplexer onto the given containers and returns a small
// controller ({ openPane, closePane, count }). Options:
//   tabsEl      — the tab-strip container (unused in solo mode; may be null).
//   panesEl     — the pane-stack container (required).
//   emptyEl     — the "no terminals" placeholder (optional).
//   solo        — true renders a single fixed terminal: no tab strip, no
//                 pop-out button (it IS the popped-out view).
//   manageTitle — true lets the active pane drive document.title. The
//                 standalone page sets it; the in-dashboard tab must NOT, or it
//                 would clobber the dashboard's own title.
//   onCount     — called with the live pane count whenever it changes, so a
//                 mount can react (the in-dashboard tab shows/hides itself off
//                 this).
export function mountMux({ tabsEl, panesEl, emptyEl = null, solo = false, manageTitle = false, onCount = () => {}, onComposeMessage = null }) {
  // key -> pane object. The key dedupes opens: opening the same agent's "web
  // window" twice focuses the existing pane instead of stacking a duplicate.
  const panes = new Map();
  let activeKey = null;
  let seq = 0;
  let unloadGuardArmed = false;

  function wizardActive() {
    return document.body.classList.contains('wizard');
  }

  // The persisted palette choice is global, even though its compact checkbox
  // appears in each pane header beside Copy / ⧉ tab. Repaint every xterm and
  // mirror every checkbox together so switching panes never shows stale state.
  function syncTerminalTheme() {
    const wizard = wizardActive();
    const enabled = arcanePaletteEnabled();
    const theme = terminalThemeFor(wizard, enabled);
    for (const p of panes.values()) {
      p.term.options.theme = theme;
      p.wrap.classList.toggle('arcane-palette', wizard && enabled);
      if (p.paletteToggle) p.paletteToggle.hidden = !wizard;
      if (p.paletteCheckbox) p.paletteCheckbox.checked = enabled;
    }
  }

  // Wizard mode can flip while panes are live, and another pane's checkbox can
  // change the shared preference. Both are repaint-only: no PTY reconnect and
  // no terminal-buffer mutation.
  document.addEventListener('tclaude:wizard', syncTerminalTheme);
  document.addEventListener('tclaude:terminal-palette', syncTerminalTheme);

  // Browsers reserve Ctrl/Cmd+W, so a page cannot reliably turn that shortcut
  // into "close this pane". beforeunload is the supported protection against
  // accidentally losing an open terminal view: supported browsers can ask
  // whether to leave for tab close, reload, and navigation alike. Keep the
  // listener strictly scoped to a non-empty mux so an idle dashboard never
  // nags or forfeits Firefox's back/forward cache merely because terminal
  // support was initialized.
  function confirmTerminalUnload(e) {
    e.preventDefault();
    // Legacy fallback for browsers that do not trigger the prompt from
    // preventDefault alone. The browser owns the prompt text either way.
    e.returnValue = true;
  }

  function updateUnloadGuard(n) {
    const shouldArm = n > 0;
    if (shouldArm === unloadGuardArmed) return;
    unloadGuardArmed = shouldArm;
    window[shouldArm ? 'addEventListener' : 'removeEventListener']('beforeunload', confirmTerminalUnload);
  }

  // Authentication expiry is an intentional navigation to the sign-in page,
  // not an accidental attempt to abandon a live terminal. Do not interpose the
  // browser's generic beforeunload confirmation on that recovery path.
  window.addEventListener('tclaude:auth-expired', () => updateUnloadGuard(0));

  function setStatus(p, text) { if (p.statusEl) p.statusEl.textContent = text; }

  function updateChrome() {
    const n = panes.size;
    if (emptyEl) emptyEl.style.display = n === 0 ? '' : 'none';
    if (tabsEl) tabsEl.style.display = (solo || n === 0) ? 'none' : '';
    updateUnloadGuard(n);
    onCount(n);
  }

  function activate(key) {
    const p = panes.get(key);
    if (!p) return false;
    activeKey = key;
    for (const [k, q] of panes) {
      const on = k === key;
      q.wrap.classList.toggle('active', on);
      if (q.tab) {
        q.tab.classList.toggle('active', on);
        q.tab.setAttribute('aria-selected', on ? 'true' : 'false');
      }
    }
    // Fit now that it's visible. A background pane was left at its previous
    // size (fitting a display:none element measures 0×0 and is meaningless), so
    // its PTY catches up to the real viewport only when it becomes active.
    fit(p);
    p.term.focus();
    sendResize(p);
    if (manageTitle) {
      document.title = (p.label ? p.label + ' — ' : '') + 'tclaude terminals';
    }
    return true;
  }

  // activePaneDescriptor is the small ownership seam used by the integrated
  // Terminals tab's page-level Ctrl/Cmd+M shortcut. Keep the live pane object
  // private; callers only need its stable key (to restore focus after the
  // composer closes) and seed (to identify the mailbox recipient).
  function activePaneDescriptor() {
    const p = panes.get(activeKey);
    return p ? { key: p.key, seed: p.seed } : null;
  }

  function fit(p) {
    try { p.fitAddon.fit(); } catch (_) { /* container not laid out yet */ }
  }

  function sendResize(p) {
    if (p.ws && p.ws.readyState === WebSocket.OPEN) {
      p.ws.send(JSON.stringify({ type: 'resize', cols: p.term.cols, rows: p.term.rows }));
    }
  }

  function closeSocket(p) {
    if (!p.ws) return;
    const old = p.ws;
    p.ws = null;
    // Detach every handler before closing so a late event on this now-orphaned
    // socket can't reach back through the pane and act on its replacement.
    old.onclose = null; old.onerror = null; old.onopen = null; old.onmessage = null;
    try { old.close(); } catch (_) { /* already closed */ }
  }

  // hideOnDetach runs the RELIABLE server-side tmux detach for a pane that
  // attached to an agent's LIVE session (seed.hideConv set) — the same
  // /api/hide the modal and the per-agent "hide" eye button use
  // (DetachSessionClients → tmux detach-client for every client on the
  // session). tclaude forks the tmux client, and closing the WebSocket alone
  // does NOT reliably detach it, so without this the session stays "attached"
  // and the next attach fails. Best-effort; returns the promise so callers can
  // sequence a reattach after the detach lands. A no-op for the ad hoc web-term
  // (its own throwaway session, no agent client to hand back).
  function hideOnDetach(p) {
    const conv = p.seed && p.seed.hideConv;
    if (!conv) return Promise.resolve();
    return fetch('/api/hide/' + encodeURIComponent(conv), { method: 'POST', credentials: 'same-origin' })
      .then((res) => { if (!res.ok) console.warn('terminal detach (hide) failed:', res.status); })
      .catch((e) => { console.warn('terminal detach (hide) request error:', e); });
  }

  async function connect(p) {
    closeSocket(p);
    setStatus(p, 'authenticating…');
    if (p.reconnectBtn) p.reconnectBtn.style.display = 'none';
    try {
      const auth = await fetch('/api/auth/session', { credentials: 'same-origin', cache: 'no-store' });
      if (!auth.ok) {
        setStatus(p, 'authentication required');
        if (p.reconnectBtn) p.reconnectBtn.style.display = '';
        return;
      }
    } catch (_) {
      setStatus(p, 'disconnected');
      if (p.reconnectBtn) p.reconnectBtn.style.display = '';
      return;
    }
    // The pane may have been closed while the auth preflight was in flight.
    if (!panes.has(p.key)) return;
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(proto + '//' + location.host + p.seed.ws);
    ws.binaryType = 'arraybuffer';
    p.ws = ws;
    setStatus(p, 'connecting…');
    ws.onopen = () => {
      setStatus(p, 'connected');
      if (activeKey === p.key) fit(p);
      sendResize(p);
    };
    ws.onmessage = (e) => {
      p.term.write(e.data instanceof ArrayBuffer ? new Uint8Array(e.data) : e.data);
    };
    ws.onclose = () => {
      // Ignore a late close from a socket we've already replaced.
      if (p.ws !== ws) return;
      setStatus(p, 'disconnected');
      // Per-pane, non-blocking reconnect affordance. The modal uses a blocking
      // confirmModal on drop; that doesn't fit a multi-pane surface, where one
      // pane dropping must not lock the others. The underlying tmux/PTY session
      // keeps running, so reconnect reattaches to the same shell.
      if (p.reconnectBtn) p.reconnectBtn.style.display = '';
    };
    ws.onerror = () => { try { ws.close(); } catch (_) { /* onclose handles it */ } };
  }

  function openPane(raw) {
    const seed = normalizeSeed(raw);
    if (!seed) return;
    const key = seedKey(seed);
    if (panes.has(key)) { activate(key); return; }

    const label = seed.label || 'terminal';
    seq += 1;

    const wrap = document.createElement('div');
    wrap.className = 'mux-pane';
    wrap.id = 'mux-pane-' + seq;

    const header = document.createElement('div');
    header.className = 'mux-pane-header';

    const titleEl = document.createElement('span');
    titleEl.className = 'mux-pane-title';
    titleEl.textContent = label;

    const statusEl = document.createElement('span');
    statusEl.className = 'mux-pane-status';
    statusEl.setAttribute('role', 'status');
    statusEl.setAttribute('aria-live', 'polite');
    statusEl.setAttribute('aria-atomic', 'true');

    const hintEl = document.createElement('span');
    hintEl.className = 'terminal-interaction-hint';
    hintEl.textContent = 'Select: Option-drag (macOS) / Shift-drag (Linux/Windows) · Copy: Ctrl/Cmd+Shift+C';

    const reconnectBtn = document.createElement('button');
    reconnectBtn.className = 'mux-btn';
    reconnectBtn.textContent = 'Reconnect';
    reconnectBtn.style.display = 'none';

    header.append(titleEl, statusEl, hintEl, reconnectBtn);

    const copyBtn = document.createElement('button');
    copyBtn.className = 'mux-btn';
    copyBtn.textContent = 'Copy';
    header.append(copyBtn);

    let messageBtn = null;
    if (seed.agent && onComposeMessage) {
      messageBtn = document.createElement('button');
      messageBtn.className = 'mux-btn';
      messageBtn.textContent = '✉ Message';
      messageBtn.title = 'Send a queued message to this agent (Ctrl/Cmd+M)';
      header.append(messageBtn);
    }

    const paletteToggle = document.createElement('label');
    paletteToggle.className = 'mux-palette-toggle';
    paletteToggle.title = 'Recolour terminal defaults with the persisted wizard palette; explicit application RGB colours are unchanged';
    paletteToggle.hidden = !wizardActive();
    const paletteCheckbox = document.createElement('input');
    paletteCheckbox.type = 'checkbox';
    paletteCheckbox.checked = arcanePaletteEnabled();
    paletteCheckbox.setAttribute('aria-label', 'Arcane terminal palette');
    const paletteLabel = document.createElement('span');
    paletteLabel.textContent = 'Arcane palette';
    paletteToggle.append(paletteCheckbox, paletteLabel);
    header.append(paletteToggle);

    // Pop-out is meaningless in a solo tab (it IS the popped-out view).
    let popBtn = null;
    if (!solo) {
      popBtn = document.createElement('button');
      popBtn.className = 'mux-btn';
      popBtn.textContent = '⧉ tab';
      popBtn.title = 'Move this terminal to its own browser tab';
      header.append(popBtn);
    }

    const host = document.createElement('div');
    host.className = 'mux-pane-xterm';
    const fitHost = document.createElement('div');
    fitHost.className = 'mux-pane-xterm-fit';
    host.append(fitHost);

    wrap.append(header, host);
    panesEl.append(wrap);

    const term = new Terminal({
      cursorBlink: true, fontSize: 13,
      // The harness owns history: Claude Code renders its own off-screen
      // content, while Codex scrolling is handled by tmux. A second xterm
      // scroll buffer only adds a redundant scrollbar and width reservation.
      scrollback: 0,
      fontFamily: 'ui-monospace, "SF Mono", Menlo, Consolas, monospace',
      theme: terminalThemeFor(wizardActive()), allowProposedApi: true,
      // xterm uses Option (not Shift) to force browser selection on macOS,
      // and ignores Option unless this is explicitly enabled.
      macOptionClickForcesSelection: true,
    });
    const fitAddon = new FitAddon.FitAddon();
    term.loadAddon(fitAddon);
    term.open(fitHost);

    const p = {
      key, label, seed, term, fitAddon, ws: null, wrap, statusEl,
      reconnectBtn, copyBtn, paletteToggle, paletteCheckbox,
      tab: null, ro: null, interactions: null,
    };

    p.interactions = attachTerminalInteractions({
      term, host, copyButton: copyBtn, setStatus: (text) => setStatus(p, text),
      baseStatus: () => p.ws && p.ws.readyState === WebSocket.OPEN ? 'connected' : 'disconnected',
      onComposeMessage: messageBtn ? () => onComposeMessage(seed) : null,
    });

    // Keystrokes go over the wire as binary frames — never text — so the
    // server's resize-control check (which only parses TextMessage frames) can
    // never mistake typed input for a {"type":"resize",…} command. Same
    // contract as the fallback terminal modal.
    term.onData((d) => { if (p.ws && p.ws.readyState === WebSocket.OPEN) p.ws.send(new TextEncoder().encode(d)); });
    term.onResize(() => sendResize(p));
    p.ro = new ResizeObserver(() => { if (activeKey === key) fit(p); });
    p.ro.observe(fitHost);

    if (!solo) {
      // The pane wrap is this tab's panel — link them so assistive tech pairs
      // the tab with the terminal it controls.
      wrap.setAttribute('role', 'tabpanel');
      const tab = document.createElement('div');
      tab.className = 'mux-tab';
      tab.setAttribute('role', 'tab');
      // Keyboard-operable + AT-exposed: focusable, activatable with Enter/Space,
      // and aria-selected kept in sync by activate().
      tab.tabIndex = 0;
      tab.setAttribute('aria-selected', 'false');
      tab.setAttribute('aria-controls', wrap.id);
      const tl = document.createElement('span');
      tl.className = 'mux-tab-label';
      tl.textContent = label;
      const tc = document.createElement('button');
      tc.className = 'mux-tab-close';
      tc.textContent = '×';
      tc.title = 'Close this terminal';
      tc.setAttribute('aria-label', 'Close ' + label);
      tab.append(tl, tc);
      tab.addEventListener('click', (e) => { if (e.target !== tc) activate(key); });
      tab.addEventListener('keydown', (e) => {
        // Enter / Space activate the tab (Space also scrolls by default).
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); activate(key); }
      });
      tc.addEventListener('click', (e) => { e.stopPropagation(); closePane(key); });
      tabsEl.append(tab);
      p.tab = tab;
    }

    if (popBtn) popBtn.addEventListener('click', () => popOut(key));
    if (messageBtn) messageBtn.addEventListener('click', () => onComposeMessage(seed));
    reconnectBtn.addEventListener('click', () => connect(p));
    paletteCheckbox.addEventListener('change', () => setArcanePaletteEnabled(paletteCheckbox.checked));

    panes.set(key, p);
    syncTerminalTheme();
    updateChrome();
    activate(key);
    connect(p);
  }

  // closePane closes (and, for a live-session attach, detaches) a pane. Async:
  // it resolves only once the server-side detach has LANDED, so a caller that
  // must reattach the same session afterwards (popOut) can await it and avoid
  // racing the detach. Fire-and-forget callers (the × button) simply don't
  // await — the pane's DOM is torn down synchronously before the await, so it
  // vanishes immediately either way.
  async function closePane(key, opts) {
    const p = panes.get(key);
    if (!p) return;
    // Close the socket AND, for a live-session attach (seed.hideConv), run the
    // reliable server-side tmux detach — otherwise the forked client stays
    // attached and reopening fails. closeSocket first (it nulls the handlers) so
    // the detach's server-side WS close lands silently. The tmux/PTY session
    // keeps running, so reopening reattaches to the same shell. opts.skipDetach
    // suppresses the detach when the close is a REACTION to an external hide
    // (eye button / palette / bulk unfocus) that already detached server-side —
    // re-running /api/hide then would be redundant.
    closeSocket(p);
    const detached = (opts && opts.skipDetach) ? null : hideOnDetach(p);
    if (p.ro) { try { p.ro.disconnect(); } catch (_) { /* already gone */ } }
    if (p.interactions) { try { p.interactions.dispose(); } catch (_) { /* already disposed */ } }
    try { p.term.dispose(); } catch (_) { /* already disposed */ }
    p.wrap.remove();
    if (p.tab) p.tab.remove();
    panes.delete(key);
    if (activeKey === key) {
      activeKey = null;
      const next = panes.keys().next();
      if (!next.done) activate(next.value);
      else if (manageTitle) document.title = 'tclaude terminals';
    }
    updateChrome();
    // Let a caller (popOut) sequence a reattach strictly after the detach.
    if (detached) await detached;
  }

  async function popOut(key) {
    const p = panes.get(key);
    if (!p) return;
    // Carry hideConv through so the popped-out tab remains a detachable
    // live-session client (it re-serializes the seed via the URL hash).
    const seed = {
      ws: p.seed.ws, label: p.label, key: p.seed.key,
      hideConv: p.seed.hideConv, wizard: wizardActive(),
    };
    const payload = encodeURIComponent(JSON.stringify(seed));
    // Open a BLANK tab synchronously, inside the click gesture, so a pop-up
    // blocker can't eat it — but DON'T navigate it to the terminal yet. A
    // blocked pop-up (or a throw) returns null/undefined; closing the pane then
    // would make the terminal vanish with nowhere to go (the tmux/PTY session
    // survives, but a silent block shouldn't lose the visible pane), so bail.
    let win = null;
    try { win = window.open('about:blank', '_blank'); }
    catch (_) { win = null; }
    if (!win) return;
    // Detach THIS pane's tmux client and WAIT for the /api/hide to land BEFORE
    // the new tab attaches — /api/hide detaches every client on the session, so
    // navigating first could let the detach drop the freshly-reattached client.
    // solo=1 strips the standalone page down to just this one terminal.
    await closePane(key);
    try { win.location.replace('/terminals?solo=1#open=' + payload); }
    catch (_) { /* user closed the blank tab mid-detach — nothing to navigate */ }
  }

  updateChrome();

  // closeForHide closes every live-session pane (seed.hideConv set) whose
  // selector is in `selectors` — the reaction to an EXTERNAL hide/detach (the
  // per-agent eye button, the command palette, a bulk unfocus). The detach
  // already happened server-side, so panes close WITHOUT re-running /api/hide
  // (skipDetach). Throwaway web-term panes (no hideConv) are never matched.
  function closeForHide(selectors) {
    const set = new Set(selectors || []);
    if (!set.size) return;
    // Snapshot the entries — closePane mutates `panes` under us.
    for (const [key, p] of [...panes]) {
      const hc = p.seed && p.seed.hideConv;
      if (hc && set.has(hc)) closePane(key, { skipDetach: true });
    }
  }

  // closeForAgents closes every per-agent pane (both a live-session "web
  // window" and a throwaway "web term") whose seed.agent matches one of the
  // selectors. Unlike closeForHide, this is not reacting to a detach that has
  // already happened, so closePane keeps its normal reliable detach for live
  // sessions. Group terminals have no seed.agent and are intentionally left
  // alone.
  function closeForAgents(selectors) {
    const set = new Set(selectors || []);
    if (!set.size) return;
    for (const [key, p] of [...panes]) {
      const agent = p.seed && p.seed.agent;
      if (agent && set.has(agent)) closePane(key);
    }
  }

  // findPaneKey returns the key of the FIRST open pane belonging to an agent in
  // `selectors` (matched on seed.agent — set for BOTH web-term and web-window
  // panes), or null. Lets a caller jump to an already-open in-browser terminal
  // instead of raising a native OS window.
  function findPaneKey(selectors) {
    const set = new Set(selectors || []);
    if (!set.size) return null;
    for (const [key, p] of panes) {
      const a = p.seed && p.seed.agent;
      if (a && set.has(a)) return key;
    }
    return null;
  }

  return {
    openPane,
    closePane,
    closeForHide,
    closeForAgents,
    findPaneKey,
    activePaneDescriptor,
    activatePane: activate,
    count: () => panes.size,
  };
}
