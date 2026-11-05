// resource-lock.js -- the agent lock on a group resource (tcl-dwzsq0) as a
// chip that is also its toggle: '🔒 agents: read-only' or '✎ agents can
// edit'. It sits on each row of a group's Resources section and on the
// resource's own header (process editor, board, page). In the Collab view a
// locked resource also wears a small LockBadge on its tab name and side panel
// row. The operator's dashboard toggles it (POST /api/resource-locks); an
// agent does through its own route under groups.resources.lock.

import { h } from 'preact';
import { useState } from 'preact/hooks';
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

// knownLocks remembers the last lock state a surface read, by "kind:id", so a
// surface with no fetch of its own (a board tab's menu) can name the state.
const knownLocks = new Map();

export function rememberLock(kind, id, locked) {
  knownLocks.set(`${kind}:${id}`, !!locked);
}

// knownLock is the remembered state, or undefined when none was read.
export function knownLock(kind, id) {
  return knownLocks.get(`${kind}:${id}`);
}

const kindNoun = { board: 'board', page: 'page', process: 'process' };

// lockBadgeTitle is the small lock icon's tooltip.
export const lockBadgeTitle = (kind) => `Locked: members can read this ${kindNoun[kind] || 'resource'} but not edit it. `
  + 'You, or an agent you granted groups.resources.lock, can unlock it.';

// resourceLocked reports whether a resource is locked against agents, from
// the snapshot's group resources, else from what a surface last read
// (knownLock): a board linked to no group is not in the snapshot.
export function resourceLocked(snapshotValue, kind, id) {
  if (!kind || !id) return false;
  const row = (snapshotValue?.group_resources || []).find((r) => r.kind === kind && r.id === id);
  if (row) return !!row.locked;
  return !!knownLock(kind, id);
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
  rememberLock(kind, id, locked);
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

