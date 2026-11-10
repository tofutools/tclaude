// The Spawn queue block (tcl-ao1yz5): the node's capacity line and the spawns
// waiting for capacity, local and remote, as one first-in-first-out list. It
// rides the snapshot (snapshot.capacity_queue, the operator's view of
// `tclaude agent queue ls`), which the daemon omits while capacity limits are
// off and nothing waits, so the block is absent for everyone else.
//
// Not to be confused with queued spawns (queued-spawns.js): spawn-dialog
// drafts the operator chose not to launch yet. Those wait for the operator,
// these wait for capacity, so this block uses ⌛ where drafts use ⏸.
import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

let deps = {
  fetchImpl: (...args) => fetch(...args),
  confirm: async () => true,
  notify: () => {},
  refresh: () => {},
};

// configureSpawnQueue wires the dashboard's confirm/toast/refresh in once at
// boot; the defaults keep isolated component tests self-contained.
export function configureSpawnQueue(next = {}) {
  deps = { ...deps, ...next };
}

export function capacityQueue(snapshot) {
  const queue = snapshot?.capacity_queue;
  return queue && typeof queue === 'object' ? queue : null;
}

function waitingEntries(queue) {
  return Array.isArray(queue?.entries) ? queue.entries.filter((e) => e?.state === 'queued' || e?.state === 'launching') : [];
}

// groupCapacityQueued counts the spawns waiting for capacity into one group,
// for its header chip.
export function groupCapacityQueued(snapshot, groupName) {
  return waitingEntries(capacityQueue(snapshot)).filter((e) => e.group === groupName).length;
}

export function formatWait(seconds) {
  const s = Math.max(0, Math.floor(Number(seconds) || 0));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
  return `${Math.floor(s / 86400)}d ${Math.floor((s % 86400) / 3600)}h`;
}

// capacitySummary is the capacity line's state: full when the agent count is
// at its cap or spawns already wait (a new spawn would wait behind them).
export function capacitySummary(queue) {
  const cap = queue?.capacity || {};
  const max = Number(cap.max_live_agents) || 0;
  const used = Number.isFinite(cap.used_agents) ? cap.used_agents : null;
  const waiting = waitingEntries(queue).length;
  const full = waiting > 0 || (max > 0 && used !== null && used >= max);
  const ratio = max > 0 && used !== null ? Math.min(1, used / max) : 0;
  return { max, used, waiting, full, ratio };
}

function limitsText(cap) {
  const parts = [`when full: ${cap.when_full === 'queue' ? 'queue' : 'refuse'}`];
  if (cap.when_full === 'queue' && cap.max_queued > 0) parts.push(`max ${cap.max_queued}`);
  if (cap.min_free_ram_mb > 0) parts.push(`RAM ≥ ${cap.min_free_ram_mb} MB`);
  if (cap.min_free_disk_mb > 0) parts.push(`disk ≥ ${cap.min_free_disk_mb} MB`);
  if (cap.when_full === 'queue') parts.push('no ETA');
  return parts.join(' · ');
}

export async function cancelCapacityQueued(entry) {
  const label = entry?.name || '(unnamed)';
  const remote = entry?.requester_kind === 'federation';
  const proceed = await deps.confirm({
    title: remote ? 'Cancel this remote spawn request?' : 'Cancel this queued spawn?',
    body: remote
      ? 'It leaves the spawn queue and the request is denied; the requesting peer is told.'
      : 'It leaves the spawn queue and will not launch; the requester is told.',
    meta: `#${entry.position} ${label} → ${entry.group}`,
    okLabel: 'Cancel spawn',
  });
  if (!proceed) return false;
  try {
    const response = await deps.fetchImpl(`/api/spawn-queue/${encodeURIComponent(entry.id)}`, {
      method: 'DELETE', credentials: 'same-origin',
    });
    if (!response.ok && response.status !== 404) {
      throw new Error((await response.text().catch(() => '')) || `HTTP ${response.status}`);
    }
    deps.notify(`cancelled queued spawn ${label}`);
  } catch (cause) {
    deps.notify(`could not cancel queued spawn ${label}: ${cause?.message || cause}`, true);
    return false;
  }
  deps.refresh();
  return true;
}

function SpawnQueueRow({ entry }) {
  const launching = entry.state === 'launching';
  return html`<tr class="spawn-queue-row" data-spawn-queue-id=${entry.id}>
    <td class="spawn-queue-pos">${entry.position || ''}</td>
    <td><span class="rowname">${entry.name || '(unnamed)'}</span></td>
    <td>${entry.group}</td>
    <td class="muted">${entry.requested_by || entry.requester_kind}${entry.requester_kind === 'federation' ? html` <span class="ui-tag spawn-queue-remote">remote</span>` : null}</td>
    <td class="muted">${launching ? 'launching…' : formatWait(entry.age_seconds)}</td>
    <td><div class="spawn-queue-actions">
      <button type="button" class="ui-btn ui-btn-danger spawn-queue-cancel" data-act="spawn-queue-cancel" disabled=${launching}
        title=${launching ? 'Being launched now; it can no longer be cancelled' : 'Cancel — the spawn leaves the queue and does not launch'}
        aria-label=${`Cancel queued spawn ${entry.name || entry.id}`}
        onClick=${(event) => { event.stopPropagation(); cancelCapacityQueued(entry); }}>✕ cancel</button>
    </div></td>
  </tr>`;
}

// CapacityLine is the block's header: used/max, full or room, queue length,
// and the configured thresholds.
function CapacityLine({ queue }) {
  const cap = queue.capacity || {};
  const s = capacitySummary(queue);
  return html`<div class="spawn-queue-capacity">
    <strong>${s.waiting ? 'Spawn queue' : 'Capacity'}</strong>
    ${s.max > 0 ? html`<span class=${`spawn-queue-meter${s.full ? ' full' : ''}`} role="meter" aria-valuemin="0" aria-valuemax=${s.max} aria-valuenow=${s.used ?? 0}
      aria-label="Agents counted toward capacity"><i style=${{ width: `${Math.round(s.ratio * 100)}%` }}></i></span>
      <span>${s.used ?? '?'} / ${s.max} agents</span>` : null}
    <span class=${`ui-tag spawn-queue-chip ${s.full ? 'full' : 'room'}`}>${s.full ? 'full' : 'room'}</span>
    ${s.waiting ? html`<span class="ui-tag spawn-queue-chip waiting">⌛ ${s.waiting} waiting</span>` : null}
    <span class="muted spawn-queue-limits">${limitsText(cap)}</span>
  </div>`;
}

export function SpawnQueueBlock({ snapshot }) {
  const queue = capacityQueue(snapshot);
  if (!queue) return null;
  const rows = waitingEntries(queue);
  // A div: the dashboard's <section>s are its tabs, hidden unless active.
  return html`<div class=${`spawn-queue-block${rows.length ? '' : ' idle'}`} id="spawn-queue-block" role="region" aria-label="Spawn queue">
    <${CapacityLine} queue=${queue} />
    ${rows.length ? html`<table class="spawn-queue-table ui-compact"><thead><tr>
      <th>#</th><th>Name</th><th>Group</th><th>Requested by</th><th>Waiting</th><th></th>
    </tr></thead><tbody>${rows.map((entry) => html`<${SpawnQueueRow} key=${entry.id} entry=${entry} />`)}</tbody></table>` : null}
  </div>`;
}
