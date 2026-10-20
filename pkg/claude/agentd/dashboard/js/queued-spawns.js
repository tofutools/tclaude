// Queued spawns: spawn-dialog drafts the operator prepared but has not
// launched yet. They ride the snapshot (snapshot.queued_spawns) and surface in
// two places — the toolbar's ⏸ queued chip (QueuedSpawnRow) and a row at the
// head of each target group's member table (groups-member-table.js's
// QueuedMemberRow). Both call the launch / edit / discard actions below.
//
// A queued spawn is a stored draft plus the files attached to it, which the
// daemon keeps with the queue entry. Launching reopens the spawn dialog from it
// and submits through the ordinary path, so worktree creation, base fetch and
// every spawn-time check happen at launch.
import { h } from 'preact';
import htm from 'htm';
import { openAgentSpawnModal } from './agent-spawn-controller.js';
import { relTime } from './helpers.js';
import { announceNodeChanged, isPageNode, nodeFetch, nodeSnapshot, selfNodeID } from './node-api.js';

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
  const files = Array.isArray(draft.attachments) ? draft.attachments.length : 0;
  if (files) parts.push(`📎 ${files}`);
  const brief = String(draft.initialMessage || '').trim().replace(/\s+/g, ' ');
  if (brief) parts.push(`“${brief.length > 60 ? `${brief.slice(0, 59)}…` : brief}”`);
  return parts.join(' · ');
}

// queuedNode is the node another node's queued spawn is on (the fused view
// names it, fleet_node); blank is the page's.
function queuedNode(entry) {
  const node = entry?.fleet_node || '';
  return isPageNode(node) ? '' : node;
}

// spawnOptions opens a queued spawn's dialog on its node, as the regular
// dialog does for a node trusting this one (or this node itself), under the
// node's own group name.
function spawnOptions(entry, extra = {}) {
  const node = queuedNode(entry);
  if (!entry?.fleet_node) return { queued: entry, ...extra };
  const own = { ...entry, group: entry.own_group || entry.group };
  if (!node) return { queued: own, ...extra };
  const self = node === selfNodeID();
  return { queued: own, ...extra, target: { node, nodeId: self ? '' : node, name: entry.fleet_node_name || '', snapshot: nodeSnapshot(node) } };
}

export function editQueuedSpawn(entry) {
  return openAgentSpawnModal(spawnOptions(entry));
}

export function launchQueuedSpawn(entry) {
  return openAgentSpawnModal(spawnOptions(entry, { autoSpawn: true }));
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
    const node = queuedNode(entry);
    const response = await (node ? nodeFetch(node, { fetchImpl: deps.fetchImpl }) : deps.fetchImpl)(`/api/queued-spawns/${encodeURIComponent(entry.id)}`, {
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
  announceNodeChanged(queuedNode(entry));
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

// groupQueuedSpawns is the queue for one group, oldest first as the snapshot
// lists them.
export function groupQueuedSpawns(snapshot, group) {
  return queuedSpawns(snapshot).filter((entry) => entry.group === group);
}
