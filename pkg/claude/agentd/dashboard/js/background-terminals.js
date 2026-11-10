// background-terminals.js — tclaude tmux sessions still running with nothing
// attached to them: agents, plain sessions and shells the operator minimized
// or closed the tab on. The Terminals tab shows them as a counter beside the
// tab strip (with an attach popover) so idle background sessions are not left
// running by mistake. GET /api/background-terminals is the source; a session
// counts as background only while no tmux client (web terminal or native
// window) is attached to it.
//
// The listing is always this node's, read past a peer view's fetch routing:
// the browser's terminals attach through this node's daemon. A fused view, or
// a peer's per-node view, also lists the shown peers' unattached agents and
// shells (read through the peer proxy, which serves them only to an
// unrestricted peer). A peer's agents and shells open as remote terminals,
// and its shells can be ended from here.
//
// The listing also has a row per drawing board with an agent's drawing board session on it: the
// agent works with no tab open, and the row is how the operator resumes the
// drawing board. It opens as a drawing board tab and ends without touching the drawing board. Drawing boards
// stay with their own node, so a peer never lists one, and a peer's per-node
// view drops this node's, whose drawing board routes the page would send to the peer.

import { signal } from '@preact/signals';
import {
  openGroupWebTermPane, openWebTermPane, openWebWindowPane, waitForTerminalSettled,
} from './terminals-tab.js';
import { drawingBoardPaneKey } from './drawing-board-model.js';
import { drawingBoardOverlays, openDrawingBoardPane } from './drawing-board-overlay-state.js';
import { terminalTabStatus } from './terminal-tab-status.js';
import { tickedNodes } from './fleet-scope.js';
import { remoteSessionAgents } from './remote-session-status.js';

export const BACKGROUND_TERMINALS_PATH = '/api/background-terminals';
export const BACKGROUND_TERMINAL_KILL_PATH = '/api/background-terminals/kill';
const POLL_MS = 5000;
// An attach takes a moment to show up as a tmux client; re-read after it so
// the row does not linger (the open pane key already hides it meanwhile).
const SETTLE_MS = 1200;
const STALE_SECONDS = 60 * 60;
const PEER_AGENT_ID = /^agt_[A-Za-z0-9]{4,64}$/;
// A peer that refuses its listing (it does not trust this node unrestricted)
// is asked again only after this long: each ask costs it a peer-view stream.
const PEER_REFUSED_BACKOFF_MS = 5 * 60 * 1000;
const REFUSED = Symbol('refused');

export const backgroundTerminals = signal([]);

let refreshImpl = null;

// refreshBackgroundTerminals asks the running poller for an immediate read.
export function refreshBackgroundTerminals() {
  refreshImpl?.();
}

// backgroundPeers lists the peers whose unattached agents the counter also
// shows: a fused view's ticked peers and the peer a per-node view shows.
export function backgroundPeers(view, remote = '') {
  const fleet = view?.fleet;
  if (!fleet) return [];
  const ids = new Set(remote ? [remote] : []);
  for (const node of tickedNodes(fleet, view.fused)) if (!node.local) ids.add(node.id);
  return fleet.peers.filter((peer) => ids.has(peer.id) && peer.online);
}

// PEER_SHELL_ATTACH is why a peer's shell has no Attach here: one from before
// shells were filed under their group, which the federation terminal stream
// addresses a shell by.
export const PEER_SHELL_ATTACH = "This shell predates group shells: attach it on that node";

// peerBackgroundRows keeps the peer's agents (the rows a remote terminal can
// open) and its tclaude shells, each tagged with its node. A peer lists only
// what its grants open from here (peer_background_terminals.go): every row
// under unrestricted trust, otherwise the shells of groups granting
// sessions.shell and the agents of groups granting a terminal. Its shells
// attach over the federation terminal stream (remote-terminal.js
// openRemoteShell) when they name their group.
export function peerBackgroundRows(rows, peer) {
  const node = { id: peer.id, name: peer.name, color: peer.color };
  return (rows || []).flatMap((row) => {
    if (row?.kind === 'agent' && PEER_AGENT_ID.test(row.agent_id || '')) {
      return [{ ...row, key: `peer:${peer.id}:${row.agent_id}`, killable: false, node }];
    }
    if ((row?.kind === 'shell' || row?.kind === 'group_shell') && row.tmux_session) {
      const shell = peerShell(row);
      return [shell
        ? { ...row, key: `peer:${peer.id}:${row.tmux_session}`, killable: !!row.killable, shell, node }
        : { ...row, key: '', killable: !!row.killable, attachLock: PEER_SHELL_ATTACH, node }];
    }
    return [];
  });
}

// hasPeerBackgroundAgent reports whether rows list a peer's agent. Its status
// light reads the federation session catalog (remote-session-status.js), so
// that catalog has to be polled while such a row is listed, not only while a
// remote pane is open: a backgrounded peer agent has no pane by definition.
export function hasPeerBackgroundAgent(rows) {
  return (rows || []).some((row) => row.node && row.kind === 'agent');
}

// peerShell is the openRemoteShell target of a peer's shell row, or null.
function peerShell(row) {
  if (!row.group) return null;
  if (row.kind === 'group_shell') return { group: row.group, shell: 'group' };
  return PEER_AGENT_ID.test(row.agent_id || '') && !row.superseded ? { group: row.group, shell: row.which || 'current', agent: row.agent_id } : null;
}

// readListing resolves to the listing's rows, null when it could not be read
// (keep the last good rows), or REFUSED when the node refuses it.
async function readListing(fetchImpl, path) {
  try {
    const response = await fetchImpl(path, { credentials: 'same-origin' });
    if (response.status === 403 || response.status === 404) return REFUSED;
    if (!response.ok) return null;
    const body = await response.json();
    return Array.isArray(body?.terminals) ? body.terminals : [];
  } catch {
    return null;
  }
}

// localBackgroundRows is the rows of this node's listing the page can open.
export function localBackgroundRows(rows, remote = globalThis.__tclaudeRemoteNode?.id) {
  return remote ? rows.filter((row) => row.kind !== 'drawing_board') : rows;
}

// startBackgroundTerminalsPoll reads the listing now and then every few
// seconds while the page is visible. peers names the peer nodes to read
// besides this one (backgroundPeers). Returns the stop function.
export function startBackgroundTerminalsPoll({
  fetchImpl = globalThis.__tclaudeRemoteNode?.localFetch || ((...args) => fetch(...args)),
  intervalMs = POLL_MS, doc = globalThis.document, peers = () => [], now = () => Date.now(),
} = {}) {
  let stopped = false;
  let inflight = false;
  let again = false;
  let local = [];
  const remote = new Map();
  const refusedUntil = new Map();
  const read = async () => {
    if (stopped || doc?.hidden) return;
    // A read asked for mid-poll (a scope change) runs once this one is done.
    if (inflight) { again = true; return; }
    inflight = true;
    try {
      const nodes = (peers() || []).filter((peer) => !(refusedUntil.get(peer.id) > now()));
      const [own, ...theirs] = await Promise.all([
        readListing(fetchImpl, BACKGROUND_TERMINALS_PATH),
        ...nodes.map((peer) => readListing(fetchImpl, `/api/peer/${encodeURIComponent(peer.id)}/background-terminals`)),
      ]);
      // A failed read keeps that node's last good rows; the next tick retries.
      if (own && own !== REFUSED) local = localBackgroundRows(own);
      const next = new Map();
      nodes.forEach((peer, i) => {
        if (theirs[i] === REFUSED) refusedUntil.set(peer.id, now() + PEER_REFUSED_BACKOFF_MS);
        const rows = theirs[i] === REFUSED ? [] : theirs[i] ? peerBackgroundRows(theirs[i], peer) : remote.get(peer.id);
        if (rows) next.set(peer.id, rows);
      });
      remote.clear();
      for (const [id, rows] of next) remote.set(id, rows);
      const rows = [...local, ...[...remote.values()].flat()];
      // Publish only on change so an unchanged poll re-renders nothing.
      if (!stopped && JSON.stringify(rows) !== JSON.stringify(backgroundTerminals.peek())) {
        backgroundTerminals.value = rows;
      }
    } finally {
      inflight = false;
      if (again) { again = false; void read(); }
    }
  };
  const timer = setInterval(read, intervalMs);
  const onVisible = () => { if (!doc?.hidden) void read(); };
  doc?.addEventListener?.('visibilitychange', onVisible);
  refreshImpl = () => void read();
  void read();
  return () => {
    stopped = true;
    clearInterval(timer);
    doc?.removeEventListener?.('visibilitychange', onVisible);
    if (refreshImpl) refreshImpl = null;
  };
}

// visibleBackgroundTerminals drops rows whose pane is already open here: the
// listing lags an attach by up to one poll. A peer's agent is open when a
// remote pane shows it, whichever mode it opened in. A drawing board is open in its tab
// or floating in a window.
export function visibleBackgroundTerminals(rows, panes, floating = drawingBoardOverlays.value) {
  const open = new Set();
  for (const pane of panes || []) {
    open.add(pane.key);
    const remote = pane.seed?.remote;
    if (remote?.peer && remote.agent) open.add(`peer:${remote.peer}:${remote.agent}`);
  }
  for (const window of floating || []) open.add(drawingBoardPaneKey(window.drawingBoard));
  return (rows || []).filter((row) => !row.key || !open.has(row.key));
}

function selectorOf(row) {
  return row.agent_id || row.conv_id || '';
}

// drawingBoardRowView reads a drawing board row: its title and who has a session on it, by
// the roster's live titles like the other rows.
function drawingBoardRowView(row, snapshotValue) {
  const roster = new Map((snapshotValue?.agents || []).map((agent) => [agent.agent_id, agent.title]));
  const names = (row.agents || []).map((agent) => roster.get(agent.agent_id) || agent.title).filter(Boolean);
  const title = row.label || row.drawingBoard;
  return { label: names.length ? `${title} · ${names.join(', ')}` : title, title, status: null };
}

// rowLabel prefers the roster's live title so a renamed agent reads right
// before the next listing.
export function rowView(row, snapshotValue) {
  if (row.kind === 'drawing_board') return drawingBoardRowView(row, snapshotValue);
  const selector = selectorOf(row);
  const seed = row.node ? { remote: { peer: row.node.id, agent: row.agent_id } } : { agent: selector };
  const status = selector ? terminalTabStatus({ label: row.label, seed }, snapshotValue, remoteSessionAgents.value) : null;
  const title = status?.agent?.title || row.label || row.tmux_session;
  let label = title;
  if (row.kind === 'shell') label = `shell · ${title}${row.which && row.which !== 'current' ? ` (${row.which})` : ''}`;
  if (row.kind === 'group_shell') label = `group shell · ${row.group}`;
  return { label, title, status };
}

export function idleText(lastActivity, nowSeconds = Date.now() / 1000) {
  const at = Number(lastActivity || 0);
  if (!at) return { text: '', stale: false };
  const age = Math.max(0, nowSeconds - at);
  const text = age < 60 ? 'just now'
    : age < 3600 ? `${Math.floor(age / 60)}m`
      : age < 86400 ? `${Math.floor(age / 3600)}h`
        : `${Math.floor(age / 86400)}d`;
  return { text: age < 60 ? text : `idle ${text}`, stale: age >= STALE_SECONDS };
}

// attachBackgroundTerminal opens row as a normal terminal tab and returns the
// promise of its pane (null when the row cannot be attached from here).
export function attachBackgroundTerminal(row, snapshotValue, { reveal = true, activate = true, openRemote = openRemoteAgent, openShell = openRemoteShellPane } = {}) {
  const { title } = rowView(row, snapshotValue);
  const selector = selectorOf(row);
  const options = { reveal, activate };
  let opened = null;
  if (row.kind === 'drawing_board' && row.drawingBoard) {
    opened = openDrawingBoardPane(row.drawingBoard, title, options);
  } else if (row.node && row.shell) {
    opened = openShell({ instance: row.node.id, ...row.shell, peerLabel: row.node.name, label: title, options });
  } else if (row.node) {
    opened = openRemote({ instance: row.node.id, agent: row.agent_id, peerLabel: row.node.name, label: row.label || '', options });
  } else if ((row.kind === 'agent' || row.kind === 'session') && selector) {
    opened = openWebWindowPane(selector, title, { harness: row.harness || '', ...options });
  } else if (row.kind === 'shell' && selector && row.which) {
    opened = openWebTermPane(selector, title, row.which, { group: row.group || '', ...options });
  } else if (row.kind === 'group_shell' && row.group) {
    opened = openGroupWebTermPane(row.group, `group shell · ${row.group}`, options);
  }
  if (!opened) return null;
  setTimeout(refreshBackgroundTerminals, SETTLE_MS);
  return opened;
}

function openRemoteAgent({ options, ...rest }) {
  return import('./remote-terminal.js').then((m) => m.openRemoteTerminal({
    ...rest, open: (args) => m.openRemotePane(args, options),
  }));
}

function openRemoteShellPane({ options, ...rest }) {
  return import('./remote-terminal.js').then((m) => m.openRemoteShell({
    ...rest, open: (args) => m.openRemotePane(args, options),
  }));
}

// killBackgroundTerminal ends a killable row's tmux session (a shell, a
// superseded generation, an unrecognised session) on its node and re-reads
// the listing. Like the listing, it goes past a peer view's fetch routing: a
// row of this node's ends here, a peer's through the peer proxy. Resolves to
// '' on success or the server's error text. A drawing board row has no tmux session:
// this ends the agents' drawing board sessions, and the drawing board itself stays.
export async function killBackgroundTerminal(row, {
  fetchImpl = globalThis.__tclaudeRemoteNode?.localFetch || ((...args) => fetch(...args)),
} = {}) {
  const path = row.node ? `/api/peer/${encodeURIComponent(row.node.id)}/background-terminals/kill` : BACKGROUND_TERMINAL_KILL_PATH;
  try {
    const response = row.kind === 'drawing_board'
      ? await fetchImpl(`/api/drawing-boards/${encodeURIComponent(row.drawingBoard)}/sessions`, { method: 'DELETE', credentials: 'same-origin' })
      : await fetchImpl(path, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ tmux_session: row.tmux_session }),
      });
    if (response.ok) return '';
    const body = await response.json().catch(() => null);
    return body?.error || `kill failed (${response.status})`;
  } catch (error) {
    return String(error?.message || error);
  } finally {
    refreshBackgroundTerminals();
  }
}

let attachAllRunning = false;

// attachAllBackgroundTerminals brings the rows up ONE AT A TIME: each attach
// makes its harness redraw its whole screen, and xterm parses that on the main
// thread, so opening them all at once froze the page for seconds. Each pane
// takes focus as it opens, exactly like attaching it by hand: a hidden pane
// (display:none) cannot measure its grid, so it would attach at xterm's
// default 80x24, and with tmux's `window-size latest` that shrinks the
// harness's window and triggers a full reprint now and another on reveal.
// The next pane opens once the previous has attached and redrawn (or timed
// out).
export async function attachAllBackgroundTerminals(rows, snapshotValue, {
  waitForSettled = waitForTerminalSettled, timeoutMs = 6000,
} = {}) {
  if (attachAllRunning) return 0;
  attachAllRunning = true;
  let opened = 0;
  try {
    for (const row of rows) {
      if (!row.key) continue;
      const pane = await attachBackgroundTerminal(row, snapshotValue, { reveal: opened === 0 });
      if (!pane) continue;
      opened += 1;
      if (!pane.floating) await waitForSettled(pane.key, { timeoutMs });
    }
  } finally {
    attachAllRunning = false;
  }
  return opened;
}

// killShell ends one of this node's tclaude shells (a group's, or one in an
// agent's directory) whether or not a client is attached: the Resources
// row's ×. A pane showing it sees its session end. Resolves to '' on success
// or the server's error text.
export async function killShell(tmuxSession, {
  fetchImpl = globalThis.__tclaudeRemoteNode?.localFetch || ((...args) => fetch(...args)),
} = {}) {
  try {
    const response = await fetchImpl('/api/shells/kill', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ tmux_session: tmuxSession }),
    });
    if (response.ok) return '';
    const body = await response.json().catch(() => null);
    return body?.error || `kill failed (${response.status})`;
  } catch (error) {
    return String(error?.message || error);
  } finally {
    refreshBackgroundTerminals();
  }
}
