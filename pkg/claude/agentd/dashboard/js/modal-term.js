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
import { confirmModal } from './refresh.js';

let term = null;
let fitAddon = null;
let ws = null;
let currentWsPath = null;
// True while ANY term-modal confirmation (the disconnect prompt OR the
// backdrop "Close terminal?" confirm) is open. confirmModal is a shared
// singleton (one #confirm-modal element); opening a second over a pending
// first would double up its button/Escape listeners so one click resolves
// both promises — clicking "Reconnect" could then close the terminal, and a
// stranded promise could wedge the disconnect prompt for the page's life.
// This single in-flight guard keeps the two confirms mutually exclusive.
let termConfirmOpen = false;

// openTermModal opens the modal and (re)connects an xterm.js terminal
// to wsPath over a WebSocket. label is shown in the modal title bar.
// The underlying tmux/tclaude session outlives the modal — closing it
// just detaches the WebSocket, reopening reattaches to the same shell.
export function openTermModal({ wsPath, label }) {
  currentWsPath = wsPath;
  // Defensive reset: a fresh open should never inherit a stuck guard from a
  // previous session (it can't with the mutual-exclusion below, but this
  // costs nothing and guarantees a reopened modal can always prompt again).
  termConfirmOpen = false;
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
  // term is a reused singleton: clear the previous session's scrollback so
  // reopening the modal for a different agent never flashes stale output
  // under the new title before the fresh PTY redraws.
  term.reset();
  fitAddon.fit();
  // Move keyboard focus into xterm so the modal is usable immediately —
  // without this, keyboard users have to click inside before they can type.
  term.focus();
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
    // Don't silently reconnect — a dropped connection often means the
    // shell/session ended, and a quiet retry loop hides that. Ask instead.
    // closeSocket() nulls this handler before any INTENTIONAL close (the ×
    // button, backdrop confirm, or a reconnect), so this only fires on a
    // genuine drop.
    setStatus('disconnected');
    promptReconnect();
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

// promptReconnect asks the human what to do after an unexpected drop:
// reconnect to the same session, or close the modal. Escape / the cancel
// button both close (the connection is already dead). Bails if a term-modal
// confirm is already open (the shared-singleton guard) so a burst of
// close/error events — or a drop landing while the backdrop "Close terminal?"
// confirm is up — can't stack a second dialog. When it bails for the latter
// reason, the backdrop handler re-offers the reconnect once its own confirm
// resolves, so the prompt is deferred, not lost.
async function promptReconnect() {
  if (termConfirmOpen) return;
  termConfirmOpen = true;
  let reconnect;
  try {
    reconnect = await confirmModal({
      title: 'Terminal disconnected',
      body: 'The connection to the terminal was closed. The underlying session keeps running — reconnect to it, or close this terminal?',
      okLabel: 'Reconnect',
      cancelLabel: 'Close terminal',
    });
  } finally {
    termConfirmOpen = false;
  }
  // The modal may have been closed out from under the prompt; don't
  // resurrect a connection for a terminal the user already dismissed.
  if (!$('#term-session-modal').classList.contains('show')) return;
  if (reconnect) connect();
  else closeTermModal();
}

function closeSocket() {
  if (ws) {
    const old = ws;
    ws = null;
    // Detach EVERY handler before closing: onclose so an intentional close
    // doesn't trigger the reconnect prompt, and onerror/onopen/onmessage so a
    // late event on this now-orphaned socket can't reach back through the
    // module-level `ws` and act on its replacement (connect() installs a
    // fresh socket right after this).
    old.onclose = null;
    old.onerror = null;
    old.onopen = null;
    old.onmessage = null;
    old.close();
  }
}

export function closeTermModal() {
  closeSocket();
  $('#term-session-modal').classList.remove('show');
}

// bindTermModal wires the close button / backdrop click. Called once at
// dashboard init (dashboard.js).
//
// The × button is the explicit, deliberate close — no confirm. A backdrop
// click is easy to do by accident while reaching for the terminal, so it
// asks first rather than tearing the session view down. Escape is NOT a
// close key here: it's a control character the terminal itself needs
// (vim, less, and the Claude Code TUI all lean on it), so it must pass
// straight through to xterm. (The disconnect prompt's own confirmModal
// still handles Escape = cancel while it's up.)
export function bindTermModal() {
  const overlay = $('#term-session-modal');
  $('#term-session-close').addEventListener('click', closeTermModal);
  overlay.addEventListener('click', async (e) => {
    if (e.target !== overlay) return;
    // Share the disconnect prompt's in-flight guard (confirmModal is a single
    // shared element). If a confirm is already up, ignore the backdrop click.
    if (termConfirmOpen) return;
    termConfirmOpen = true;
    let close;
    try {
      close = await confirmModal({
        title: 'Close terminal?',
        body: 'The underlying session keeps running — you can reopen it to reattach.',
        okLabel: 'Close terminal',
        cancelLabel: 'Keep open',
      });
    } finally {
      termConfirmOpen = false;
    }
    if (close) { closeTermModal(); return; }
    // Kept open: if the socket dropped while this confirm was up (its onclose
    // saw the guard set and skipped the prompt), surface the reconnect choice
    // now instead of leaving a silently-dead terminal on screen. Gate on
    // readyState (not a bool) so a still-CONNECTING socket isn't mistaken for
    // a drop.
    const dropped = ws && (ws.readyState === WebSocket.CLOSED || ws.readyState === WebSocket.CLOSING);
    if (dropped && $('#term-session-modal').classList.contains('show')) promptReconnect();
  });
}
