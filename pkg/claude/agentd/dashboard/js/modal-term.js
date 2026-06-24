// modal-term.js — the in-browser terminal modal: an xterm.js instance
// streamed over a WebSocket to a real PTY on the agentd host.
//
// This is the fallback the "term" / "term-dir" / "open-window" row
// actions (row-actions.js) fall back to when the daemon can't pop a
// native terminal window — no DISPLAY/WAYLAND_DISPLAY (headless
// agentd), or no terminal emulator installed at all. The backend side
// is handleDashboardTermWS / handleDashboardOpenWindowWS
// (dashboard_term.go); wsPath is the path those handlers expect,
// handed back by the POST /api/term or /api/open-window response as
// `ws` when it reports `mode:"browser"`.
//
// xterm.js + the fit addon are vendored (not a CDN, like every other
// dashboard asset) and loaded as plain classic <script> tags in
// dashboard.html — ahead of the `type="module"` script that pulls in
// this file — so `Terminal` / `FitAddon` are plain globals here, not
// ES imports.
//
// Distinct from #term-modal (refresh.js's termDirModal), which is the
// start/current/worktree picker that runs BEFORE this — this is what
// opens once that choice (or a plain open-window click) comes back
// with mode:"browser".

import { $ } from './helpers.js';

let term = null;
let fitAddon = null;
let ws = null;
let reconnectTimer = null;
let currentWsPath = null;

// openTermModal opens the modal and (re)connects an xterm.js terminal
// to wsPath over a WebSocket. label is shown in the modal title bar.
// The underlying tmux/tclaude session outlives the modal — closing it
// just detaches the WebSocket, reopening reattaches to the same shell.
export function openTermModal({ wsPath, label }) {
  currentWsPath = wsPath;
  $('#term-session-title').textContent = label ? `Terminal — ${label}` : 'Terminal';
  $('#term-session-modal').classList.add('show');

  if (!term) {
    term = new Terminal({
      cursorBlink: true,
      fontSize: 13,
      fontFamily: 'ui-monospace, "SF Mono", Menlo, Consolas, monospace',
      theme: {
        background: '#0d1117', foreground: '#c9d1d9', cursor: '#c9d1d9',
        selectionBackground: 'rgba(255,255,255,0.2)',
      },
      allowProposedApi: true,
    });
    fitAddon = new FitAddon.FitAddon();
    term.loadAddon(fitAddon);
    term.open($('#term-session-xterm'));
    // Keystrokes go over the wire as binary frames — never as a text
    // frame — so the server's resize-control-message check (which
    // only inspects TextMessage frames) can never misinterpret typed
    // input as a {"type":"resize",...} command.
    term.onData((data) => {
      if (ws && ws.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(data));
    });
    term.onResize(() => sendResize());
    new ResizeObserver(() => fitAddon.fit()).observe($('#term-session-xterm'));
  }
  fitAddon.fit();
  connect();
}

function connect() {
  closeSocket();
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  ws = new WebSocket(proto + '//' + location.host + currentWsPath);
  ws.binaryType = 'arraybuffer';
  setStatus('connecting…');
  ws.onopen = () => {
    setStatus('connected');
    fitAddon.fit();
    sendResize();
  };
  ws.onmessage = (e) => {
    term.write(e.data instanceof ArrayBuffer ? new Uint8Array(e.data) : e.data);
  };
  ws.onclose = () => {
    setStatus('disconnected — reconnecting…');
    reconnectTimer = setTimeout(connect, 2000);
  };
  ws.onerror = () => { try { ws.close(); } catch (_) { /* onclose handles it */ } };
}

function sendResize() {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
  }
}

function setStatus(text) {
  const el = $('#term-session-status');
  if (el) el.textContent = text;
}

function closeSocket() {
  if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
  if (ws) {
    const old = ws;
    ws = null;
    old.onclose = null;
    old.close();
  }
}

export function closeTermModal() {
  closeSocket();
  $('#term-session-modal').classList.remove('show');
}

// bindTermModal wires the close button / backdrop click / Escape key.
// Called once at dashboard init (dashboard.js).
export function bindTermModal() {
  const overlay = $('#term-session-modal');
  $('#term-session-close').addEventListener('click', closeTermModal);
  overlay.addEventListener('click', (e) => { if (e.target === overlay) closeTermModal(); });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && overlay.classList.contains('show')) closeTermModal();
  });
}
