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
// a peer's per-node view, also lists the shown peers' unattached agents (read
// through the peer proxy, which serves them only to an unrestricted peer);
// those open as remote terminals.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';
import {
  openGroupWebTermPane, openWebTermPane, openWebWindowPane, waitForTerminalSettled,
} from './terminals-tab.js';
import { terminalTabStatus } from './terminal-tab-status.js';
import { tickedNodes } from './fleet-scope.js';

const html = htm.bind(h);

export const BACKGROUND_TERMINALS_PATH = '/api/background-terminals';
export const BACKGROUND_TERMINAL_KILL_PATH = '/api/background-terminals/kill';
const POLL_MS = 5000;
// An attach takes a moment to show up as a tmux client; re-read after it so
// the row does not linger (the open pane key already hides it meanwhile).
const SETTLE_MS = 1200;
const STALE_SECONDS = 60 * 60;
const PEER_AGENT_ID = /^agt_[A-Za-z0-9]{4,64}$/;

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

// peerBackgroundRows keeps the peer's agents, the rows a remote terminal can
// open, each tagged with its node.
export function peerBackgroundRows(rows, peer) {
  const node = { id: peer.id, name: peer.name, color: peer.color };
  return (rows || [])
    .filter((row) => row?.kind === 'agent' && PEER_AGENT_ID.test(row.agent_id || ''))
    .map((row) => ({ ...row, key: `peer:${peer.id}:${row.agent_id}`, killable: false, node }));
}

// readListing resolves to the listing's rows, null when it could not be read
// (keep the last good rows), or [] when the node refuses it.
async function readListing(fetchImpl, path) {
  try {
    const response = await fetchImpl(path, { credentials: 'same-origin' });
    if (response.status === 403 || response.status === 404) return [];
    if (!response.ok) return null;
    const body = await response.json();
    return Array.isArray(body?.terminals) ? body.terminals : [];
  } catch {
    return null;
  }
}

// startBackgroundTerminalsPoll reads the listing now and then every few
// seconds while the page is visible. peers names the peer nodes to read
// besides this one (backgroundPeers). Returns the stop function.
export function startBackgroundTerminalsPoll({
  fetchImpl = globalThis.__tclaudeRemoteNode?.localFetch || ((...args) => fetch(...args)),
  intervalMs = POLL_MS, doc = globalThis.document, peers = () => [],
} = {}) {
  let stopped = false;
  let inflight = false;
  let local = [];
  const remote = new Map();
  const read = async () => {
    if (stopped || inflight || doc?.hidden) return;
    inflight = true;
    try {
      const nodes = peers() || [];
      const [own, ...theirs] = await Promise.all([
        readListing(fetchImpl, BACKGROUND_TERMINALS_PATH),
        ...nodes.map((peer) => readListing(fetchImpl, `/api/peer/${encodeURIComponent(peer.id)}/background-terminals`)),
      ]);
      // A failed read keeps that node's last good rows; the next tick retries.
      if (own) local = own;
      const next = new Map();
      nodes.forEach((peer, i) => {
        const rows = theirs[i] ? peerBackgroundRows(theirs[i], peer) : remote.get(peer.id);
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
// remote pane shows it, whichever mode it opened in.
export function visibleBackgroundTerminals(rows, panes) {
  const open = new Set();
  for (const pane of panes || []) {
    open.add(pane.key);
    const remote = pane.seed?.remote;
    if (remote?.peer && remote.agent) open.add(`peer:${remote.peer}:${remote.agent}`);
  }
  return (rows || []).filter((row) => !row.key || !open.has(row.key));
}

const KIND_SECTIONS = Object.freeze([
  ['agent', 'Agents'],
  ['session', 'Sessions'],
  ['shell', 'Shells'],
  ['group_shell', 'Group shells'],
  ['other', 'Other tmux sessions'],
]);

// backgroundSections groups this node's rows by kind, then each peer's agents
// under their node.
function backgroundSections(rows) {
  const sections = KIND_SECTIONS.map(([kind, title]) => ({ key: kind, title, node: null, items: rows.filter((row) => !row.node && row.kind === kind) }));
  const peers = new Map();
  for (const row of rows) {
    if (!row.node) continue;
    if (!peers.has(row.node.id)) peers.set(row.node.id, { key: `peer:${row.node.id}`, title: 'Agents', node: row.node, items: [] });
    peers.get(row.node.id).items.push(row);
  }
  return [...sections, ...peers.values()].filter((section) => section.items.length);
}

function selectorOf(row) {
  return row.agent_id || row.conv_id || '';
}

// rowLabel prefers the roster's live title so a renamed agent reads right
// before the next listing.
function rowView(row, snapshotValue) {
  const selector = selectorOf(row);
  const status = selector ? terminalTabStatus({ label: row.label, seed: { agent: selector } }, snapshotValue) : null;
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
export function attachBackgroundTerminal(row, snapshotValue, { reveal = true, activate = true, openRemote = openRemoteAgent } = {}) {
  const { title } = rowView(row, snapshotValue);
  const selector = selectorOf(row);
  const options = { reveal, activate };
  let opened = null;
  if (row.node) {
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

// killBackgroundTerminal ends a killable row's tmux session (a shell, a
// superseded generation, an unrecognised session) and re-reads the listing.
// Resolves to '' on success or the server's error text.
export async function killBackgroundTerminal(row, { fetchImpl = (...args) => fetch(...args) } = {}) {
  try {
    const response = await fetchImpl(BACKGROUND_TERMINAL_KILL_PATH, {
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

// KillButton asks for a second click before killing: whatever runs in the
// session stops, and the popover is too small for a modal.
function KillButton({ row, label }) {
  const [armed, setArmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => {
    if (!armed) return undefined;
    const timer = setTimeout(() => setArmed(false), 3000);
    return () => clearTimeout(timer);
  }, [armed]);
  const onClick = async () => {
    if (!armed) { setArmed(true); setError(''); return; }
    setBusy(true);
    const failure = await killBackgroundTerminal(row);
    setBusy(false);
    setArmed(false);
    setError(failure);
  };
  return html`<button type="button" class=${`mux-btn mux-bg-kill${armed ? ' armed' : ''}`} disabled=${busy}
    onClick=${onClick} aria-label=${`Kill ${label}`}
    title=${error || (armed ? 'Click again to end this tmux session and whatever runs in it' : `End tmux session ${row.tmux_session}`)}>
    ${busy ? 'Killing…' : armed ? 'Kill?' : error ? 'Failed' : 'Kill'}</button>`;
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
      await waitForSettled(pane.key, { timeoutMs });
    }
  } finally {
    attachAllRunning = false;
  }
  return opened;
}

function BackgroundIcon() {
  return html`<svg class="mux-bg-icon" width="14" height="14" viewBox="0 0 16 16" fill="none"
    stroke="currentColor" stroke-width="1.4" aria-hidden="true" focusable="false">
    <rect x="1.5" y="2.5" width="13" height="9" rx="1.5" /><path d="M4 14h8" />
    <path d="M5.5 7.5h5" stroke-linecap="round" /></svg>`;
}

// BackgroundTerminalsChip is the right-aligned counter beside the tab strip.
// Hover peeks the breakdown; a click pins it open so the attach buttons are
// reachable without holding the pointer still.
export function BackgroundTerminalsChip({ rows, snapshot = null }) {
  const [open, setOpen] = useState(false);
  const [pinned, setPinned] = useState(false);
  const rootRef = useRef(null);
  const closeTimer = useRef(0);
  const count = rows.length;

  const close = () => { setOpen(false); setPinned(false); };
  const cancelClose = () => { clearTimeout(closeTimer.current); closeTimer.current = 0; };
  const scheduleClose = () => {
    if (pinned) return;
    cancelClose();
    closeTimer.current = setTimeout(() => setOpen(false), 220);
  };
  useEffect(() => () => cancelClose(), []);
  useEffect(() => {
    if (!open) return undefined;
    const onDown = (event) => { if (!rootRef.current?.contains(event.target)) close(); };
    const onKey = (event) => { if (event.key === 'Escape') close(); };
    document.addEventListener('pointerdown', onDown, true);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('pointerdown', onDown, true);
      document.removeEventListener('keydown', onKey);
    };
  }, [open]);
  useEffect(() => { if (!count) close(); }, [count]);
  if (!count) return null;

  const snapshotValue = snapshot?.value;
  const attachable = rows.filter((row) => row.key);
  const attach = (row) => {
    attachBackgroundTerminal(row, snapshotValue);
  };
  const attachAll = () => {
    void attachAllBackgroundTerminals(attachable, snapshotValue);
    close();
  };
  const now = Date.now() / 1000;
  const noun = count === 1 ? 'session' : 'sessions';

  return html`
    <div ref=${rootRef} class=${`mux-bg${open ? ' open' : ''}`}
      onMouseEnter=${() => { cancelClose(); setOpen(true); }} onMouseLeave=${scheduleClose}>
      <button type="button" class="mux-bg-trigger" aria-expanded=${open ? 'true' : 'false'} aria-haspopup="dialog"
        aria-label=${`${count} tclaude ${noun} running in the background without a terminal`}
        title=${`${count} tclaude ${noun} still running in tmux with no terminal attached`}
        onClick=${() => { if (pinned) close(); else { setOpen(true); setPinned(true); } }}>
        <${BackgroundIcon} /><span class="mux-bg-count">${count}</span>
      </button>
      ${open ? html`
        <div class="mux-bg-pop" role="dialog" aria-label="Background terminals">
          <div class="mux-bg-pop-head"><b>${count} running in background</b><span> · no terminal attached</span></div>
          ${backgroundSections(rows).map(({ key, title, node, items }) => html`
              <div class="mux-bg-sec" key=${key}>
                <div class="mux-bg-sec-head">${node ? html`<span class="mux-node-pill" style=${`--nc:${node.color}`}>${node.name}</span>` : null}${title} · ${items.length}</div>
                ${items.map((row) => {
                  const view = rowView(row, snapshotValue);
                  const idle = idleText(row.last_activity, now);
                  // A shell's liveness is not its agent's status; only the
                  // agent/session rows borrow the roster light.
                  const conversation = row.kind === 'agent' || row.kind === 'session';
                  const statusClass = conversation ? (view.status?.className || 'unknown') : 'shell';
                  return html`
                    <div class="mux-bg-row" key=${row.key || row.tmux_session}>
                      <span class=${`mux-tab-status mux-tab-status-${statusClass}`}
                        title=${conversation && view.status?.agent ? view.status.description : ''}></span>
                      <span class="mux-bg-name" title=${view.label}>${view.label}</span>
                      ${idle.text ? html`<span class=${`mux-bg-idle${idle.stale ? ' stale' : ''}`}>${idle.text}</span>` : null}
                      ${row.key
                        ? html`<button type="button" class="mux-btn mux-bg-attach" onClick=${() => attach(row)}
                            aria-label=${`Attach ${view.label}`}>Attach</button>`
                        : html`<span class="mux-bg-noattach" title=${`Attach from a shell: tmux -L tclaude attach -t '${row.tmux_session}'`}>${row.superseded ? 'previous generation' : 'not a tclaude terminal'}</span>`}
                      ${row.killable ? html`<${KillButton} row=${row} label=${view.label} />` : null}
                    </div>`;
                })}
              </div>`)}
          ${attachable.length > 1 ? html`
            <div class="mux-bg-foot"><button type="button" class="mux-btn" onClick=${attachAll}>Attach all</button></div>
          ` : null}
        </div>
      ` : null}
    </div>
  `;
}
