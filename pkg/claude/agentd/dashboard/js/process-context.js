// process-context.js -- the dashboard's read of GET /v1/process/context, the
// "processes in context" read model: run projections keyed by run id plus
// per-group and per-agent indexes into them. Groups (G1/G3) and Terminals
// (T1-T3) read it; the projections render through process-kit.js.
//
//   { runs: { <runId>: projection },
//     groups: [{ groupId, group, active: [runId], needsYou: [runId], done: [runId],
//                planned: [{ source, id, templateId, name, nextAt }], activeCount, needsYouCount,
//                processes: [{ templateId, name, ref, listed, steps, params, programProfiles }] }],
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

// activeRunCount counts distinct live runs across every group's index.
export function activeRunCount(context) {
  const ids = new Set();
  for (const entry of context?.groups || []) for (const id of entry.active || []) ids.add(id);
  return ids.size;
}

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

// The run filter is a checklist over RUN_KINDS, held as a canonical
// comma-joined string ("running,planned") so a signal compares it by value.
// 'all' (the old single-select value) reads as every kind; an empty or
// unknown value falls back to DEFAULT_RUN_FILTER, so the list never shows
// nothing at all.
export const RUN_KINDS = Object.freeze(['running', 'planned', 'done']);
export const DEFAULT_RUN_FILTER = 'running,planned';
// [regular, wizard] label per kind.
export const RUN_KIND_LABELS = Object.freeze({
  running: ['Running', 'Unfolding'],
  planned: ['Planned', 'Foretold'],
  done: ['Done', 'Fulfilled'],
});

export function normalizeRunFilter(value) {
  if (value === 'all') return RUN_KINDS.join(',');
  const parts = Array.isArray(value) ? value : String(value ?? '').split(',');
  const kinds = RUN_KINDS.filter((kind) => parts.includes(kind));
  return kinds.length ? kinds.join(',') : DEFAULT_RUN_FILTER;
}

export function runFilterKinds(filter) {
  return normalizeRunFilter(filter).split(',');
}

export function runFilterHas(filter, kind) {
  return runFilterKinds(filter).includes(kind);
}

// toggleRunKind flips one kind; unticking the last ticked kind is a no-op.
export function toggleRunKind(filter, kind) {
  const kinds = runFilterKinds(filter);
  if (!kinds.includes(kind)) return normalizeRunFilter([...kinds, kind]);
  return kinds.length > 1 ? normalizeRunFilter(kinds.filter((k) => k !== kind)) : normalizeRunFilter(kinds);
}

export function withRunKind(filter, kind) {
  return normalizeRunFilter([...runFilterKinds(filter), kind]);
}

// runKindCounts totals distinct runs per kind across every group's index,
// for the run filter's checklist.
export function runKindCounts(context) {
  const running = new Set();
  const done = new Set();
  const planned = new Set();
  for (const entry of context?.groups || []) {
    for (const id of [...(entry.active || []), ...(entry.needsYou || [])]) running.add(id);
    for (const id of entry.done || []) done.add(id);
    for (const item of entry.planned || []) planned.add(`${item.source || ''}:${item.id || item.name || ''}`);
  }
  return { running: running.size, planned: planned.size, done: done.size };
}

// groupRunsFor returns the projections and planned entries the filter shows
// for a group: running is needs-you first, then the rest of the active runs.
export function groupRunsFor(context, entry, filter) {
  const running = runFilterHas(filter, 'running')
    ? resolve(context, [...(entry?.needsYou || []), ...(entry?.active || [])]) : [];
  const done = runFilterHas(filter, 'done') ? resolve(context, entry?.done) : [];
  const planned = runFilterHas(filter, 'planned') ? (entry?.planned || []) : [];
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

// saveMeta is a template save's library and group metadata: sent only when
// set.
function saveMeta(listed, pinGroup) {
  return { ...(listed === undefined ? {} : { listed }), ...(pinGroup ? { pinGroup } : {}) };
}

async function processRequest(path, init, fetchImpl) {
  const response = await fetchImpl(path, { credentials: 'same-origin', cache: 'no-store', ...init });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    // The body rides along: a template save whose listing or pin failed
    // names the version it did store ({id, ref}).
    const error = new Error(body.message || body.error || `${response.status} ${response.statusText || ''}`.trim());
    error.body = body;
    throw error;
  }
  return body;
}

// templateStartRequirements lists a template's params (required first) and
// the distinct program profiles its program performers use, walking stages
// and inline subprocess bodies. An unnamed profile is the empty string, as
// the server names it.
export function templateStartRequirements(template = {}) {
  const params = Object.entries(template.params || {}).map(([name, spec]) => ({
    name,
    type: String(spec?.type || 'string'),
    description: String(spec?.description || ''),
    required: spec?.required === true || (spec?.required === undefined && spec?.default === undefined),
    default: spec?.default === undefined || spec?.default === null ? '' : String(spec.default),
    hasDefault: spec?.default !== undefined && spec?.default !== null,
  })).sort((a, b) => (a.required === b.required ? a.name.localeCompare(b.name, 'en') : a.required ? -1 : 1));
  const profiles = new Set();
  const walk = (value) => {
    if (!value || typeof value !== 'object') return;
    if (Array.isArray(value)) { value.forEach(walk); return; }
    if (value.kind === 'program' && (typeof value.run === 'string' || Array.isArray(value.run) || 'profile' in value)) {
      profiles.add(String(value.profile || ''));
    }
    for (const child of Object.values(value)) walk(child);
  };
  walk(template.nodes);
  return { params, programProfiles: [...profiles].sort() };
}

// unauthorizedProfiles reads the profiles a 403 process_program_unauthorized
// refusal names ("...authorization: a, b"; "<empty>" is the unnamed one).
export function unauthorizedProfiles(message = '') {
  const at = String(message).indexOf('require explicit authorization:');
  if (at < 0) return [];
  return String(message).slice(at + 'require explicit authorization:'.length).split(',')
    .map((profile) => profile.trim()).filter(Boolean).map((profile) => (profile === '<empty>' ? '' : profile));
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
    // templateRequirements reads what starting a template asks for: its
    // params and the program profiles its programs run under (inline bodies
    // included; a referenced template's are named by the server's refusal).
    async templateRequirements(templateId) {
      const body = await processRequest(`/v1/process/templates/${encodeURIComponent(templateId)}`, {}, fetchImpl);
      return templateStartRequirements(body.template || {});
    },
    // createTemplate stores a new template from an edit view and returns the
    // created {id, ref, ...}; the store mints the id. `listed` and
    // `pinGroup` are sent only when given, so a daemon without library
    // listing still accepts a plain create. Both are request metadata, never
    // template content.
    async createTemplate({ template, edges, layout }, { listed, pinGroup = '', attemptKey = newAttemptKey() } = {}) {
      const path = '/v1/process/templates';
      const { id: _id, ...rest } = template;
      const body = JSON.stringify({ template: rest, edges, layout, ...saveMeta(listed, pinGroup) });
      return processRequest(path, {
        method: 'POST', headers: idempotentRequestHeaders('POST', path, body, attemptKey), body,
      }, fetchImpl);
    },
    // getTemplate reads a stored template's edit view ({template, edges,
    // layout, sourceHash, listed, ...}).
    getTemplate(templateId) {
      return processRequest(`/v1/process/templates/${encodeURIComponent(templateId)}`, {}, fetchImpl);
    },
    // saveTemplate saves a new version of a stored template against the
    // sourceHash it was opened at; a 409 means someone saved in between.
    async saveTemplate(templateId, { template, edges, layout }, { sourceHash = '', listed, pinGroup = '', attemptKey = newAttemptKey() } = {}) {
      const path = `/v1/process/templates/${encodeURIComponent(templateId)}`;
      const body = JSON.stringify({ template: { ...template, id: templateId }, edges, layout, sourceHash, ...saveMeta(listed, pinGroup) });
      return processRequest(path, {
        method: 'POST', headers: idempotentRequestHeaders('POST', path, body, attemptKey), body,
      }, fetchImpl);
    },
    // publishTemplate lists a template in the library.
    async publishTemplate(templateId, attemptKey = newAttemptKey()) {
      const path = `/v1/process/templates/${encodeURIComponent(templateId)}/listing`;
      const body = JSON.stringify({ listed: true });
      return processRequest(path, {
        method: 'POST', headers: idempotentRequestHeaders('POST', path, body, attemptKey), body,
      }, fetchImpl);
    },
    // pinProcess saves an existing template to a group's processes;
    // unpinProcess removes it from them (the template itself stays).
    async pinProcess(group, templateId, attemptKey = newAttemptKey()) {
      const path = `/v1/process/groups/${encodeURIComponent(group)}/processes`;
      const body = JSON.stringify({ templateId });
      return processRequest(path, {
        method: 'POST', headers: idempotentRequestHeaders('POST', path, body, attemptKey), body,
      }, fetchImpl);
    },
    async unpinProcess(group, templateId) {
      return processRequest(`/v1/process/groups/${encodeURIComponent(group)}/processes/${encodeURIComponent(templateId)}`, {
        method: 'DELETE',
      }, fetchImpl);
    },
    // validateTemplate reads the server's diagnostics for an unsaved view.
    async validateTemplate({ template, edges, layout }) {
      const body = await processRequest('/v1/process/validate', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ template: { ...template, id: template.id || 'draft' }, edges, layout }),
      }, fetchImpl);
      return body.diagnostics || [];
    },
    // startGroupRun starts a run scoped to a group, or to one agent when
    // agent is given instead.
    async startGroupRun({ templateId, group, agent, params = {}, authorizeProgramProfiles = [], attemptKey = newAttemptKey() }) {
      const path = '/v1/process/runs';
      const filled = Object.fromEntries(Object.entries(params).filter(([, value]) => String(value ?? '') !== ''));
      const body = JSON.stringify({
        templateId, ...(agent ? { agent } : { group }), ...(Object.keys(filled).length ? { params: filled } : {}),
        authorizeProgramProfiles: [...authorizeProgramProfiles],
      });
      return processRequest(path, {
        method: 'POST', headers: idempotentRequestHeaders('POST', path, body, attemptKey), body,
      }, fetchImpl);
    },
  };
}

export const processRunApi = createProcessRunApi();

// RUN_EVENT_PAGES caps how many 16-event pages one events read walks, so a
// long run's first open costs a bounded number of requests; later polls
// continue from the last sequence seen.
export const RUN_EVENT_PAGES = 8;

// createRunDetailApi is the single-run focus's seam (G5): the run's durable
// view (awaited decisions with their verdicts, blocked branches), its evidence
// events, and the manual decision a human gate waits for.
export function createRunDetailApi({ fetchImpl = (...args) => fetch(...args) } = {}) {
  const runPath = (runId) => `/v1/process/runs/${encodeURIComponent(runId)}`;
  const postEmpty = (path, attemptKey) => processRequest(path, {
    method: 'POST', headers: idempotentRequestHeaders('POST', path, '{}', attemptKey), body: '{}',
  }, fetchImpl);
  return {
    getRun: (runId) => processRequest(runPath(runId), {}, fetchImpl),
    // listEvents reads the events after `after`, oldest first; `more` says
    // the page cap stopped the walk early.
    async listEvents(runId, after = 0) {
      const events = [];
      let cursor = after;
      for (let page = 0; page < RUN_EVENT_PAGES; page++) {
        const body = await processRequest(`${runPath(runId)}/events${cursor ? `?after=${cursor}` : ''}`, {}, fetchImpl);
        events.push(...(body.events || []));
        if (!body.next) return { events, more: false };
        cursor = body.next;
      }
      return { events, more: true };
    },
    // decide records a human gate's verdict against the awaited window
    // (attempt) the inspector read, so a stale click is refused, not replayed.
    // Without an attempt the daemon takes the decision's current window.
    async decide(runId, { nodeId, verdict, evidence = '', attempt, attemptKey = newAttemptKey() }) {
      const path = `${runPath(runId)}/decide`;
      const request = { nodeId, verdict };
      if (evidence) request.evidence = evidence;
      if (Number.isInteger(attempt)) request.attempt = attempt;
      const body = JSON.stringify(request);
      return processRequest(path, {
        method: 'POST', headers: idempotentRequestHeaders('POST', path, body, attemptKey), body,
      }, fetchImpl);
    },
    // assign hands a node's outstanding agent attempt to another performer.
    // nodeId is the executable id the attempt runs under (dispatch.nodeId).
    async assign(runId, { nodeId, agent, attemptKey = newAttemptKey() }) {
      const path = `${runPath(runId)}/nodes/${encodeURIComponent(nodeId)}/assign`;
      const body = JSON.stringify({ agent });
      return processRequest(path, {
        method: 'POST', headers: idempotentRequestHeaders('POST', path, body, attemptKey), body,
      }, fetchImpl);
    },
    // pause holds the run: work in flight finishes, nothing new is
    // dispatched until resume releases it. Both are idempotent server-side.
    pause: (runId, { attemptKey = newAttemptKey() } = {}) => postEmpty(`${runPath(runId)}/pause`, attemptKey),
    // listLiveRuns walks every page of the run list for each filter ({group}
    // or {agent}) and returns the running runs once each. The context read
    // model caps how many runs a group or agent lists; acting on all of them
    // needs the full list.
    async listLiveRuns(filters) {
      const found = new Map();
      for (const filter of filters) {
        let after = '';
        do {
          const query = new URLSearchParams({ ...filter, limit: '32', ...(after ? { after } : {}) });
          const body = await processRequest(`/v1/process/runs?${query}`, {}, fetchImpl);
          for (const run of body.runs || []) {
            if (run.status === 'running' && !found.has(run.id)) found.set(run.id, { ...run, runId: run.id });
          }
          after = body.next || '';
        } while (after);
      }
      return [...found.values()];
    },
    resume: (runId, { attemptKey = newAttemptKey() } = {}) => postEmpty(`${runPath(runId)}/resume`, attemptKey),
  };
}

export const runDetailApi = createRunDetailApi();
