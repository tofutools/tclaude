// terminals-core.js — plain terminal domain helpers and the opaque
// xterm-over-WebSocket widget adapter shared by every Preact terminal shell.
//
// Shell state, title/status/actions, and pane chrome deliberately live outside
// this module. Each adapter instance streams one xterm over the existing PTY
// WebSocket endpoints and owns only the descendants of its stable host.
//
// Terminal / FitAddon / WebLinksAddon are globals from the vendored classic
// xterm scripts. The standalone page loads them before its module graph; the
// dashboard loads the xterm core on the first terminal request. This module
// imports nothing from the dashboard SPA, so the standalone page stays free of
// dashboard.css / helpers.js.

import { attachTerminalInteractions } from './terminal-interactions.js';
import { restartWatcher as sharedRestartWatcher } from './instance-watch.js';
import { terminalThemeFor } from './terminal-theme.js';

// A newly-created browser terminal can briefly lose an attach race with the
// client it is replacing (pop-out / reattach), or arrive while a freshly
// spawned tmux session is still becoming available. Keep this deliberately
// small and bounded: after the initial stability window, disconnects return to
// the explicit Reconnect control instead of creating a permanent retry loop.
export const INITIAL_RETRY_DELAYS_MS = Object.freeze([200, 500, 1000]);
export const INITIAL_RETRY_STABILITY_MS = 1000;
// The opening resize is also the PTY startup handshake, so even the next-frame
// refit can reach tmux before the attached harness has installed its resize
// handling. Codex is particularly visible when that happens: tmux's status bar
// moves to the right place, but the TUI keeps its old layout until the browser
// changes size again. After the first terminal output proves the attach is
// producing a screen, briefly nudge the PTY by one row, then put it back. A
// same-size resend is insufficient here: it signals the tmux client, but tmux
// does not relay unchanged geometry to the process inside its pane.
export const POST_ATTACH_RESIZE_DELAY_MS = 250;
export const POST_ATTACH_RESIZE_NUDGE_MS = 100;

// Losing agentd is the one disconnect a terminal may repair on its own, because
// it is the one it can PROVE. Every other reason a socket closes — the session
// ended, the operator reopened this terminal somewhere else — leaves a daemon
// that is alive and unchanged, and reattaching there drags back a terminal
// somebody deliberately moved. So a settled disconnect asks /api/instance what
// the daemon was doing when we lost it, and only a daemon that had already gone
// away (or already come back as a different process) earns one reattach. See
// instance-watch.js and watchForRestart below; everything else keeps nothing
// but the explicit Reconnect control.

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
// A remote terminal is a peer's agent pane, bridged by this node's daemon over
// the federation terminal stream (the path `tclaude federation attach` uses).
// It keeps the local framing (binary output and input) and adds JSON text
// control frames: hello (mode and the target's pinned size), size, and closed
// (a reason, sent just before the socket closes). The viewer renders at the
// pinned size and never resizes the target; watch mode sends no input. Each
// binary output frame is acknowledged with {type:'credit', bytes} once xterm
// has written it, which is what refills the stream's flow-control window.
export const REMOTE_TERMINAL_PATH = '/api/federation/terminal';
// A peer's pinned size beyond these is refused rather than rendered: a hostile
// peer must not be able to make this tab allocate a huge grid.
const MAX_REMOTE_COLS = 500;
// A remote pane keeps the peer's size, so its font shrinks (to a readable
// floor) until the whole grid fits; past the floor the host scrolls.
const REMOTE_FONT_MAX = 13;
const REMOTE_FONT_MIN = 9;
const MAX_REMOTE_ROWS = 200;

export function remoteTerminalPath({ peer, agent, mode = 'watch' }) {
  return `${REMOTE_TERMINAL_PATH}?${new URLSearchParams({ peer, agent, mode: mode === 'interactive' ? 'interactive' : 'watch' })}`;
}

export function isRemoteTerminalPath(path) {
  return typeof path === 'string' && path.startsWith(`${REMOTE_TERMINAL_PATH}?`);
}

// REMOTE_CLOSE says why a remote terminal closed; final reasons offer no
// reconnect, since reopening would be refused the same way.
export const REMOTE_CLOSE = Object.freeze({
  exit: { text: 'the agent exited', final: false },
  reincarnated: { text: 'the session restarted on the peer; reopen to follow it', final: false },
  revoked: { text: 'the peer revoked your access to this terminal', final: true },
  untrusted: { text: 'the peer is no longer trusted', final: true },
  kicked: { text: "the peer's operator disconnected this view", final: false },
  denied: { text: "the peer does not share this terminal with you", final: true },
  limit: { text: 'the peer has too many terminal views open; try again shortly', final: false },
  offline: { text: 'the peer is unreachable', final: false },
  error: { text: 'the connection failed', final: false },
});

// remoteCloseText words a closure from its reason; the peer's own message is
// shown only as a short, control-free detail next to a known reason.
export function remoteCloseText(closed) {
  const known = REMOTE_CLOSE[closed?.reason];
  if (!known) return 'the remote terminal closed';
  const detail = String(closed?.message || '').replace(/[\u0000-\u001f\u007f-\u009f]/g, ' ').trim().slice(0, 160);
  return detail ? `${known.text} (${detail})` : known.text;
}

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
  onComposeMessage = null,
  applicationClipboardShortcuts = false,
  onDisconnect = () => {},
  // Remote terminals report {mode, closed} as the control frames arrive.
  onRemoteChange = () => {},
  initialRetry = false,
  initialRetryDelays = INITIAL_RETRY_DELAYS_MS,
  initialRetryStabilityMs = INITIAL_RETRY_STABILITY_MS,
  attachResizeMode = 'repair',
  initialResizeDelayMs = 0,
  postAttachResizeDelayMs = POST_ATTACH_RESIZE_DELAY_MS,
  preAttachDelayMs = POST_ATTACH_RESIZE_DELAY_MS,
  postAttachResizeNudgeMs = POST_ATTACH_RESIZE_NUDGE_MS,
  restartWatcher = sharedRestartWatcher,
  // Off for a surface that answers its own disconnect. The modal terminal
  // raises a blocking "reconnect or close?" dialog, and a reattach landing
  // silently behind it would leave the operator answering a stale question —
  // "Close terminal" would then kill a session that had already come back.
  autoReattach = true,
  setTimeoutImpl = globalThis.setTimeout,
  clearTimeoutImpl = globalThis.clearTimeout,
  requestAnimationFrameImpl = globalThis.requestAnimationFrame,
  cancelAnimationFrameImpl = globalThis.cancelAnimationFrame,
  now = () => Date.now(),
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
  let retryIndex = 0;
  let retryTimer = null;
  let initialResizeTimer = null;
  let initialResizeReady = false;
  let firstOutputSeen = false;
  let postOpenFitFrame = null;
  let postAttachResizeTimer = null;
  let postAttachResizeScheduled = false;
  // The daemon this socket is attached to, read when it opened, and the
  // outstanding "tell me if that changes" registration.
  let instanceBaseline = null;
  let cancelRestartWatch = null;
  const remote = isRemoteTerminalPath(wsPath);
  let remoteState = { mode: '', closed: null, files: false, viewer: '' };
  function setRemote(next) {
    if (disposed) return;
    remoteState = Object.freeze({ ...remoteState, ...next });
    onRemoteChange(remoteState);
  }
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
  if (remote) host.classList?.add('term-remote');

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
    if (remote) { fitRemote(); return; }
    try { fitAddon.fit(); } catch (_) { /* host may not be laid out yet */ }
  }

  // fitRemote scales the font so the peer's grid fits the host, never
  // resizing the grid itself.
  function fitRemote() {
    const screen = host.querySelector?.('.xterm-screen');
    const width = host.clientWidth; const height = host.clientHeight;
    if (!screen || !width || !height) return;
    const box = () => screen.getBoundingClientRect();
    let r = box();
    if (!r.width || !r.height) return;
    const font = term.options.fontSize;
    const next = Math.max(REMOTE_FONT_MIN, Math.min(REMOTE_FONT_MAX, Math.floor(font * Math.min(width / r.width, height / r.height) * 2) / 2));
    if (next !== font) term.options.fontSize = next;
    // Cell sizes round to device pixels; step down until it really fits.
    for (let i = 0; i < 4 && term.options.fontSize > REMOTE_FONT_MIN; i++) {
      r = box();
      if (r.width <= width && r.height <= height) break;
      term.options.fontSize = Math.max(REMOTE_FONT_MIN, term.options.fontSize - 0.5);
    }
  }

  function focus() {
    if (!disposed) term.focus();
  }

  function sendDimensions(cols, rows, requireLayout = true, extra = null) {
    if (disposed || !initialResizeReady || !ws || ws.readyState !== WebSocketCtor.OPEN) return;
    // A host with no layout — a display:none mount before the Terminals tab
    // reveals, a pane mid-teardown — makes FitAddon propose garbage (a tiny
    // grid measured from an unrendered box), and the PTY bridge adopts the
    // FIRST reported size as the size the command is born at. Better to say
    // nothing: the server falls back to 80x24 on its own, and every reveal
    // path (setActive, the ResizeObserver, the post-open refit) re-fits and
    // resends once the host has real geometry. Strictly-equal zero so a test
    // DOM without layout (offsetWidth undefined) keeps its sends.
    if (requireLayout && (host.offsetWidth === 0 || host.offsetHeight === 0)) return false;
    ws.send(JSON.stringify({ type: 'resize', cols, rows, ...(extra || {}) }));
    return true;
  }

  function sendResize() {
    return sendDimensions(term.cols, term.rows);
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

  function cancelRetry() {
    if (retryTimer === null) return;
    clearTimeoutImpl(retryTimer);
    retryTimer = null;
  }

  function cancelInitialResize() {
    if (initialResizeTimer === null) return;
    clearTimeoutImpl(initialResizeTimer);
    initialResizeTimer = null;
  }

  function cancelPostOpenFit() {
    if (postOpenFitFrame === null) return;
    if (typeof cancelAnimationFrameImpl === 'function') {
      cancelAnimationFrameImpl(postOpenFitFrame);
    }
    postOpenFitFrame = null;
  }

  function cancelPostAttachResize() {
    if (postAttachResizeTimer === null) return;
    clearTimeoutImpl(postAttachResizeTimer);
    postAttachResizeTimer = null;
  }

  // xterm may accept a very fast WebSocket before its first render has
  // established cell dimensions. In that window FitAddon cannot calculate a
  // grid, so the immediate resize below sends xterm's constructor default and
  // no later ResizeObserver callback is guaranteed: the host itself may
  // already be at its final size. Refit once on the next animation frame, when
  // both layout and xterm's renderer have settled, and explicitly resend even
  // if xterm decides the grid did not change.
  function schedulePostOpenFit(mine, socket) {
    cancelPostOpenFit();
    if (typeof requestAnimationFrameImpl !== 'function') return;
    postOpenFitFrame = requestAnimationFrameImpl(() => {
      postOpenFitFrame = null;
      if (disposed || mine !== generation || ws !== socket || !isActive) return;
      fit();
      sendResize();
    });
  }

  // One animation frame is enough for xterm's cell metrics, but not
  // necessarily for the tmux client and harness behind the PTY to finish
  // attaching. This later nudge deliberately fits first: if surrounding
  // dashboard chrome also settled during that wait, the re-sync starts from
  // the newest grid. The original size is restored after a short interval so
  // tmux observes a real geometry transition and relays SIGWINCH into its pane.
  // It is one-shot and connection-scoped, never a resize loop.
  function schedulePostAttachResize(mine, socket) {
    cancelPostAttachResize();
    if (attachResizeMode !== 'repair') return;
    const delay = Number(postAttachResizeDelayMs);
    if (!Number.isFinite(delay) || delay < 0) return;
    postAttachResizeTimer = setTimeoutImpl(() => {
      postAttachResizeTimer = null;
      if (disposed || mine !== generation || ws !== socket || !isActive) return;
      fit();
      const cols = term.cols;
      const rows = term.rows;
      const nudgedRows = rows > 1 ? rows - 1 : rows + 1;
      if (!sendDimensions(cols, nudgedRows)) return;

      const nudgeDelay = Math.max(0, Number(postAttachResizeNudgeMs) || 0);
      postAttachResizeTimer = setTimeoutImpl(() => {
        postAttachResizeTimer = null;
        if (disposed || mine !== generation || ws !== socket) return;
        // Fit again in case a genuine browser resize landed during the nudge.
        // Restore even if the pane just became inactive: leaving the backing
        // PTY one row short is worse than sending the last xterm grid.
        if (isActive) fit();
        sendDimensions(term.cols, term.rows, false);
      }, nudgeDelay);
    }, delay);
  }

  function scheduleRepairAfterOutput(mine, socket) {
    if (!firstOutputSeen || !initialResizeReady || postAttachResizeScheduled) return;
    postAttachResizeScheduled = true;
    schedulePostAttachResize(mine, socket);
  }

  // Delay the opening handshake when requested, then send the fitted grid. In
  // pre_attach mode this first resize also tells the PTY bridge how long to
  // hold the already-sized PTY before starting the tmux attachment command.
  function scheduleInitialResize(mine, socket) {
    cancelInitialResize();
    initialResizeReady = false;
    const delay = Math.max(0, Number(initialResizeDelayMs) || 0);
    const run = () => {
      initialResizeTimer = null;
      if (disposed || mine !== generation || ws !== socket) return;
      initialResizeReady = true;
      if (isActive) fit();
      const extra = attachResizeMode === 'pre_attach'
        ? { attach_delay_ms: Math.max(0, Number(preAttachDelayMs) || 0) }
        : null;
      sendDimensions(term.cols, term.rows, true, extra);
      schedulePostOpenFit(mine, socket);
      scheduleRepairAfterOutput(mine, socket);
    };
    if (delay === 0) run();
    else {
      // Tell the bridge this intentional silence is not a dead/pre-protocol
      // client. It extends only this connection's bounded opening-size wait;
      // without it a configured delay above the ordinary one-second fallback
      // would start the PTY at 80x24 before the real grid arrived.
      if (socket.readyState === WebSocketCtor.OPEN) {
        socket.send(JSON.stringify({ type: 'resize_wait', delay_ms: delay }));
      }
      initialResizeTimer = setTimeoutImpl(run, delay);
    }
  }

  function cancelRestartWatchIfAny() {
    if (!cancelRestartWatch) return;
    const cancel = cancelRestartWatch;
    cancelRestartWatch = null;
    cancel();
  }

  // captureInstanceBaseline records which agentd this socket is attached to.
  // Read per connection rather than once per page: a widget opened long after
  // the last one must not inherit a stale id, or its very first probe would
  // read as a restart. Failure is fine and simply leaves this terminal without
  // self-repair for the current connection.
  function captureInstanceBaseline(mine) {
    instanceBaseline = null;
    if (!restartWatcher) return;
    Promise.resolve()
      .then(() => restartWatcher.currentID())
      .then((id) => {
        if (disposed || mine !== generation) return;
        instanceBaseline = typeof id === 'string' && id ? id : null;
        // The socket can die while this read is still in flight — a reattach
        // dropped by a daemon that is up but not yet ready does exactly that.
        // Such a disconnect settled with no baseline to reason about, so give
        // it the chance now rather than losing its repair to a race.
        if (instanceBaseline && !cancelRestartWatch && status === 'disconnected') {
          watchForRestart();
        }
      }, () => { /* no baseline; this connection simply cannot self-repair */ });
  }

  function armRestartWatch(baseline, mine) {
    cancelRestartWatch = restartWatcher.watchForRestart(baseline, (id) => {
      cancelRestartWatch = null;
      if (disposed || mine !== generation) return;
      // Adopt the daemon we were just told about BEFORE dialing it. A reattach
      // that dies before its socket ever opens would otherwise still be holding
      // the dead process's id, and the very next probe would read as another
      // restart — turning "one reattach per restart" into a dial per probe for
      // the rest of the round.
      if (typeof id === 'string' && id) instanceBaseline = id;
      void connect();
    });
  }

  // watchForRestart decides whether this disconnect is one a reattach may
  // repair, and arms at most one automatic reattach if it is. It runs only from
  // a SETTLED disconnect, after the bounded initial-retry window.
  //
  // A later restart is NOT on its own a reason to reattach: it says the daemon
  // changed, not that this socket died because of it. A terminal the operator
  // moved elsewhere — a second dashboard tab, another device, a native window —
  // is dead here while its session runs happily under the new client, and
  // reattaching would drag it back (tmux attaches with -d, so the reattach
  // detaches whoever holds it). So the disconnect has to qualify first: probe
  // once, immediately, and ask what the daemon was doing when we lost it.
  //
  //   answers, same id  → it was alive and unchanged, so it did not kill this
  //                       socket. Something else did. Never reattach.
  //   answers, new id   → it already restarted. Reattach now.
  //   no answer         → it is gone, which is exactly the case this exists
  //                       for. Wait for it to come back as a different process.
  function watchForRestart() {
    cancelRestartWatchIfAny();
    if (disposed || !autoReattach || !restartWatcher || !instanceBaseline) return;
    const baseline = instanceBaseline;
    const mine = generation;
    Promise.resolve()
      .then(() => restartWatcher.currentID())
      .then((id) => {
        if (disposed || mine !== generation || cancelRestartWatch) return;
        if (id === baseline) return;
        if (typeof id === 'string' && id) {
          instanceBaseline = id;
          void connect();
          return;
        }
        armRestartWatch(baseline, mine);
      }, () => { /* nothing learned; this disconnect keeps its manual control */ });
  }

  function scheduleInitialRetry() {
    if (!initialRetry || retryIndex >= initialRetryDelays.length) return false;
    const delay = Math.max(0, Number(initialRetryDelays[retryIndex]) || 0);
    retryIndex += 1;
    setStatus('retrying…');
    setReconnectAvailable(false);
    retryTimer = setTimeoutImpl(() => {
      retryTimer = null;
      void dial();
    }, delay);
    return true;
  }

  async function dial() {
    if (disposed) return false;
    generation += 1;
    const mine = generation;
    postAttachResizeScheduled = false;
    firstOutputSeen = false;
    initialResizeReady = false;
    abortAuth();
    closeSocket();
    cancelPostOpenFit();
    cancelInitialResize();
    cancelPostAttachResize();
    // Dialing now, so any pending "tell me when the daemon is new" is moot.
    cancelRestartWatchIfAny();
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
        if (scheduleInitialRetry()) return false;
        setStatus('disconnected');
        setReconnectAvailable(true);
        watchForRestart();
        return false;
      } finally {
        if (authController === controller) authController = null;
      }
    }

    if (disposed || mine !== generation) return false;
    const proto = locationRef.protocol === 'https:' ? 'wss:' : 'ws:';
    const socket = new WebSocketCtor(proto + '//' + locationRef.host + wsPath);
    if (remote) setRemote({ mode: '', closed: null, files: false, viewer: '' });
    socket.binaryType = 'arraybuffer';
    ws = socket;
    let openedAt = null;
    setStatus('connecting…');
    socket.onopen = () => {
      if (disposed || mine !== generation || ws !== socket) return;
      openedAt = now();
      captureInstanceBaseline(mine);
      setReconnectAvailable(false);
      if (remote) { setStatus('connecting to the peer…'); return; }
      setStatus('connected');
      scheduleInitialResize(mine, socket);
    };
    socket.onmessage = (event) => {
      if (disposed || mine !== generation || ws !== socket) return;
      if (remote && typeof event.data === 'string') { remoteControl(event.data); return; }
      if (remote) {
        // Credit flows back only once xterm has consumed the bytes, so the
        // federation stream's window stays bounded through the renderer.
        const bytes = event.data instanceof ArrayBuffer ? new Uint8Array(event.data) : new TextEncoder().encode(String(event.data));
        term.write(bytes, () => {
          if (disposed || mine !== generation || ws !== socket || socket.readyState !== WebSocketCtor.OPEN || !bytes.byteLength) return;
          socket.send(JSON.stringify({ type: 'credit', bytes: bytes.byteLength }));
        });
        return;
      }
      term.write(event.data instanceof ArrayBuffer ? new Uint8Array(event.data) : event.data);
      // WebSocket open precedes PTY creation on the server. The first output is
      // the browser's earliest proof that the command/tmux attach is actually
      // producing a screen, so anchor the delayed harness reflow here instead
      // of to a fixed interval from socket open. Only the first frame arms it.
      firstOutputSeen = true;
      scheduleRepairAfterOutput(mine, socket);
    };
    socket.onclose = () => {
      if (disposed || mine !== generation || ws !== socket) return;
      cancelPostAttachResize();
      cancelInitialResize();
      if (remote) {
        // The pinned viewer is gone with the socket: no more file links.
        setRemote({ files: false, viewer: '' });
        const closed = remoteState.closed;
        setStatus(closed ? `closed: ${remoteCloseText(closed)}` : 'disconnected');
        setReconnectAvailable(!(closed && REMOTE_CLOSE[closed.reason]?.final));
        onDisconnect();
        return;
      }
      const unstable = openedAt === null || now() - openedAt < initialRetryStabilityMs;
      if (unstable && scheduleInitialRetry()) return;
      setStatus('disconnected');
      setReconnectAvailable(true);
      watchForRestart();
      onDisconnect();
    };
    socket.onerror = () => {
      if (disposed || mine !== generation || ws !== socket) return;
      try { socket.close(); } catch (_) { /* onclose handles it */ }
    };
    return true;
  }

  // remoteControl applies a remote terminal's JSON control frame. Unparseable
  // text is dropped, never written to the terminal.
  function remoteControl(text) {
    let msg = null;
    try { msg = JSON.parse(text); } catch (_) { return; }
    const size = () => {
      const cols = Number(msg.cols); const rows = Number(msg.rows);
      if (Number.isInteger(cols) && Number.isInteger(rows) && cols > 0 && rows > 0 && cols <= MAX_REMOTE_COLS && rows <= MAX_REMOTE_ROWS && (cols !== term.cols || rows !== term.rows)) {
        term.resize(cols, rows);
        if (typeof requestAnimationFrameImpl === 'function') requestAnimationFrameImpl(fit);
      }
    };
    if (msg?.type === 'hello') {
      const mode = msg.mode === 'interactive' ? 'interactive' : 'watch';
      term.options.disableStdin = mode !== 'interactive';
      size();
      // files says the peer shares this terminal's files with us
      // (sessions.files.read); viewer_id pins downloads to this very view.
      const viewer = typeof msg.viewer_id === 'string' && /^[A-Za-z0-9_.:-]{1,128}$/.test(msg.viewer_id) ? msg.viewer_id : '';
      setRemote({ mode, files: msg.files === true && !!viewer, viewer });
      setStatus(mode === 'interactive' ? 'connected · interactive' : 'connected · watch-only');
    } else if (msg?.type === 'size') {
      size();
    } else if (msg?.type === 'closed') {
      setRemote({ closed: { reason: String(msg.reason || ''), message: String(msg.message || '') } });
    }
  }

  function connect() {
    cancelRetry();
    retryIndex = 0;
    return dial();
  }

  const interactions = interactionsFactory({
    term,
    host,
    terminalPath: wsPath,
    copyButton: null,
    setStatus,
    baseStatus: () => ws && ws.readyState === WebSocketCtor.OPEN ? 'connected' : 'disconnected',
    onComposeMessage,
    applicationClipboardShortcuts,
    onSelectionChange: (selected) => { if (!disposed) onSelectionChange(selected); },
    canInput: () => !remote || remoteState.mode === 'interactive',
    fileDownloads: !remote,
    // A remote terminal's visible paths download from the peer only while
    // the live view holds sessions.files.read.
    remoteFileViewer: remote ? () => (remoteState.files ? remoteState.viewer : '') : null,
    oscClipboard: !remote,
  });

  disposables.push(term.onData((data) => {
    if (remote && remoteState.mode !== 'interactive') return;
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
    remoteState: () => (remote ? remoteState : null),
    reconnectAvailable: () => reconnectAvailable,
    isDisposed: () => disposed,
    dispose() {
      if (disposed) return;
      disposed = true;
      generation += 1;
      cancelRetry();
      cancelPostOpenFit();
      cancelInitialResize();
      cancelPostAttachResize();
      cancelRestartWatchIfAny();
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
