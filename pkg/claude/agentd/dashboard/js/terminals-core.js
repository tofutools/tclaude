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
// in-dashboard modal (modal-term.js) connects to. Background panes stay live:
// their socket keeps writing into the off-screen xterm buffer, so switching to
// a pane (or back to the tab) shows the up-to-date terminal.
//
// Terminal / FitAddon are globals from the vendored classic xterm scripts
// loaded before the module graph (both dashboard.html and terminals.html load
// them). The core imports nothing from the dashboard SPA, so the standalone
// page stays free of dashboard.css / helpers.js.

const THEME = {
  background: '#0d1117', foreground: '#c9d1d9', cursor: '#c9d1d9',
  selectionBackground: 'rgba(255,255,255,0.2)',
};

function seedKey(seed) {
  return seed.key || seed.ws;
}

// normalizeSeed accepts a seed only if its ws is a same-origin absolute path
// (leading "/"), so neither a crafted hash nor a caller can point the socket at
// an arbitrary host. Returns the seed or null.
export function normalizeSeed(seed) {
  return (seed && typeof seed.ws === 'string' && seed.ws.startsWith('/')) ? seed : null;
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
export function mountMux({ tabsEl, panesEl, emptyEl = null, solo = false, manageTitle = false, onCount = () => {} }) {
  // key -> pane object. The key dedupes opens: opening the same agent's "web
  // window" twice focuses the existing pane instead of stacking a duplicate.
  const panes = new Map();
  let activeKey = null;
  let seq = 0;

  function setStatus(p, text) { if (p.statusEl) p.statusEl.textContent = text; }

  function updateChrome() {
    const n = panes.size;
    if (emptyEl) emptyEl.style.display = n === 0 ? '' : 'none';
    if (tabsEl) tabsEl.style.display = (solo || n === 0) ? 'none' : '';
    onCount(n);
  }

  function activate(key) {
    const p = panes.get(key);
    if (!p) return;
    activeKey = key;
    for (const [k, q] of panes) {
      const on = k === key;
      q.wrap.classList.toggle('active', on);
      if (q.tab) q.tab.classList.toggle('active', on);
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

  function connect(p) {
    closeSocket(p);
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(proto + '//' + location.host + p.seed.ws);
    ws.binaryType = 'arraybuffer';
    p.ws = ws;
    setStatus(p, 'connecting…');
    if (p.reconnectBtn) p.reconnectBtn.style.display = 'none';
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

    const reconnectBtn = document.createElement('button');
    reconnectBtn.className = 'mux-btn';
    reconnectBtn.textContent = 'Reconnect';
    reconnectBtn.style.display = 'none';

    header.append(titleEl, statusEl, reconnectBtn);

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

    wrap.append(header, host);
    panesEl.append(wrap);

    const term = new Terminal({
      cursorBlink: true, fontSize: 13,
      fontFamily: 'ui-monospace, "SF Mono", Menlo, Consolas, monospace',
      theme: THEME, allowProposedApi: true,
    });
    const fitAddon = new FitAddon.FitAddon();
    term.loadAddon(fitAddon);
    term.open(host);

    const p = { key, label, seed, term, fitAddon, ws: null, wrap, statusEl, reconnectBtn, tab: null, ro: null };

    // Keystrokes go over the wire as binary frames — never text — so the
    // server's resize-control check (which only parses TextMessage frames) can
    // never mistake typed input for a {"type":"resize",…} command. Same
    // contract as modal-term.js.
    term.onData((d) => { if (p.ws && p.ws.readyState === WebSocket.OPEN) p.ws.send(new TextEncoder().encode(d)); });
    term.onResize(() => sendResize(p));
    p.ro = new ResizeObserver(() => { if (activeKey === key) fit(p); });
    p.ro.observe(host);

    if (!solo) {
      const tab = document.createElement('div');
      tab.className = 'mux-tab';
      tab.setAttribute('role', 'tab');
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
      tc.addEventListener('click', (e) => { e.stopPropagation(); closePane(key); });
      tabsEl.append(tab);
      p.tab = tab;
    }

    if (popBtn) popBtn.addEventListener('click', () => popOut(key));
    reconnectBtn.addEventListener('click', () => connect(p));

    panes.set(key, p);
    updateChrome();
    activate(key);
    connect(p);
  }

  function closePane(key) {
    const p = panes.get(key);
    if (!p) return;
    // Closing only detaches the socket; the underlying tmux/PTY session keeps
    // running, so reopening this agent reattaches to the same shell.
    closeSocket(p);
    if (p.ro) { try { p.ro.disconnect(); } catch (_) { /* already gone */ } }
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
  }

  function popOut(key) {
    const p = panes.get(key);
    if (!p) return;
    const seed = { ws: p.seed.ws, label: p.label, key: p.seed.key };
    const payload = encodeURIComponent(JSON.stringify(seed));
    // A fresh, UNNAMED tab so it's independent; solo=1 strips the standalone
    // page down to just this one terminal (its own OS/browser window).
    window.open('/terminals?solo=1#open=' + payload, '_blank');
    closePane(key);
  }

  updateChrome();

  return {
    openPane,
    closePane,
    count: () => panes.size,
  };
}
