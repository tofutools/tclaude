// Queued spawns: spawn-dialog drafts the operator prepared but has not
// launched yet. They ride the snapshot (snapshot.queued_spawns) and surface in
// two places — the toolbar's ⏸ queued chip and a "Queued" block in each target
// group. Both render QueuedSpawnRow, so ▶ / edit / ✕ behave identically.
//
// A queued spawn is only a stored draft. Launching reopens the spawn dialog
// from it and submits through the ordinary path, so worktree creation, base
// fetch, attachment upload and every spawn-time check happen at launch.
import { h } from 'preact';
import htm from 'htm';
import { openAgentSpawnModal } from './agent-spawn-controller.js';
import { relTime } from './helpers.js';

const html = htm.bind(h);

let deps = {
  fetchImpl: (...args) => fetch(...args),
  confirm: async () => true,
  notify: () => {},
  refresh: () => {},
};

// configureQueuedSpawns wires the dashboard's confirm/toast/refresh in once at
// boot; the defaults keep isolated component tests self-contained.
export function configureQueuedSpawns(next = {}) {
  deps = { ...deps, ...next };
}

export function queuedSpawns(snapshot) {
  return Array.isArray(snapshot?.queued_spawns) ? snapshot.queued_spawns : [];
}

export function queuedSpawnLabel(entry) {
  const draft = entry?.draft || {};
  return String(draft.name || '').trim() || String(draft.descr || '').trim() || '(unnamed)';
}

// queuedSpawnDetail is the row's second line: target group, then whatever
// identifies the launch shape, then the start of the brief.
export function queuedSpawnDetail(entry, { group = true } = {}) {
  const draft = entry?.draft || {};
  const parts = [];
  if (group) parts.push(`→ ${entry?.group || draft.group || '?'}`);
  const roles = Array.isArray(draft.roleRefs) ? draft.roleRefs.filter(Boolean) : [];
  if (roles.length) parts.push(roles.join(', '));
  else if (draft.role) parts.push(String(draft.role));
  if (draft.profile) parts.push(String(draft.profile));
  else if (draft.harness) parts.push([draft.harness, draft.model].filter(Boolean).join(' '));
  const brief = String(draft.initialMessage || '').trim().replace(/\s+/g, ' ');
  if (brief) parts.push(`“${brief.length > 60 ? `${brief.slice(0, 59)}…` : brief}”`);
  return parts.join(' · ');
}

export function editQueuedSpawn(entry) {
  return openAgentSpawnModal({ queued: entry });
}

export function launchQueuedSpawn(entry) {
  return openAgentSpawnModal({ queued: entry, autoSpawn: true });
}

export async function discardQueuedSpawn(entry) {
  const label = queuedSpawnLabel(entry);
  const proceed = await deps.confirm({
    title: 'Discard this queued agent?',
    body: 'The prepared spawn is removed from the queue. Nothing was launched, so there is nothing else to clean up.',
    meta: `${label} → ${entry.group}`,
    okLabel: 'Discard',
  });
  if (!proceed) return false;
  try {
    const response = await deps.fetchImpl(`/api/queued-spawns/${encodeURIComponent(entry.id)}`, {
      method: 'DELETE', credentials: 'same-origin',
    });
    if (!response.ok && response.status !== 404) {
      throw new Error((await response.text().catch(() => '')) || `HTTP ${response.status}`);
    }
    deps.notify(`discarded queued ${label}`);
  } catch (cause) {
    deps.notify(`could not discard queued ${label}: ${cause?.message || cause}`, true);
    return false;
  }
  deps.refresh();
  return true;
}

export function QueuedSpawnRow({ entry, showGroup = true, Tag = 'div' }) {
  const label = queuedSpawnLabel(entry);
  const stop = (fn) => (event) => { event.stopPropagation(); void fn(entry); };
  return html`<${Tag} class="queued-spawn-row" data-queued-id=${entry.id}
    title=${`Queued${entry.created_at ? ` ${relTime(entry.created_at)}` : ''} — click to review or edit the full spawn dialog`}
    onClick=${() => editQueuedSpawn(entry)}>
    <button type="button" class="queued-spawn-play" data-act="queued-spawn-launch"
      title="Spawn now — opens the dialog and launches it; worktree, fetch and spawn checks run now"
      aria-label=${`Spawn ${label} now`} onClick=${stop(launchQueuedSpawn)}>▶</button>
    <span class="queued-spawn-meta">
      <span class="queued-spawn-name">${label}</span>
      <span class="queued-spawn-detail">${queuedSpawnDetail(entry, { group: showGroup })}</span>
    </span>
    <span class="queued-spawn-age">${entry.created_at ? relTime(entry.created_at) : ''}</span>
    <button type="button" class="queued-spawn-discard" data-act="queued-spawn-discard"
      title="Discard — remove from the queue without launching" aria-label=${`Discard queued ${label}`}
      onClick=${stop(discardQueuedSpawn)}>✕</button>
  </${Tag}>`;
}

// GroupQueuedBlock lists a group's queued spawns above its member table, the
// same slot the "Pending spawns" block uses.
export function GroupQueuedBlock({ group, snapshot }) {
  const rows = queuedSpawns(snapshot).filter((entry) => entry.group === group);
  if (!rows.length) return null;
  return html`<div class="group-queued-block">
    <div class="group-queued-title"><span class="theme-copy-regular">⏸ Queued</span><span class="theme-copy-wizard">⏸ Dormant summonings</span></div>
    ${rows.map((entry) => html`<${QueuedSpawnRow} key=${entry.id} entry=${entry} showGroup=${false} />`)}
  </div>`;
}
