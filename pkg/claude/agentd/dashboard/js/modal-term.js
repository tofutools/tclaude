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
import { openTerminalPane } from './terminals-tab.js';
import { attachTerminalInteractions } from './terminal-interactions.js';

let term = null;
let fitAddon = null;
let interactions = null;
let ws = null;
let currentWsPath = null;
// The agent conv to detach on close, or null. Set only for the "open window"
// attach (a terminal on the agent's LIVE tmux session), where closing the modal
// must actually detach the tmux client — otherwise the session stays "attached"
// and can't be reattached. We do that by POSTing /api/hide/{conv}, the same
// server-side detach the per-agent "hide" eye button uses. Left null for the ad
// hoc web-term (its own throwaway session, no agent client to hide), where
// closing the WebSocket is enough.
let hideConv = null;
// The label shown in the modal title, remembered so the "⧉ tab" pop-out can
// hand it to the standalone terminals page.
let currentLabel = null;
// True while ANY term-modal confirmation (the disconnect prompt OR the
// detach/close confirm shared by the × button and the backdrop click) is
// open. confirmModal is a shared singleton (one #confirm-modal element);
// opening a second over a pending first would double up its button/Escape
// listeners so one click resolves both promises — clicking "Reconnect" could
// then close the terminal, and a stranded promise could wedge the disconnect
// prompt for the page's life. This single in-flight guard keeps the confirms
// mutually exclusive.
let termConfirmOpen = false;

// openTermModal opens the modal and (re)connects an xterm.js terminal
// to wsPath over a WebSocket. label is shown in the modal title bar.
// The underlying tmux/tclaude session outlives the modal — closing it
// just detaches the WebSocket, reopening reattaches to the same shell.
export function openTermModal({ wsPath, label, hideConv: hc }) {
  // The xterm instance is reused across modal opens. Invalidate any async
  // clipboard-image upload owned by the previous session before changing the
  // module-level WebSocket target.
  if (interactions) interactions.invalidate();
  currentWsPath = wsPath;
  // Only the live-agent "open window" attach passes hideConv; clear any value
  // left over from a previous (possibly web-term) open so a stale conv can't
  // get detached under a different modal.
  hideConv = hc || null;
  currentLabel = label;
  // The Detach button only makes sense for a web WINDOW (a view onto the agent's
  // LIVE session, hideConv set): "detach" hands the tmux client back while the
  // agent keeps running. An ad hoc web TERMINAL (hideConv null) is its own
  // throwaway shell — nothing to detach from — so it gets only the × Close.
  // Toggle per open; the modal is a reused singleton. (The "⧉ tab" pop-out
  // stays visible for both — any terminal can move to the terminals page.)
  $('#term-session-detach').style.display = hideConv ? '' : 'none';
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
      // xterm uses Option (not Shift) to force browser selection on macOS,
      // and ignores Option unless this is explicitly enabled.
      macOptionClickForcesSelection: true,
    });
    fitAddon = new FitAddon.FitAddon();
    term.loadAddon(fitAddon);
    term.open($('#term-session-xterm'));
    interactions = attachTerminalInteractions({
      term,
      host: $('#term-session-xterm'),
      copyButton: $('#term-session-copy'),
      setStatus,
      baseStatus: () => ws && ws.readyState === WebSocket.OPEN ? 'connected' : 'disconnected',
    });
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
// close/error events — or a drop landing while the backdrop detach/close
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
  if (interactions) interactions.invalidate();
  closeSocket();
  $('#term-session-modal').classList.remove('show');
}

// detachAndClose closes the modal and then runs the RELIABLE server-side
// detach. For a live-agent "open window" (hideConv set) it POSTs
// /api/hide/{conv} — the exact server-side detach the per-agent "hide" eye
// button uses (DetachSessionClients → tmux detach-client for every client on
// the session). We do this explicitly instead of relying on the WebSocket close
// to detach the tmux client: for the open-window attach (tclaude forks the tmux
// client) that implicit teardown didn't reliably detach, leaving the session
// stuck "attached" so it couldn't be reattached. For the ad hoc web-term
// (hideConv null) there's no agent client to hide, so it just closes.
//
// ORDER MATTERS — closeTermModal() runs FIRST so closeSocket() nulls ws.onclose
// before the hide can land. /api/hide detaches THIS window's own client, which
// EOFs the PTY and makes agentd close the WebSocket; if onclose were still armed
// when that close frame arrives (we'd be parked on the await), it would fire
// promptReconnect() and pop a spurious "Terminal disconnected" dialog over the
// just-closed modal. Closing first makes the server-side close silent — and the
// view drops instantly, which is what the click asked for. Best-effort: a hide
// error is logged but never reopens the view.
async function detachAndClose() {
  closeTermModal();
  if (hideConv) {
    try {
      const res = await fetch(`/api/hide/${encodeURIComponent(hideConv)}`, {
        method: 'POST', credentials: 'same-origin',
      });
      if (!res.ok) console.warn('term modal detach (hide) failed:', res.status, await res.text().catch(() => ''));
    } catch (e) {
      console.warn('term modal detach (hide) request error:', e);
    }
  }
}

// confirmAndClose runs the ask-first confirm and, if the human accepts, runs
// detachAndClose. Shared by the × Close button and the backdrop click — both are
// the cautious, ask-first path (a plain × press, and an outside click while
// reaching for the terminal, are both easy accidents). The Detach button skips
// the confirm (detachAndClose directly): detaching is the deliberate "drop my
// view now" action, so it needs no confirmation.
//
// The copy depends on which kind of view this is, keyed off hideConv:
//   • Web window (hideConv set — a terminal on the agent's LIVE tmux session):
//     closing detaches the tmux client via /api/hide while the agent keeps
//     running, so it asks to DETACH, not to shut down.
//   • Ad hoc web terminal (hideConv null — its own throwaway shell, no agent
//     client to hand back): closing just drops the WebSocket, so it asks to
//     CLOSE, exactly as it did before.
//
// Shares the disconnect prompt's in-flight guard (confirmModal is a single
// shared element); if a confirm is already up, this is a no-op.
async function confirmAndClose() {
  if (termConfirmOpen) return;
  termConfirmOpen = true;
  let close;
  try {
    close = await confirmModal(hideConv ? {
      title: 'Detach terminal?',
      body: 'This only drops your view — the agent keeps running, and you can reopen it to reattach.',
      okLabel: 'Detach',
      cancelLabel: 'Keep open',
    } : {
      title: 'Close terminal?',
      body: 'The underlying session keeps running — you can reopen it to reattach.',
      okLabel: 'Close terminal',
      cancelLabel: 'Keep open',
    });
  } finally {
    termConfirmOpen = false;
  }
  if (close) { await detachAndClose(); return; }
  // Kept open: if the socket dropped while this confirm was up (its onclose
  // saw the guard set and skipped the prompt), surface the reconnect choice
  // now instead of leaving a silently-dead terminal on screen. Gate on
  // readyState (not a bool) so a still-CONNECTING socket isn't mistaken for
  // a drop.
  const dropped = ws && (ws.readyState === WebSocket.CLOSED || ws.readyState === WebSocket.CLOSING);
  if (dropped && $('#term-session-modal').classList.contains('show')) promptReconnect();
}

// bindTermModal wires the header buttons + backdrop click. Called once at
// dashboard init (dashboard.js).
//
// Two deliberate-close affordances, genuinely different by intent — but both
// run the same reliable detach (detachAndClose → /api/hide for an open-window
// attach) and leave the underlying agent session running. The only difference
// is the confirmation gate:
//   • Detach — instant, no confirm. The deliberate "drop my view now, the
//     agent keeps running" action; the human reached for exactly this.
//   • × Close — asks first (confirmAndClose, which then detachAndClose-s). A
//     plain close is also where an accidental click lands, so it confirms
//     before tearing the view down.
// A backdrop click is the easiest accident of all, so it routes through the
// same confirm as ×.
//
// Escape is NOT a close key here: it's a control character the terminal
// itself needs (vim, less, and the Claude Code TUI all lean on it), so it
// must pass straight through to xterm. (The confirm's own confirmModal still
// handles Escape = cancel while it's up.)
//
// Detach binds straight to detachAndClose with no termConfirmOpen guard: it
// doesn't need one because a confirm, when open, covers it. #confirm-modal is
// a full-viewport overlay at z-index 1000, above this modal's z-index 100, so
// the Detach button isn't clickable while any confirm (×, backdrop, or the
// disconnect prompt) is up. That layering is the guard — keep #confirm-modal
// above #term-session-modal if either z-index ever changes.
export function bindTermModal() {
  const overlay = $('#term-session-modal');
  $('#term-session-detach').addEventListener('click', detachAndClose);
  $('#term-session-close').addEventListener('click', confirmAndClose);
  // "⧉ tab" — hand this same WebSocket terminal to the dashboard's Terminals
  // tab (which holds several at once), then tear down this modal view. The
  // underlying tmux/PTY session outlives the modal, so the new pane reattaches
  // to the very same shell; hideConv rides along so closing the new pane later
  // still runs the reliable server-side detach.
  //
  // We AWAIT detachAndClose (which /api/hide-s an open-window attach) BEFORE
  // opening the new pane: /api/hide detaches EVERY client on the session, and
  // the in-SPA pane reattaches almost immediately (a microtask), so opening it
  // first would race the hide and get the fresh client detached out from under
  // it. Detaching first, then attaching, is race-free. Lets the gesture-less
  // fallbacks that land in this modal (spawn auto-focus, native-window-
  // unavailable) escape the single blocking overlay into the multi-terminal
  // view.
  $('#term-session-pop').addEventListener('click', async () => {
    // Capture the seed synchronously — detachAndClose / a concurrent open could
    // reset these module-level vars while we await. agent (= hideConv for a
    // live-session attach) lets the "focus" button later jump to this pane; a
    // web-term modal has no hideConv, so its popped pane isn't focus-jumpable.
    const seed = currentWsPath ? { ws: currentWsPath, label: currentLabel, hideConv, agent: hideConv } : null;
    await detachAndClose();
    if (seed) openTerminalPane(seed);
  });
  overlay.addEventListener('click', (e) => {
    if (e.target !== overlay) return;
    confirmAndClose();
  });
}
