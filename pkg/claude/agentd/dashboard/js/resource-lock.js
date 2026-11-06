// resource-lock.js -- the agent lock on a group resource (tcl-dwzsq0) as a
// chip that is also its toggle: '🔒 agents: read-only' or '✎ agents can
// edit'. It sits on each row of a group's Resources section and on the
// resource's own header (process editor, board, page). In the Collab view a
// locked resource also wears a small LockBadge on its tab name and side panel
// row. The operator's dashboard toggles it (POST /api/resource-locks); an
// agent does through its own route under groups.resources.lock.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';
import { onPeerPage } from './node-api.js';

const html = htm.bind(h);

// lockLabel is the chip's text for a lock state.
export function lockLabel(locked) {
  return locked ? '🔒 agents: read-only' : '✎ agents can edit';
}

// lockTitle is the chip's tooltip: the state, what it means, the action.
export const lockTitle = (locked, name) => (locked
  ? `Locked: members can read ${name} but not edit it. Click to unlock`
  : `Unlocked: members can read and edit ${name}. Click to lock`);

// knownLocks remembers the last lock state a surface read or set, by
// "kind:id", so a surface with no fetch of its own can name the state. With
// the snapshot's rows (resourceLockState) it is the one lock state the tab
// name, the tab menu and the Collab panel row read, so a toggle in any of them
// shows in all three at once. A toggle's entry is pending: it wins over the
// snapshot row until a snapshot row agrees with it, or PENDING_MS passes (a
// re-read that failed or was superseded publishes nothing). lockVersion makes
// a component that reads it re-render when it changes.
const knownLocks = new Map();
const lockVersion = signal(0);
export const PENDING_MS = 15000;
const lockKey = (kind, id) => `${kind}:${id}`;

// pendingEntry is the entry while its toggle still waits for the snapshot.
function pendingEntry(entry, now = Date.now()) {
  return entry?.pendingUntil && now < entry.pendingUntil ? entry : null;
}

// rememberLock records what a surface read (a board's load). A read does not
// undo a toggle still waiting for its snapshot, unless it agrees with it.
export function rememberLock(kind, id, locked) {
  const key = lockKey(kind, id);
  const pending = pendingEntry(knownLocks.get(key));
  if (pending && pending.locked !== !!locked) return;
  knownLocks.set(key, { locked: !!locked, pendingUntil: 0 });
  lockVersion.value += 1;
}

// knownLock is the remembered state, or undefined when none was read.
export function knownLock(kind, id) {
  void lockVersion.value;
  return knownLocks.get(lockKey(kind, id))?.locked;
}

// lockRefresh re-reads the dashboard snapshot after a lock change; a test
// swaps run.
export const lockRefresh = {
  run: () => import('./refresh.js').then((m) => m.refresh()),
};

// lockChanged records a lock change this dashboard made: every surface shows
// it at once, and the snapshot is re-read so its rows catch up.
export function lockChanged(kind, id, locked, { now = Date.now() } = {}) {
  knownLocks.set(lockKey(kind, id), { locked: !!locked, pendingUntil: now + PENDING_MS });
  lockVersion.value += 1;
  return Promise.resolve().then(() => lockRefresh.run()).catch(() => {});
}

const kindNoun = { board: 'board', page: 'page', process: 'process' };

// lockBadgeTitle is the small lock icon's tooltip.
export const lockBadgeTitle = (kind) => `Locked: members can read this ${kindNoun[kind] || 'resource'} but not edit it. `
  + 'You, or an agent you granted groups.resources.lock, can unlock it.';

// resourceLocked reports whether a resource is locked against agents, from
// the snapshot's group resources, else from what a surface last read
// (knownLock): a board linked to no group is not in the snapshot.
export function resourceLocked(snapshotValue, kind, id) {
  return !!resourceLockState(snapshotValue, kind, id);
}

// resourceLockState is resourceLocked, or undefined when nothing says.
export function resourceLockState(snapshotValue, kind, id) {
  if (!kind || !id) return undefined;
  void lockVersion.value;
  const entry = knownLocks.get(lockKey(kind, id));
  const row = (snapshotValue?.group_resources || []).find((r) => r.kind === kind && r.id === id);
  if (pendingEntry(entry)) {
    if (!row || !!row.locked !== entry.locked) return entry.locked;
    // The snapshot caught up: from now on its row is the truth again. (No
    // signal bump: the state read here does not change.)
    entry.pendingUntil = 0;
  }
  if (row) return !!row.locked;
  return entry?.locked;
}

// LockBadge is the small lock icon a tab name or a Collab panel row wears
// while its resource is locked. Nothing shows while it is unlocked.
export function LockBadge({ kind, locked }) {
  if (!locked) return null;
  const title = lockBadgeTitle(kind);
  return html`<span class="res-lock-badge" role="img" aria-label=${title} title=${title} data-lock-badge=${kind}>🔒</span>`;
}

// setResourceLock is the lock toggle for a surface without the process api.
export async function setResourceLock(kind, id, locked, fetchImpl = (...args) => globalThis.fetch(...args)) {
  const response = await fetchImpl('/api/resource-locks', {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ kind, id, locked }),
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.message || body.error || `${response.status}`);
  void lockChanged(kind, id, locked);
  return body;
}

// LockChip shows and toggles one resource's agent lock. The group card passes
// its process api and context source; a resource's own header (board, page,
// process editor) passes onChanged instead, with the new state. A peer's
// group card passes peer: the fetch that reaches that node, and whether the
// node grants this one the lock there (groups.resources.lock).
export function LockChip({ kind, id, name, locked, api = null, source = null, onChanged = null, notify = () => {}, peer = null }) {
  const [busy, setBusy] = useState(false);
  const toggle = async (event) => {
    event.stopPropagation();
    if (busy) return;
    setBusy(true);
    try {
      if (peer) await setResourceLock(kind, id, !locked, peer.fetchImpl);
      else await (api?.setResourceLock ? api.setResourceLock(kind, id, !locked) : setResourceLock(kind, id, !locked));
      notify(locked ? `Agents can edit ${name} again` : `${name} is read-only for agents`);
      onChanged?.(!locked);
      await source?.refresh?.();
    } catch (cause) {
      notify(`Could not change the lock on ${name}: ${cause.message}`, true);
    } finally {
      setBusy(false);
    }
  };
  // On a peer's page this node's own surfaces (a board, a page) cannot reach
  // that peer's lock, so they show no chip; a peer's group card does (peer).
  if (onPeerPage() && !peer) return null;
  const denied = peer && !peer.allowed;
  const title = denied
    ? `${lockLabel(locked)}. ${peer.node || 'This node'} does not grant you groups.resources.lock here, so you cannot change it.`
    : lockTitle(locked, name);
  // A chip that is also the toggle: a .ui-tag pill, warn-coloured while
  // locked. It is a real button, so it takes keyboard focus and Enter/Space.
  return html`<button data-ui-exempt type="button" class=${`gr-lock ui-tag${locked ? ' ui-tag-warn' : ''}`} data-resource-lock=${`${kind}:${id}`}
    aria-pressed=${locked ? 'true' : 'false'} aria-label=${title} aria-disabled=${denied ? 'true' : undefined} aria-busy=${busy ? 'true' : undefined}
    disabled=${busy || denied} title=${title} onClick=${toggle}>${lockLabel(locked)}</button>`;
}


// railLockTitle is the Collab panel lock icon's tooltip; denied says why it
// cannot be clicked.
export function railLockTitle(locked, denied = false) {
  const state = locked ? 'Locked. Group members can read but not edit.' : 'Unlocked. Group members can edit.';
  if (denied) return `${state} Changing it needs groups.resources.lock, which this node does not grant you here.`;
  return locked ? `${state} Click to unlock.` : `${state} Click to lock (members keep read).`;
}

// The lock glyphs, drawn in currentColor so the row's CSS colours them: a
// closed shackle while locked, an open one while unlocked.
const lockGlyph = (locked) => html`<svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true" focusable="false">
  <path d=${locked ? 'M5 7V5a3 3 0 0 1 6 0v2' : 'M5 7V5a3 3 0 0 1 5.8-1'} fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" />
  <rect x="3" y="7" width="10" height="7.5" rx="1.5" fill="currentColor" />
</svg>`;

// RailLock is the lock icon at the right end of a Collab panel resource row:
// always there, amber, while the resource is locked; shown on row hover or
// focus while unlocked. Clicking it toggles the lock (POST /api/resource-locks,
// the same route as the board tab menu and the Groups tab chip). On a peer's
// page this node cannot reach the lock: the icon shows the state but does
// nothing. snapshot is the dashboard's (read here, so a lock change that
// re-renders only this icon reads the current one). The state is resourceLockState's, so the tab name and the tab
// menu follow a toggle here.
export function RailLock({ kind, id, name, snapshot, notify = () => {}, fetchImpl }) {
  const [busy, setBusy] = useState(false);
  const locked = !!resourceLockState(snapshot?.value, kind, id);
  const denied = onPeerPage();
  const title = railLockTitle(locked, denied);
  const toggle = async (event) => {
    event.stopPropagation();
    event.preventDefault();
    if (busy || denied) return;
    setBusy(true);
    try {
      await setResourceLock(kind, id, !locked, fetchImpl);
      notify(locked ? `Agents can edit ${name} again` : `${name} is read-only for agents`);
    } catch (cause) {
      notify(`Could not change the lock on ${name}: ${cause.message}`, true);
    } finally {
      setBusy(false);
    }
  };
  return html`<button data-ui-exempt type="button" draggable="false"
    class=${`cr-lock${locked ? ' is-locked' : ''}${denied ? ' is-denied' : ''}`} data-rail-lock=${`${kind}:${id}`}
    aria-pressed=${locked ? 'true' : 'false'} aria-label=${`${name}: ${title}`} aria-disabled=${denied || busy ? 'true' : undefined}
    aria-busy=${busy ? 'true' : undefined} title=${title} onClick=${toggle} onDragStart=${(event) => event.preventDefault()}>${lockGlyph(locked)}</button>`;
}
