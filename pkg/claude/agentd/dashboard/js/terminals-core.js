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
  initialRetry = false,
  initialRetryDelays = INITIAL_RETRY_DELAYS_MS,
  initialRetryStabilityMs = INITIAL_RETRY_STABILITY_MS,
  restartWatcher = sharedRestartWatcher,
  // Off for a surface that answers its own disconnect. The modal terminal
  // raises a blocking "reconnect or close?" dialog, and a reattach landing
  // silently behind it would leave the operator answering a stale question —
  // "Close terminal" would then kill a session that had already come back.
  autoReattach = true,
  setTimeoutImpl = globalThis.setTimeout,
  clearTimeoutImpl = globalThis.clearTimeout,
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
  // The daemon this socket is attached to, read when it opened, and the
  // outstanding "tell me if that changes" registration.
  let instanceBaseline = null;
  let cancelRestartWatch = null;
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

  function cancelRetry() {
    if (retryTimer === null) return;
    clearTimeoutImpl(retryTimer);
    retryTimer = null;
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
    abortAuth();
    closeSocket();
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
    socket.binaryType = 'arraybuffer';
    ws = socket;
    let openedAt = null;
    setStatus('connecting…');
    socket.onopen = () => {
      if (disposed || mine !== generation || ws !== socket) return;
      openedAt = now();
      captureInstanceBaseline(mine);
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
      cancelRetry();
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
