// process-context.js -- the dashboard's read of GET /v1/process/context, the
// "processes in context" read model: run projections keyed by run id plus
// per-group and per-agent indexes into them. Groups (G1/G3) and Terminals
// (T1-T3) read it; the projections render through process-kit.js.
//
//   { runs: { <runId>: projection },
//     groups: [{ groupId, group, active: [runId], needsYou: [runId], done: [runId],
//                planned: [{ source, id, templateId, name, nextAt }], activeCount, needsYouCount }],
//     agents: [{ agentId, runs: [{ runId, nodeId, index, total, state }] }],
//     orphaned: [runId], truncated }
//
// Every field is optional on this side; a daemon without the read model
// answers 404 and the source reports `unavailable` rather than an error.

import { signal } from '@preact/signals';
import { idempotentRequestHeaders } from './request-idempotency.js';

export const PROCESS_CONTEXT_PATH = '/v1/process/context';

export function createProcessContextSource({ fetchImpl = (...args) => fetch(...args) } = {}) {
  const context = signal(null);
  // idle | ready | unavailable | error
  const status = signal('idle');
  const error = signal('');
  let inflight = null;

  function refresh() {
    if (inflight) return inflight;
    inflight = (async () => {
      try {
        const response = await fetchImpl(PROCESS_CONTEXT_PATH, { credentials: 'same-origin', cache: 'no-store' });
        if (response.status === 404) {
          context.value = null;
          status.value = 'unavailable';
          return;
        }
        const body = await response.json().catch(() => null);
        if (!response.ok || !body || typeof body !== 'object') {
          throw new Error(body?.message || body?.error || `${response.status} ${response.statusText || ''}`.trim());
        }
        context.value = body;
        status.value = 'ready';
        error.value = '';
      } catch (cause) {
        // Keep the last good context on screen; the error is shown beside it.
        status.value = context.value ? 'ready' : 'error';
        error.value = cause?.message || String(cause);
      } finally {
        inflight = null;
      }
    })();
    return inflight;
  }

  return { context, status, error, refresh };
}

export const processContext = createProcessContextSource();

// groupContextEntry finds a snapshot group's entry by name, then by id.
export function groupContextEntry(context, group) {
  const entries = context?.groups || [];
  return entries.find((entry) => entry.group && entry.group === group?.name)
    || entries.find((entry) => entry.groupId && (entry.groupId === group?.id || entry.groupId === group?.group_id))
    || null;
}

function resolve(context, ids) {
  const runs = context?.runs || {};
  const seen = new Set();
  const out = [];
  for (const id of ids || []) {
    if (seen.has(id) || !runs[id]) continue;
    seen.add(id);
    out.push({ ...runs[id], runId: runs[id].runId ?? id });
  }
  return out;
}

export const RUN_FILTERS = Object.freeze(['running', 'planned', 'done', 'all']);

export function normalizeRunFilter(value) {
  return RUN_FILTERS.includes(value) ? value : 'running';
}

// groupRunsFor returns the projections and planned entries one filter shows
// for a group: Running is needs-you first, then the rest of the active runs.
export function groupRunsFor(context, entry, filter) {
  const which = normalizeRunFilter(filter);
  const running = which === 'running' || which === 'all'
    ? resolve(context, [...(entry?.needsYou || []), ...(entry?.active || [])]) : [];
  const done = which === 'done' || which === 'all' ? resolve(context, entry?.done) : [];
  const planned = which === 'planned' || which === 'all' ? (entry?.planned || []) : [];
  return { runs: [...running, ...done], planned };
}

export function runNeedsYou(run, entry) {
  return run?.needsYou === true || (entry?.needsYou || []).includes(run?.runId);
}

// busyPerformers is the set of agent ids performing a live node in any of
// the given runs.
export function busyPerformers(runs) {
  const busy = new Set();
  for (const run of runs || []) {
    for (const node of run.nodes || []) {
      if (!['running', 'awaiting', 'blocked', 'ready'].includes(node.state)) continue;
      const ref = node.performer?.ref;
      if (node.performer?.kind === 'agent' && ref && ref !== 'unassigned') busy.add(ref);
    }
  }
  return busy;
}

// withPerformerNames labels performer refs with the group's member titles so
// avatars show a real initial and name.
export function withPerformerNames(run, nameOf) {
  return {
    ...run,
    nodes: (run.nodes || []).map((node) => {
      const ref = node.performer?.ref;
      if (!ref || node.performer.label || node.performer.name) {
        return node.performer?.name ? { ...node, performer: { ...node.performer, label: node.performer.name } } : node;
      }
      const name = nameOf(ref);
      return name ? { ...node, performer: { ...node.performer, label: name } } : node;
    }),
  };
}

export function formatRunDuration(fromISO, now = Date.now()) {
  const from = Date.parse(fromISO || '');
  if (!Number.isFinite(from)) return '';
  const minutes = Math.max(0, Math.floor((now - from) / 60000));
  if (minutes < 1) return '<1m';
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ${String(minutes % 60).padStart(2, '0')}m`;
  return `${Math.floor(hours / 24)}d ${hours % 24}h`;
}

function newAttemptKey() {
  if (typeof crypto?.randomUUID === 'function') return crypto.randomUUID();
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

async function processRequest(path, init, fetchImpl) {
  const response = await fetchImpl(path, { credentials: 'same-origin', cache: 'no-store', ...init });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText || ''}`.trim());
  return body;
}

// processRunApi is the start-a-run seam the Groups Processes lens uses; tests
// and dashsnap swap the fetch underneath.
export function createProcessRunApi({ fetchImpl = (...args) => fetch(...args) } = {}) {
  return {
    async listTemplates() {
      const body = await processRequest('/v1/process/templates', {}, fetchImpl);
      return (body.templates || []).map((t) => ({ id: t.id, name: t.name || t.id }));
    },
    // A group-scoped start: POST /v1/process/runs with the group by name
    // (process.runs.start covering the group; the human always passes).
    // `attemptKey` makes a retried submit of the same attempt replay rather
    // than start a second run.
    async startGroupRun({ templateId, group, attemptKey = newAttemptKey() }) {
      const path = '/v1/process/runs';
      const body = JSON.stringify({ templateId, group, authorizeProgramProfiles: [] });
      return processRequest(path, {
        method: 'POST', headers: idempotentRequestHeaders('POST', path, body, attemptKey), body,
      }, fetchImpl);
    },
  };
}

export const processRunApi = createProcessRunApi();
