// resource-lock.js -- the agent lock on a group resource (tcl-dwzsq0) as a
// one-click toggle button: a lock icon and 'Locked' or 'Unlocked'. It sits on
// each row of a group's Resources section, in the Collab ＋ list, and on the
// resource's own header (process editor, board, page). The operator's
// dashboard toggles it (POST /api/resource-locks); an agent does through its
// own route under groups.resources.lock.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { onPeerPage } from './node-api.js';

const html = htm.bind(h);

// lockLabel is the chip's text for a lock state.
export function lockLabel(locked) {
  return locked ? 'Locked' : 'Unlocked';
}

// lockTitle is the button's tooltip: the state, what it means, the action.
export const lockTitle = (locked, name) => (locked
  ? `Locked: members can read ${name} but not edit it. Click to unlock`
  : `Unlocked: members can read and edit ${name}. Click to lock`);

// LockIcon is a closed or open padlock drawn in the current text colour.
export function LockIcon({ locked }) {
  return html`<svg class="gr-lock-icon" viewBox="0 0 24 24" width="13" height="13" aria-hidden="true"
    fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
    <rect x="5" y="11" width="14" height="10" rx="2" />
    <path d=${locked ? 'M8 11V7a4 4 0 0 1 8 0v4' : 'M8 11V7a4 4 0 0 1 7.5-2'} /></svg>`;
}

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
  // A bordered toggle button, amber while locked.
  return html`<button type="button" class=${`ui-btn ui-sm gr-lock${locked ? ' gr-locked' : ''}`} data-resource-lock=${`${kind}:${id}`}
    aria-pressed=${locked ? 'true' : 'false'} aria-label=${title} aria-disabled=${denied ? 'true' : undefined} aria-busy=${busy ? 'true' : undefined}
    disabled=${busy || denied} title=${title} onClick=${toggle}><${LockIcon} locked=${locked} /><span>${lockLabel(locked)}</span></button>`;
}

