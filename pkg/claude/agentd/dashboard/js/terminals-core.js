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
import { drawingBoardPaneKey, isDrawingBoardId } from './drawing-board-model.js';
import { isProcessTemplateId, processPaneKey } from './process-pane-model.js';
import { isPageId, pagePaneKey } from './page-pane-model.js';

// A newly-created browser terminal can briefly lose an attach race with the
// client it is replacing (pop-out / reattach), or arrive while a freshly
// spawned tmux session is still becoming available. Keep this deliberately
// small and bounded: after the initial stability window, disconnects return to
// the explicit Reconnect control instead of creating a permanent retry loop.
export const INITIAL_RETRY_DELAYS_MS = Object.freeze([200, 500, 1000]);
export const INITIAL_RETRY_STABILITY_MS = 1000;
// An agent whose launch is still in progress (agentd closes with
// TERMINAL_STARTING, a peer with the 'starting' reason) is waited for, not
// failed: the terminal says so and redials with this backoff, the last delay
// repeating, for up to START_WAIT_MS from the first such answer. agentd only
// answers 'starting' inside its own window after the launch began, so a launch
// that never produces a pane still ends in the ordinary Disconnected state.
export const TERMINAL_STARTING = 4005;
export const START_RETRY_DELAYS_MS = Object.freeze([250, 500, 1000, 2000]);
export const START_WAIT_MS = 30000;
// A live terminal that drops without a settled answer (a socket hiccup, agentd
// restarting) gets these few quiet redials before the manual Reconnect.
export const TRANSIENT_RETRY_DELAYS_MS = Object.freeze([500, 1500, 3000]);
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

// A settled answer from a live daemon — the session ended, or the operator
// reopened this terminal somewhere else, which detaches this one — carries a
// close code (TERMINAL_EXIT), and reattaching there would drag back a terminal
// somebody deliberately moved, so it never redials. A close without one (a
// socket hiccup, agentd going away) first gets a few quiet redials
// (TRANSIENT_RETRY_DELAYS_MS). After those, losing agentd is the one disconnect
// a terminal may still repair on its own, because it is the one it can PROVE:
// a settled disconnect asks /api/instance what the daemon was doing when we
// lost it, and only a daemon that had already gone away (or already come back
// as a different process) earns one reattach. See instance-watch.js and
// watchForRestart below; everything else keeps the explicit Reconnect control.

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
// control frames: hello (mode, the target's size, shared, others, take_over),
// size, others, and closed (a reason, sent just before the socket closes).
// A shared view (a current peer) fits its panel and reports that size; the
// peer's window takes, per axis, the smallest size across every attached
// client, and the view renders the grid it is sent — never stretched — and
// says when another client limits it. With the peer's sessions.takeover grant
// it can take the session over, disconnecting the others. An older peer pins
// the view at its own size instead. Watch mode sends no input. Each
// binary output frame is acknowledged with {type:'credit', bytes} once xterm
// has written it, which is what refills the stream's flow-control window.
export const REMOTE_TERMINAL_PATH = '/api/federation/terminal';
// A peer's pinned size beyond these is refused rather than rendered: a hostile
// peer must not be able to make this tab allocate a huge grid.
const MAX_REMOTE_COLS = 500;
// A pinned remote pane keeps the peer's size, so its font shrinks (to a readable
// floor) until the whole grid fits; past the floor the host scrolls.
const REMOTE_FONT_MAX = 13;
const REMOTE_FONT_MIN = 9;
const MAX_REMOTE_ROWS = 200;
// The size a terminal opens at, and how far zoom takes it. A step is a control's
// click; the wheel goes by half steps, so a pinch moves in small amounts.
export const TERMINAL_FONT_DEFAULT = 13;
export const TERMINAL_FONT_MIN = 8;
export const TERMINAL_FONT_MAX = 32;
export const TERMINAL_FONT_STEP = 1;
const ZOOM_WHEEL_PX_PER_HALF_STEP = 15;
const ZOOM_WHEEL_MAX_PX = 30; // a mouse notch is one step, however large its delta
// A zoom shows at once as a scale on the rendered screen, which the compositor
// can do in a frame. The real font and the refit wait for the gesture to settle,
// because a font change repaints every row, and a refit resizes the pty and
// makes the harness redraw. A click only waits for the scale to paint; a wheel
// waits long enough to span a gesture's events.
const ZOOM_CLICK_SETTLE_MS = 32;
const ZOOM_WHEEL_SETTLE_MS = 110;
// A touch pinch settles once the fingers have rested this long, so a pinch
// reflows the grid once, not on every move.
const ZOOM_PINCH_SETTLE_MS = 220;

// A one-finger swipe scrolls as a mouse wheel does (touchScroll): one wheel
// notch per SWIPE_ROWS_PER_NOTCH rows the finger travels (tmux scrolls
// copy-mode five lines a notch, so the text keeps up with the finger). Only
// a session that takes the mouse (tmux mouse on) scrolls: without it a
// wheel notch is an arrow key, which recalls prompt history or moves a menu,
// and a stray swipe on a phone must not. A swipe starts once the finger has
// moved SWIPE_START_PX, mostly up or down. Let go while moving and it coasts,
// slowing by SWIPE_DECAY each frame until slower than SWIPE_STOP px/ms.
const SWIPE_ROWS_PER_NOTCH = 5;
const SWIPE_START_PX = 10;
const SWIPE_FLING = 0.3;
// A finger that rested this long before lifting does not fling.
const SWIPE_REST_MS = 100;
const SWIPE_STOP = 0.05;
const SWIPE_DECAY = 0.95;
const SWIPE_MAX_NOTCHES_PER_FRAME = 4;
// A mouse report is how xterm hands the session a wheel notch; it is not
// typing, and must not bring a scrolled-back view back to live.
const MOUSE_REPORT = /^\x1b\[(<|M)/;

// The keys a touch surface sends by name (key()): what a keyboard's key
// would send. The arrows follow the application's cursor-key mode.
const NAMED_KEYS = Object.freeze({
  enter: '\r', esc: '\x1b', tab: '\t', 'ctrl-c': '\x03', backspace: '\x7f',
});
const CURSOR_KEYS = Object.freeze({ up: 'A', down: 'B', right: 'C', left: 'D' });
// How long a shared view waits for the peer to answer its own size report
// before saying another viewer limits the size.
const REMOTE_LIMITED_GRACE_MS = 800;

export function remoteTerminalPath({ peer, agent, mode = 'watch' }) {
  return `${REMOTE_TERMINAL_PATH}?${new URLSearchParams({ peer, agent, mode: mode === 'interactive' ? 'interactive' : 'watch' })}`;
}

// remoteShellPath is the bridge to one of a peer's tclaude shells: its
// group's shell (shell 'group') or a member agent's directory shell (shell
// 'start' | 'current' | 'worktree', with agent). Always interactive: the
// peer's sessions.shell grant is to type into it.
export function remoteShellPath({ peer, group, shell, agent = '' }) {
  return `${REMOTE_TERMINAL_PATH}?${new URLSearchParams({ peer, group, shell, ...(agent ? { agent } : {}), mode: 'interactive' })}`;
}

export function isRemoteTerminalPath(path) {
  return typeof path === 'string' && path.startsWith(`${REMOTE_TERMINAL_PATH}?`);
}

// REMOTE_CLOSE says why a remote terminal closed; final reasons offer no
// reconnect, since reopening would be refused the same way.
export const REMOTE_CLOSE = Object.freeze({
  exit: { text: 'the agent exited', final: false },
  reincarnated: { text: 'the session restarted on the peer', final: false },
  revoked: { text: 'the peer revoked your access to this terminal', final: true },
  untrusted: { text: 'the peer is no longer trusted', final: true },
  kicked: { text: "the peer's operator disconnected this view", final: false },
  taken_over: { text: 'another viewer took over this terminal', final: false },
  denied: { text: 'the peer does not share this terminal with you (needs sessions.watch to look, sessions.attach to type)', final: true },
  starting: { text: 'the agent did not start on the peer in time', final: false },
  not_running: { text: 'the agent is not running on the peer', final: false },
  limit: { text: 'the peer has too many terminal views open; try again shortly', final: false },
  offline: { text: 'the peer is unreachable', final: false },
  error: { text: 'the connection failed', final: false },
});

// REMOTE_FOLLOW are the remote closures a terminal waits out with the start
// wait, as a local terminal waits out TERMINAL_STARTING: the peer is launching
// the agent, or a relaunch (restart, resume, reincarnation) replaced the pane
// this view was on. Each redial names the agent, so it reaches the new launch.
const REMOTE_FOLLOW = new Set(['starting', 'reincarnated']);

// REMOTE_TRANSIENT are the remote closures a terminal redials quietly before
// it settles: they say the path to the peer failed, not that the peer refused.
const REMOTE_TRANSIENT = new Set(['offline', 'error']);

// remoteCloseText words a closure from its reason; the peer's own message is
// shown only as a short, control-free detail next to a known reason.
export function remoteCloseText(closed) {
  const known = REMOTE_CLOSE[closed?.reason];
  if (!known) return 'the remote terminal closed';
  const detail = String(closed?.message || '').replace(/[\u0000-\u001f\u007f-\u009f]/g, ' ').trim().slice(0, 160);
  return detail ? `${known.text} (${detail})` : known.text;
}

// TERMINAL_EXIT maps the close codes agentd sends when a terminal's own command
// exited (dashboard_term.go terminalExitClose) to the pane status and whether
// dashboard.close_terminal_tabs wants the tab closed. Either way it is a
// settled answer from a live daemon, so no retry or restart watch follows.
export const TERMINAL_EXIT = Object.freeze({
  4001: { text: 'session ended', closeTab: false },
  4002: { text: 'session ended', closeTab: true },
  4003: { text: 'detached', closeTab: false },
  4004: { text: 'detached', closeTab: true },
  // The agent has no live pane and is not starting (agentd's attach refusal).
  4006: { text: 'the agent is not running', closeTab: false },
});

// A drawing board seed (seed.drawing board) has no socket: it must name a valid drawing board id, and
// whatever else a crafted hash put in it cannot give it one, or a key that
// collides with another pane.
export function normalizeSeed(seed) {
  if (seed?.drawingBoard !== undefined) {
    if (!isDrawingBoardId(seed.drawingBoard) || seed.process !== undefined || seed.page !== undefined) return null;
    const { ws: _ws, ...rest } = seed;
    return { ...rest, key: drawingBoardPaneKey(seed.drawingBoard) };
  }
  // A process seed (seed.process) is the same: a template id and nothing to dial.
  if (seed?.process !== undefined) {
    if (!isProcessTemplateId(seed.process) || seed.page !== undefined) return null;
    const { ws: _ws, ...rest } = seed;
    return { ...rest, key: processPaneKey(seed.process) };
  }
  // A live page's seed (seed.page) names the page and nothing else: its URL
  // comes from the snapshot, never from a seed.
  if (seed?.page !== undefined) {
    // Only these fields: a seed can come from a crafted /terminals?solo=1 link.
    if (!isPageId(seed.page)) return null;
    const { label, floats, overlay, initialRetry, wizard, terminalAttach } = seed;
    return {
      page: seed.page, key: pagePaneKey(seed.page), label: typeof label === 'string' ? label : '',
      ...(floats && { floats: true }), ...(overlay && { overlay: true }), ...(initialRetry !== undefined && { initialRetry }),
      ...(wizard !== undefined && { wizard }), ...(terminalAttach !== undefined && { terminalAttach }),
    };
  }
  return (seed && typeof seed.ws === 'string' && seed.ws.startsWith('/')) ? seed : null;
}

function clampFont(size) {
  return Math.min(TERMINAL_FONT_MAX, Math.max(TERMINAL_FONT_MIN, Math.round(size * 2) / 2));
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
  // {kind: 'starting' | 'reconnecting', text} while the widget waits on its
  // own between redials, null otherwise, so a shell can show the quiet state
  // in the terminal area.
  onWaitingChange = () => {},
  // Who the terminal is for, in "Waiting for <agentLabel> to start…".
  agentLabel = '',
  onSelectionChange = () => {},
  onComposeMessage = null,
  applicationClipboardShortcuts = false,
  onDisconnect = () => {},
  // Remote terminals report {mode, closed} as the control frames arrive.
  onRemoteChange = () => {},
  // A zoom the operator chose before, and {size, zoomed} whenever the font
  // changes; zoomed is false while the size is the terminal's own.
  initialFontSize = null,
  onFontChange = () => {},
  // touchZoom: a two-finger pinch on the terminal sets its font size, and the
  // grid reflows once the pinch rests (the phone terminal). The host should
  // then be touch-action: none, so the browser leaves the page alone.
  touchZoom = false,
  // softKeyboard: false keeps a touch device's on-screen keyboard down when
  // the terminal takes focus, until setSoftKeyboard(true).
  softKeyboard = true,
  // touchScroll: a one-finger vertical swipe on the terminal scrolls as the
  // wheel does (the phone terminal), and onScrollBackChange(back) says
  // whether the view is scrolled back from live (tmux copy-mode).
  touchScroll = false,
  onScrollBackChange = () => {},
  initialRetry = false,
  initialRetryDelays = INITIAL_RETRY_DELAYS_MS,
  initialRetryStabilityMs = INITIAL_RETRY_STABILITY_MS,
  startRetryDelays = START_RETRY_DELAYS_MS,
  startWaitMs = START_WAIT_MS,
  transientRetryDelays = TRANSIENT_RETRY_DELAYS_MS,
  attachResizeMode = 'repair',
  initialResizeDelayMs = 0,
  postAttachResizeDelayMs = POST_ATTACH_RESIZE_DELAY_MS,
  preAttachDelayMs = POST_ATTACH_RESIZE_DELAY_MS,
  postAttachResizeNudgeMs = POST_ATTACH_RESIZE_NUDGE_MS,
  restartWatcher = sharedRestartWatcher,
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
  // The start wait: when the first 'starting' answer came (null: not waiting)
  // and how far along its backoff the redials are.
  let startingSince = null;
  let startIndex = 0;
  // Quiet redials spent since the terminal last proved it was attached, and
  // whether the next dial is one. A quiet redial asks agentd to reattach only
  // if nobody else holds the session (reattach=1), so it repairs a dropped
  // socket without ever taking back a terminal the operator moved elsewhere.
  let transientIndex = 0;
  let reattachNext = false;
  let waiting = null;
  let initialResizeTimer = null;
  let initialResizeReady = false;
  let firstOutputSeen = false;
  // When xterm first / last finished parsing output from this connection
  // (0 = none yet). settled() reads them to tell "attached and redrawn" from
  // "still busy".
  let firstOutputParsedAt = 0;
  let lastOutputParsedAt = 0;
  let postOpenFitFrame = null;
  let postAttachResizeTimer = null;
  let postAttachResizeScheduled = false;
  // The daemon this socket is attached to, read when it opened, and the
  // outstanding "tell me if that changes" registration.
  let instanceBaseline = null;
  let cancelRestartWatch = null;
  const remote = isRemoteTerminalPath(wsPath);
  const REMOTE_RESET = Object.freeze({ mode: '', closed: null, files: false, viewer: '', shared: false, others: [], canTakeOver: false, limited: false });
  let remoteState = REMOTE_RESET;
  // desired: the grid a shared view's panel could show; lastDesired: the one
  // last reported to the peer.
  let desired = null;
  let lastDesired = '';
  // limitedHold runs while the peer may still be answering this view's latest
  // report: the grid then lags the panel by design, so "limited" waits.
  let limitedHold = null;
  function cancelLimitedHold() {
    if (limitedHold !== null) clearTimeoutImpl(limitedHold);
    limitedHold = null;
  }
  // takeOverNext asks the next dial to take the peer's session over.
  let takeOverNext = false;
  function setRemote(next) {
    if (disposed) return;
    remoteState = Object.freeze({ ...remoteState, ...next });
    onRemoteChange(remoteState);
  }
  const disposables = [];
  // The operator's size, or null to leave it to the terminal: 13, or what fits
  // a remote grid. While set it wins over that fit, and a grid too large for
  // the panel scrolls, because the operator asked for this size.
  let zoomFont = Number.isFinite(initialFontSize) ? clampFont(initialFontSize) : null;
  let zoomWheelRest = 0;
  let zoomFitTimer = null;
  // The size a zoom is previewing, until it settles.
  let zoomPending = null;

  const term = new TerminalCtor({
    cursorBlink: true,
    fontSize: zoomFont ?? TERMINAL_FONT_DEFAULT,
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
  reportFont();

  function reportFont() {
    if (!disposed) onFontChange({ size: zoomPending ?? term.options.fontSize, zoomed: zoomFont !== null });
  }

  function previewZoom(size) {
    const screen = host.getElementsByClassName?.('xterm-screen')[0];
    if (!screen) return;
    screen.style.transformOrigin = '0 0';
    screen.style.transform = size === null ? '' : `scale(${size / term.options.fontSize})`;
  }

  function setZoom(next, settleMs = ZOOM_CLICK_SETTLE_MS) {
    if (disposed) return;
    zoomFont = next;
    zoomPending = next ?? TERMINAL_FONT_DEFAULT;
    previewZoom(zoomPending);
    reportFont();
    if (zoomFitTimer !== null) clearTimeoutImpl(zoomFitTimer);
    zoomFitTimer = setTimeoutImpl(() => {
      zoomFitTimer = null;
      // Font and grid change in one tick, so xterm paints them once.
      const size = zoomPending;
      zoomPending = null;
      previewZoom(null);
      term.options.fontSize = size;
      fit();
      reportFont();
    }, settleMs);
  }

  function zoomBy(step, settleMs) {
    if (!disposed) setZoom(clampFont((zoomPending ?? term.options.fontSize) + step), settleMs);
  }

  // zoomWheel takes Ctrl+wheel, which is also how a browser reports a trackpad
  // pinch: both zoom this terminal, and the browser leaves the page alone.
  function zoomWheel(event) {
    if (!event.ctrlKey && !event.metaKey) return;
    event.preventDefault();
    event.stopPropagation();
    const scale = event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? 400 : 1;
    const delta = Number(event.deltaY) * scale || 0;
    zoomWheelRest += Math.max(-ZOOM_WHEEL_MAX_PX, Math.min(ZOOM_WHEEL_MAX_PX, delta));
    const halves = Math.trunc(zoomWheelRest / ZOOM_WHEEL_PX_PER_HALF_STEP);
    if (!halves) return;
    zoomWheelRest -= halves * ZOOM_WHEEL_PX_PER_HALF_STEP;
    zoomBy(-halves * TERMINAL_FONT_STEP / 2, ZOOM_WHEEL_SETTLE_MS);
  }
  // Capture: xterm would otherwise take the wheel for the application first.
  host.addEventListener?.('wheel', zoomWheel, { capture: true, passive: false });

  // A pinch scales from the size it started at by how far the fingers spread.
  let pinch = null;
  const spread = (touches) => Math.hypot(touches[0].clientX - touches[1].clientX, touches[0].clientY - touches[1].clientY);
  function pinchStart(event) {
    if (event.touches?.length !== 2) return;
    swipe = null;
    stopCoasting();
    const distance = spread(event.touches);
    if (!(distance > 0)) return;
    pinch = { distance, size: zoomPending ?? term.options.fontSize };
    event.preventDefault();
  }
  function pinchMove(event) {
    if (!pinch || event.touches?.length !== 2) return;
    event.preventDefault();
    const next = clampFont(pinch.size * spread(event.touches) / pinch.distance);
    if (next !== (zoomPending ?? term.options.fontSize)) setZoom(next, ZOOM_PINCH_SETTLE_MS);
  }
  function pinchEnd(event) {
    if ((event.touches?.length ?? 0) < 2) pinch = null;
  }
  // The swipe: wheel notches dispatched on xterm's own element, so xterm
  // encodes them exactly as a desktop wheel (a mouse report when the session
  // takes the mouse, an arrow key otherwise).
  let swipe = null;
  let coast = null;
  // Where tmux's wheel copy-mode is, in notches: scrolledBack is how many
  // notches up from the bottom (0: live). A local session's first notch only
  // enters copy-mode (tmux's WheelUpPane runs copy-mode -e); a remote pane
  // enters and scrolls. Scrolling down to the bottom leaves copy-mode, and so
  // does a notch down at the bottom.
  let scrolledBack = 0;
  let inCopyMode = false;
  const mouseTaken = () => (term.modes?.mouseTrackingMode || 'none') !== 'none';
  const canSend = () => !!ws && ws.readyState === WebSocketCtor.OPEN && (!remote || remoteState.mode === 'interactive');
  function rowHeight() {
    const screen = host.getElementsByClassName?.('xterm-screen')[0];
    const height = screen?.getBoundingClientRect?.().height;
    return height > 0 && term.rows > 0 ? height / term.rows : 16;
  }
  const notchPx = () => rowHeight() * SWIPE_ROWS_PER_NOTCH;
  function setCopyMode(on, notches) {
    const was = inCopyMode;
    inCopyMode = on;
    scrolledBack = on ? notches : 0;
    if (was !== on && !disposed) onScrollBackChange(on);
  }
  // notch sends one wheel notch, if it can reach the session, and follows
  // where copy-mode goes.
  function notch(older) {
    if (!canSend()) return false;
    const target = term.element || host;
    const box = target.getBoundingClientRect?.() || { left: 0, top: 0, width: 0, height: 0 };
    const init = {
      deltaY: older ? -1 : 1, deltaMode: 1, clientX: box.left + box.width / 2, clientY: box.top + box.height / 2,
      bubbles: true, cancelable: true,
    };
    const View = documentRef.defaultView || globalThis;
    const event = typeof View.WheelEvent === 'function'
      ? new View.WheelEvent('wheel', init)
      : Object.assign(new View.Event('wheel', { bubbles: true, cancelable: true }), init);
    target.dispatchEvent(event);
    if (older) setCopyMode(true, inCopyMode ? scrolledBack + 1 : (remote ? 1 : 0));
    else if (inCopyMode) setCopyMode(scrolledBack > 1, scrolledBack - 1);
    return true;
  }
  // travel turns finger travel (down is older) into notches, keeping the rest.
  function travel(px) {
    if (!swipe) return;
    swipe.rest += px;
    const step = notchPx();
    let sent = 0;
    while (Math.abs(swipe.rest) >= step && sent < SWIPE_MAX_NOTCHES_PER_FRAME) {
      const older = swipe.rest > 0;
      if (!notch(older)) { swipe.rest = 0; return; }
      swipe.rest += older ? -step : step;
      sent += 1;
    }
  }
  function stopCoasting() {
    if (coast !== null && typeof cancelAnimationFrameImpl === 'function') cancelAnimationFrameImpl(coast);
    coast = null;
  }
  function coastFrom(velocity, at) {
    if (typeof requestAnimationFrameImpl !== 'function') return;
    let speed = velocity;
    let last = at;
    const frame = () => {
      coast = null;
      if (disposed || !swipe) return;
      const time = now();
      const dt = Math.max(1, time - last);
      last = time;
      travel(speed * dt);
      speed *= SWIPE_DECAY ** (dt / 16);
      if (Math.abs(speed) >= SWIPE_STOP) coast = requestAnimationFrameImpl(frame);
    };
    coast = requestAnimationFrameImpl(frame);
  }
  function swipeStart(event) {
    stopCoasting();
    if (event.touches?.length !== 1) { swipe = null; return; }
    const { clientX: x, clientY: y } = event.touches[0];
    swipe = { x0: x, y0: y, y, at: now(), speed: 0, rest: 0, moving: false };
  }
  function swipeMove(event) {
    if (!swipe || event.touches?.length !== 1 || !mouseTaken()) return;
    const { clientX: x, clientY: y } = event.touches[0];
    if (!swipe.moving) {
      const dy = y - swipe.y0;
      if (Math.abs(dy) < SWIPE_START_PX || Math.abs(dy) < Math.abs(x - swipe.x0)) return;
      swipe.moving = true;
    }
    event.preventDefault();
    const time = now();
    const dy = y - swipe.y;
    swipe.speed = dy / Math.max(1, time - swipe.at);
    swipe.y = y;
    swipe.at = time;
    travel(dy);
  }
  function swipeEnd(event) {
    if (!swipe || (event.touches?.length ?? 0) > 0) return;
    const fling = swipe.moving && Math.abs(swipe.speed) >= SWIPE_FLING && now() - swipe.at < SWIPE_REST_MS;
    if (fling) coastFrom(swipe.speed, now());
    else swipe = null;
  }
  // toLive scrolls back down to live: tmux's wheel copy-mode leaves itself
  // at the bottom. One notch more covers a count that drifted (scrolling up
  // past the top of the history); a notch down when live does nothing.
  function toLive() {
    if (disposed) return;
    stopCoasting();
    swipe = null;
    if (!inCopyMode) return;
    const notches = Math.max(1, scrolledBack) + 1;
    setCopyMode(false, 0);
    for (let i = 0; i < notches; i += 1) notch(false);
  }

  const pinchListeners = [
    ...(touchZoom ? [['touchstart', pinchStart], ['touchmove', pinchMove], ['touchend', pinchEnd], ['touchcancel', pinchEnd]] : []),
    ...(touchScroll ? [['touchstart', swipeStart], ['touchmove', swipeMove], ['touchend', swipeEnd], ['touchcancel', swipeEnd]] : []),
  ];
  for (const [type, listener] of pinchListeners) host.addEventListener?.(type, listener, { capture: true, passive: false });

  function setSoftKeyboard(on) {
    const textarea = term.textarea;
    if (disposed || !textarea) return;
    // A browser raises its keyboard when an element takes focus, not when
    // inputmode changes under focus: a terminal already focused (tapped)
    // lets go first.
    if (on && documentRef.activeElement === textarea) textarea.blur?.();
    textarea.inputMode = on ? 'text' : 'none';
    if (on) term.focus();
    else textarea.blur?.();
  }
  if (!softKeyboard) setSoftKeyboard(false);

  // sendInput delivers bytes as if typed, by the path keystrokes take. A
  // view scrolled back goes live first: tmux copy-mode would take the keys.
  function sendInput(data) {
    if (disposed || !ws || ws.readyState !== WebSocketCtor.OPEN) return false;
    if (remote && remoteState.mode !== 'interactive') return false;
    if (inCopyMode) toLive();
    ws.send(new TextEncoder().encode(data));
    return true;
  }

  function setStatus(next) {
    if (disposed) return;
    status = next;
    onStatus(next);
  }

  function setWaiting(next) {
    if (disposed) return;
    const same = waiting === next || (waiting && next && waiting.kind === next.kind && waiting.text === next.text);
    if (same) return;
    waiting = next;
    onWaitingChange(next);
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
    // A pending zoom settles with its own fit. Until then the font is the old
    // one, and a remote host's panel may be shrunk by the preview's scrollbars,
    // so a fit here would report the peer a wrong grid.
    if (disposed || zoomPending !== null) return;
    if (remote && remoteState.shared) { fitShared(); return; }
    if (remote) { fitRemote(); return; }
    try { fitAddon.fit(); } catch (_) { /* host may not be laid out yet */ }
  }

  // fitShared measures the grid this panel could show and reports it to the
  // peer. The grid itself stays at the shared size the peer sends, which can
  // be smaller (another client limits it) but is never stretched.
  function fitShared() {
    // A hidden host proposes a garbage grid; strictly zero, as in
    // sendDimensions, so a test DOM without layout still fits.
    if (host.offsetWidth === 0 || host.offsetHeight === 0) return;
    let dims = null;
    try { dims = fitAddon.proposeDimensions?.(); } catch (_) { /* host may not be laid out yet */ }
    const cols = Math.floor(Number(dims?.cols)); const rows = Math.floor(Number(dims?.rows));
    if (!(cols > 0) || !(rows > 0)) return;
    desired = { cols: Math.min(MAX_REMOTE_COLS, cols), rows: Math.min(MAX_REMOTE_ROWS, rows) };
    sendDesired();
    updateLimited();
  }

  function sendDesired() {
    if (!desired) return;
    const key = `${desired.cols}x${desired.rows}`;
    if (key !== lastDesired && sendDimensions(desired.cols, desired.rows)) {
      lastDesired = key;
      cancelLimitedHold();
      limitedHold = setTimeoutImpl(() => { limitedHold = null; updateLimited(); }, REMOTE_LIMITED_GRACE_MS);
    }
  }

  // updateLimited says whether the shared grid is smaller than this panel. It
  // shows only once the peer had time to answer this view's latest report, so
  // the view's own resize never flashes the note; clearing it is immediate.
  function updateLimited() {
    const limited = !!(remoteState.shared && desired && (term.cols < desired.cols || term.rows < desired.rows));
    if (limited && limitedHold) return;
    if (limited !== remoteState.limited) setRemote({ limited });
  }

  // fitRemote scales the font so the peer's grid fits the host, never
  // resizing the grid itself.
  function fitRemote() {
    // Reads xterm's own grid box; the core never writes shell DOM.
    const screen = host.getElementsByClassName?.('xterm-screen')[0];
    if (zoomFont !== null) return;
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
    if (term.options.fontSize !== font) reportFont();
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
    // A remote grid is the peer's: a shared view reports its panel, never the
    // grid it was sent; a pinned view reports nothing.
    if (remote) {
      if (remoteState.shared) sendDesired();
      return false;
    }
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

  function scheduleRedial(delay) {
    setReconnectAvailable(false);
    retryTimer = setTimeoutImpl(() => {
      retryTimer = null;
      void dial();
    }, Math.max(0, Number(delay) || 0));
  }

  function scheduleInitialRetry() {
    if (!initialRetry || retryIndex >= initialRetryDelays.length) return false;
    const delay = initialRetryDelays[retryIndex];
    retryIndex += 1;
    setStatus('retrying…');
    scheduleRedial(delay);
    return true;
  }

  function startWaitOpen() {
    return startingSince !== null && now() - startingSince < startWaitMs;
  }

  // scheduleStartRetry waits for an agent that is still starting: it opens the
  // start wait on the first 'starting' answer and redials with its backoff
  // until the wait runs out. False once it has.
  function scheduleStartRetry() {
    if (startingSince === null) startingSince = now();
    if (!startWaitOpen() || !startRetryDelays.length) return false;
    const delay = startRetryDelays[Math.min(startIndex, startRetryDelays.length - 1)];
    startIndex += 1;
    const text = `Waiting for ${agentLabel || 'the agent'} to start…`;
    setStatus(text.charAt(0).toLowerCase() + text.slice(1));
    setWaiting({ kind: 'starting', text });
    scheduleRedial(delay);
    return true;
  }

  // scheduleTransientRetry redials a terminal that dropped without a settled
  // answer. Inside an open start wait the drop is part of waiting for the
  // launch (the daemon may be busy creating the pane), so it keeps that wait;
  // otherwise it spends one of the few quiet redials. False when neither
  // applies and the caller settles.
  function scheduleTransientRetry() {
    if (startWaitOpen()) return scheduleStartRetry();
    if (!autoReattach || transientIndex >= transientRetryDelays.length) return false;
    const delay = transientRetryDelays[transientIndex];
    transientIndex += 1;
    reattachNext = !remote;
    setStatus('reconnecting…');
    setWaiting({ kind: 'reconnecting', text: 'Reconnecting…' });
    scheduleRedial(delay);
    return true;
  }

  // attached marks the moment the terminal proved it is attached (its first
  // output): the start wait is over, and the initial retries, which belong to
  // the first attach only, are spent — a later drop is the quiet redials' to
  // handle, which never take the session back (reattach=1). Their budget is
  // restored only by a connection that then stays up (see onclose), so a
  // command that draws and dies at once cannot redial forever.
  function attached() {
    startingSince = null;
    startIndex = 0;
    retryIndex = initialRetryDelays.length;
    setWaiting(null);
  }

  // settle ends any waiting: the pane now shows a settled state.
  function settle(text) {
    setWaiting(null);
    setStatus(text);
    setReconnectAvailable(true);
  }

  async function dial() {
    if (disposed) return false;
    generation += 1;
    const mine = generation;
    postAttachResizeScheduled = false;
    firstOutputSeen = false;
    firstOutputParsedAt = 0;
    lastOutputParsedAt = 0;
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
          settle('authentication required');
          return false;
        }
      } catch (error) {
        if (disposed || mine !== generation || controller.signal.aborted) return false;
        if (startWaitOpen() && scheduleStartRetry()) return false;
        if (scheduleInitialRetry()) return false;
        if (transientIndex > 0 && scheduleTransientRetry()) return false;
        settle('disconnected');
        watchForRestart();
        return false;
      } finally {
        if (authController === controller) authController = null;
      }
    }

    if (disposed || mine !== generation) return false;
    const proto = locationRef.protocol === 'https:' ? 'wss:' : 'ws:';
    const takeOver = remote && takeOverNext;
    takeOverNext = false;
    const reattach = reattachNext;
    reattachNext = false;
    const query = takeOver ? '&take_over=1' : reattach ? `${wsPath.includes('?') ? '&' : '?'}reattach=1` : '';
    const socket = new WebSocketCtor(proto + '//' + locationRef.host + wsPath + query);
    if (remote) { setRemote(REMOTE_RESET); desired = null; lastDesired = ''; cancelLimitedHold(); }
    socket.binaryType = 'arraybuffer';
    ws = socket;
    let openedAt = null;
    // A redial inside a wait keeps saying what it is waiting for.
    if (!waiting) setStatus('connecting…');
    socket.onopen = () => {
      if (disposed || mine !== generation || ws !== socket) return;
      openedAt = now();
      // agentd leaves copy-mode when a terminal socket closes: a new one
      // starts live.
      setCopyMode(false, 0);
      captureInstanceBaseline(mine);
      setReconnectAvailable(false);
      if (remote) { if (!waiting) setStatus('connecting to the peer…'); return; }
      // agentd answers a starting agent by closing an opened socket, so the
      // wait ends only on output, never on the open alone.
      if (!waiting) setStatus('connected');
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
          markOutputParsed();
          if (disposed || mine !== generation || ws !== socket || socket.readyState !== WebSocketCtor.OPEN || !bytes.byteLength) return;
          socket.send(JSON.stringify({ type: 'credit', bytes: bytes.byteLength }));
        });
        return;
      }
      if (!firstOutputSeen) { attached(); setStatus('connected'); }
      term.write(event.data instanceof ArrayBuffer ? new Uint8Array(event.data) : event.data, markOutputParsed);
      // WebSocket open precedes PTY creation on the server. The first output is
      // the browser's earliest proof that the command/tmux attach is actually
      // producing a screen, so anchor the delayed harness reflow here instead
      // of to a fixed interval from socket open. Only the first frame arms it.
      firstOutputSeen = true;
      scheduleRepairAfterOutput(mine, socket);
    };
    socket.onclose = (event) => {
      if (disposed || mine !== generation || ws !== socket) return;
      setCopyMode(false, 0);
      cancelPostAttachResize();
      cancelInitialResize();
      // A connection that drew and stayed up earns a fresh set of quiet redials.
      if (firstOutputSeen && openedAt !== null && now() - openedAt >= initialRetryStabilityMs) transientIndex = 0;
      const starting = remote ? REMOTE_FOLLOW.has(remoteState.closed?.reason) : event?.code === TERMINAL_STARTING;
      if (starting) {
        if (scheduleStartRetry()) return;
        if (remote) setRemote({ files: false, viewer: '' });
        settle(remote ? `closed: ${remoteCloseText(remoteState.closed)}` : `${agentLabel || 'the agent'} did not start in time`);
        onDisconnect();
        return;
      }
      const exit = !remote && TERMINAL_EXIT[event?.code];
      // A pop-out or reattach can briefly lose to the client it replaces: the
      // old socket's teardown detaches every client, ours included, which
      // reads as a detach. Inside the initial-retry window that race still
      // earns its retry before an exit code is trusted.
      if (exit && (openedAt === null || now() - openedAt < initialRetryStabilityMs) && scheduleInitialRetry()) return;
      if (exit) {
        settle(exit.text);
        // A quiet redial's "detached" may only be this browser's own stale
        // client (a half-open socket agentd has not noticed yet): it never
        // closes the tab, and Reconnect stays to take the session by hand.
        onDisconnect({ closeTab: exit.closeTab && !reattach });
        return;
      }
      if (remote) {
        // The pinned viewer is gone with the socket: no more file links.
        setRemote({ files: false, viewer: '' });
        const closed = remoteState.closed;
        // An unreachable peer or a failed stream is worth the quiet redials;
        // every other reason is the peer's settled answer.
        if ((!closed || REMOTE_TRANSIENT.has(closed.reason)) && scheduleTransientRetry()) return;
        settle(closed ? `closed: ${remoteCloseText(closed)}` : 'disconnected');
        setReconnectAvailable(!(closed && REMOTE_CLOSE[closed.reason]?.final));
        onDisconnect();
        return;
      }
      const unstable = openedAt === null || now() - openedAt < initialRetryStabilityMs;
      if (unstable && scheduleInitialRetry()) return;
      if (scheduleTransientRetry()) return;
      settle('disconnected');
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
        if (remoteState.shared) updateLimited();
        else if (typeof requestAnimationFrameImpl === 'function') requestAnimationFrameImpl(fit);
      }
    };
    const othersOf = (list) => (!Array.isArray(list) ? [] : list
      .filter((o) => typeof o === 'string')
      .map((o) => o.replace(/[\u0000-\u001f\u007f-\u009f]/g, ' ').trim().slice(0, 80))
      .filter(Boolean)
      .slice(0, 4));
    if (msg?.type === 'hello') {
      // The peer attached this view: any wait is over.
      attached();
      const mode = msg.mode === 'interactive' ? 'interactive' : 'watch';
      term.options.disableStdin = mode !== 'interactive';
      size();
      // files says the peer shares this terminal's files with us
      // (sessions.files.read); viewer_id pins downloads to this very view.
      const viewer = typeof msg.viewer_id === 'string' && /^[A-Za-z0-9_.:-]{1,128}$/.test(msg.viewer_id) ? msg.viewer_id : '';
      // shared: the peer sizes its window to the smallest attached client, so
      // this view reports what its panel could show (at the normal font) and
      // renders the grid it is sent. others: what else is attached;
      // take_over: the peer lets this node disconnect them.
      const shared = msg.shared === true;
      setRemote({ mode, files: msg.files === true && !!viewer, viewer, shared, others: othersOf(msg.others), canTakeOver: msg.take_over === true });
      setStatus(mode === 'interactive' ? 'connected · interactive' : 'connected · watch-only');
      if (shared) {
        term.options.fontSize = zoomFont ?? REMOTE_FONT_MAX;
        reportFont();
        initialResizeReady = true;
        lastDesired = '';
        fit();
      }
    } else if (msg?.type === 'size') {
      size();
    } else if (msg?.type === 'others') {
      setRemote({ others: othersOf(msg.others) });
    } else if (msg?.type === 'closed') {
      setRemote({ closed: { reason: String(msg.reason || ''), message: String(msg.message || '') } });
    }
  }

  function markOutputParsed() {
    lastOutputParsedAt = now();
    if (!firstOutputParsedAt) firstOutputParsedAt = lastOutputParsedAt;
  }

  // settled resolves true once this terminal has drawn output and xterm has
  // then been idle for quietMs, or maxAfterOutputMs after the first output
  // was parsed — a busy harness (spinner, elapsed-time counter) redraws
  // forever and never goes quiet, but its expensive full-screen attach redraw
  // is parsed by then. Resolves false at timeoutMs (no output at all) or on
  // disposal. Lets a caller opening several terminals bring them up one at a
  // time instead of parsing every harness's full redraw on the main thread at
  // once.
  function settled({ quietMs = 150, maxAfterOutputMs = 750, timeoutMs = 6000 } = {}) {
    const started = now();
    return new Promise((resolve) => {
      const check = () => {
        if (disposed) { resolve(false); return; }
        const at = now();
        if (lastOutputParsedAt
          && (at - lastOutputParsedAt >= quietMs || at - firstOutputParsedAt >= maxAfterOutputMs)) {
          resolve(true);
          return;
        }
        if (at - started >= timeoutMs) { resolve(false); return; }
        setTimeoutImpl(check, 50);
      };
      check();
    });
  }

  function connect() {
    cancelRetry();
    retryIndex = 0;
    reattachNext = false;
    startingSince = null;
    startIndex = 0;
    transientIndex = 0;
    setWaiting(null);
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
    // A remote stream's clipboard writes copy only the cells a pointer
    // selection covered (terminal-interactions.js selectedText).
    oscClipboard: remote ? 'selection' : true,
  });

  disposables.push(term.onData((data) => {
    if (remote && remoteState.mode !== 'interactive') return;
    if (inCopyMode && !MOUSE_REPORT.test(data)) toLive();
    if (!disposed && ws && ws.readyState === WebSocketCtor.OPEN) {
      ws.send(new TextEncoder().encode(data));
    }
  }));
  disposables.push(term.onResize(sendResize));

  const observer = typeof ResizeObserverCtor === 'function'
    // A shared remote view reports its panel even when inactive: a visible
    // split pane still takes part in the peer's shared size.
    ? new ResizeObserverCtor(() => { if (!disposed && (isActive || remote && remoteState.shared)) fit(); })
    : null;
  observer?.observe(host);

  return Object.freeze({
    connect,
    settled,
    fit,
    focus,
    sendResize,
    // takeOver reopens a remote terminal asking the peer to disconnect the
    // session's other viewers, so this view owns its size.
    takeOver() {
      if (disposed || !remote) return Promise.resolve(false);
      takeOverNext = true;
      return connect();
    },
    zoomBy: (step) => zoomBy(step),
    zoomWheel,
    // key sends a named key (NAMED_KEYS, the arrows) or a single printable
    // character, and says whether it went.
    key(name) {
      const arrow = CURSOR_KEYS[name];
      if (arrow) return sendInput(`${term.modes?.applicationCursorKeysMode ? '\x1bO' : '\x1b['}${arrow}`);
      if (Object.hasOwn(NAMED_KEYS, name)) return sendInput(NAMED_KEYS[name]);
      return typeof name === 'string' && name.length === 1 ? sendInput(name) : false;
    },
    setSoftKeyboard,
    // toLive scrolls a swiped-back view down to live; scrolledBack says
    // whether it is back from live.
    toLive,
    scrolledBack: () => inCopyMode,
    resetZoom: () => setZoom(null),
    copy: () => disposed ? Promise.resolve() : interactions.copySelection(),
    // paste delivers text as terminal input, by the path keystrokes take, and
    // says whether it could: a closed socket or a watch-only view takes none.
    paste(text) {
      if (disposed || !ws || ws.readyState !== WebSocketCtor.OPEN) return false;
      if (remote && remoteState.mode !== 'interactive') return false;
      term.paste(text);
      return true;
    },
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
    waiting: () => waiting,
    isDisposed: () => disposed,
    dispose() {
      if (disposed) return;
      disposed = true;
      generation += 1;
      cancelRetry();
      cancelPostOpenFit();
      cancelInitialResize();
      cancelPostAttachResize();
      cancelLimitedHold();
      cancelRestartWatchIfAny();
      if (zoomFitTimer !== null) clearTimeoutImpl(zoomFitTimer);
      host.removeEventListener?.('wheel', zoomWheel, { capture: true });
      for (const [type, listener] of pinchListeners) host.removeEventListener?.(type, listener, { capture: true });
      stopCoasting();
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
