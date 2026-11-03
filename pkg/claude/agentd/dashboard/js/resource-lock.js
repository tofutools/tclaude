// resource-lock.js -- the operator's agent lock on a group resource
// (tcl-dwzsq0) as a chip that is also its toggle: '🔒 agents: read-only' or
// '✎ agents can edit'. It sits on each row of a group's Resources section
// and on the resource's own header (process editor, board, page). Only the
// operator's dashboard can toggle it (POST /api/resource-locks).

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

// lockLabel is the chip's text for a lock state.
export function lockLabel(locked) {
  return locked ? '🔒 agents: read-only' : '✎ agents can edit';
}

const lockTitle = (locked, name) => (locked
  ? `No agent can change ${name}, whatever it is granted; members still use it. Click to let agents edit it.`
  : `Agents edit ${name} under their usual permissions. Click to make it read-only for every agent.`);

// setResourceLock is the lock toggle for a surface without the process api.
export async function setResourceLock(kind, id, locked, fetchImpl = (...args) => globalThis.fetch(...args)) {
  const response = await fetchImpl('/api/resource-locks', {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ kind, id, locked }),
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.message || body.error || `${response.status}`);
  return body;
}

// LockChip shows and toggles one resource's agent lock. The group card passes
// its process api and context source; a resource's own header (board, page,
// process editor) passes onChanged instead, with the new state.
export function LockChip({ kind, id, name, locked, api = null, source = null, onChanged = null, notify = () => {} }) {
  const [busy, setBusy] = useState(false);
  const toggle = async (event) => {
    event.stopPropagation();
    if (busy) return;
    setBusy(true);
    try {
      await (api?.setResourceLock ? api.setResourceLock(kind, id, !locked) : setResourceLock(kind, id, !locked));
      notify(locked ? `Agents can edit ${name} again` : `${name} is read-only for agents`);
      onChanged?.(!locked);
      await source?.refresh?.();
    } catch (cause) {
      notify(`Could not change the lock on ${name}: ${cause.message}`, true);
    } finally {
      setBusy(false);
    }
  };
  // A chip that is also the toggle: a .ui-tag pill, warn-coloured while locked.
  return html`<button data-ui-exempt type="button" class=${`gr-lock ui-tag${locked ? ' ui-tag-warn' : ''}`} data-resource-lock=${`${kind}:${id}`}
    aria-pressed=${locked ? 'true' : 'false'} disabled=${busy} title=${lockTitle(locked, name)} onClick=${toggle}>${lockLabel(locked)}</button>`;
}

