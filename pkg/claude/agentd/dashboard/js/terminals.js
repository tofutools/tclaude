// terminals.js — the standalone multi-terminal page ("terminals multiplexer").
//
// A separate browser tab/window that holds MANY live xterm.js terminals at
// once, each streamed over a WebSocket to a real PTY on the agentd host —
// the same /api/term-ws/{conv} and /api/open-window-ws/{conv} endpoints the
// in-dashboard modal (modal-term.js) connects to. The modal is a single
// blocking overlay reusing one terminal; this page is non-blocking (its own
// browser tab) and multiplexes N terminals behind a tab strip, with a
// per-terminal "pop out" that re-opens one in its own browser tab.
//
// This module is DELIBERATELY self-contained — it imports nothing from the
// dashboard SPA (helpers.js / refresh.js / …) because it runs on its own page
// without the dashboard DOM. Terminal / FitAddon are globals from the vendored
// classic scripts loaded before this module (same arrangement as modal-term.js
// and dashboard.html).
//
// Entry points:
//   * The dashboard's "web term" / "web window" row actions open/focus this
//     tab (a named window target, so panes accumulate in ONE tab) and hand it
//     a terminal via the URL hash (#open=<json>). See js/terminals-launch.js.
//   * /terminals?solo=1 renders a single terminal with no tab strip — what the
//     pop-out button opens.

const solo = new URLSearchParams(location.search).has('solo');

const tabsEl = document.getElementById('mux-tabs');
const panesEl = document.getElementById('mux-panes');
const emptyEl = document.getElementById('mux-empty');

if (solo) document.body.classList.add('solo');

// key -> pane object. The key dedupes opens: clicking the same agent's "web
// window" twice focuses the existing pane instead of stacking a duplicate.
const panes = new Map();
let activeKey = null;
let seq = 0;

const THEME = {
  background: '#0d1117', foreground: '#c9d1d9', cursor: '#c9d1d9',
  selectionBackground: 'rgba(255,255,255,0.2)',
};

function seedKey(seed) {
  return seed.key || seed.ws;
}

// decodeOpenHash pulls the { ws, label, key } seed out of "#open=<encoded
// json>". Only same-origin absolute WS paths (leading "/") are accepted, so a
// crafted hash can't point the socket at an arbitrary host.
function decodeOpenHash() {
  const m = /[#&]open=([^&]+)/.exec(location.hash || '');
  if (!m) return null;
  try {
    const seed = JSON.parse(decodeURIComponent(m[1]));
    if (seed && typeof seed.ws === 'string' && seed.ws.startsWith('/')) return seed;
  } catch (_) { /* malformed hash — ignore */ }
  return null;
}

function consumeHash() {
  const seed = decodeOpenHash();
  // Clear the hash either way so (a) a manual reload doesn't re-open a stale
  // seed, and (b) the NEXT open — even an identical one — still changes the
  // hash and fires hashchange (the dashboard re-uses this one named tab).
  if (location.hash) history.replaceState(null, '', location.pathname + location.search);
  if (seed) openPane(seed);
}

function setStatus(p, text) { if (p.statusEl) p.statusEl.textContent = text; }

function updateChrome() {
  const n = panes.size;
  if (emptyEl) emptyEl.style.display = n === 0 ? '' : 'none';
  if (tabsEl) tabsEl.style.display = (solo || n === 0) ? 'none' : '';
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
  document.title = (p.label ? p.label + ' — ' : '') + 'tclaude terminals';
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
    // confirmModal on drop; that doesn't fit a multi-pane page, where one
    // pane dropping must not lock the others. The underlying tmux/PTY session
    // keeps running, so reconnect reattaches to the same shell.
    if (p.reconnectBtn) p.reconnectBtn.style.display = '';
  };
  ws.onerror = () => { try { ws.close(); } catch (_) { /* onclose handles it */ } };
}

function openPane(seed) {
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
    else document.title = 'tclaude terminals';
  }
  updateChrome();
}

function popOut(key) {
  const p = panes.get(key);
  if (!p) return;
  const seed = { ws: p.seed.ws, label: p.label, key: p.seed.key };
  const payload = encodeURIComponent(JSON.stringify(seed));
  // A fresh, UNNAMED tab (not the shared multiplexer target) so it's
  // independent; solo=1 strips the tab strip down to just this terminal.
  window.open('/terminals?solo=1#open=' + payload, '_blank');
  closePane(key);
}

window.addEventListener('hashchange', consumeHash);
updateChrome();
consumeHash();
