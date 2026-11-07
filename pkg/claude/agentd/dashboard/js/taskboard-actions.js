// taskboard-actions.js -- the Task board section's writes (docs/taskboard.md).
// Each is the route its CLI verb uses, so every dashboard action has a CLI
// form:
//
//   capture / paste   add <group> "title" [--next]  · add --lines-file -
//   + from awb/…      add <group> --from-awb / --from-linear / --from-github
//   edit              edit <task> --title … --ref …|--clear-ref
//   drag reorder      move <task> --before|--after|--top|--bottom
//   ✓ Next / demote   promote <task> · demote <task>
//   block / unblock   block <task> --reason … · unblock <task>
//   done / drop       done <task> · drop <task> [--reason …]
//   reopen            (route only: status open on a done or dropped task)
//
// Every write carries an idempotency key, so a retried click never applies
// twice.

import { idempotentRequestHeaders } from './request-idempotency.js';
import { randomUUID } from './insecure-origin.js';

export class TaskboardRequestError extends Error {
  constructor(message, { status = 0, code = '' } = {}) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

async function request(fetchImpl, method, path, payload) {
  const body = payload === undefined ? undefined : JSON.stringify(payload);
  const init = { method, credentials: 'same-origin', cache: 'no-store' };
  if (body !== undefined) {
    init.body = body;
    init.headers = idempotentRequestHeaders(method, path, body, randomUUID());
  }
  const response = await fetchImpl(path, init);
  const out = await response.json().catch(() => null);
  if (!response.ok) {
    throw new TaskboardRequestError(out?.message || out?.error || `${response.status} ${response.statusText || ''}`.trim(),
      { status: response.status, code: out?.error || out?.code || '' });
  }
  return out;
}

const taskPath = (id, suffix = '') => `/v1/taskboard/tasks/${encodeURIComponent(id)}${suffix}`;
const boardPath = (group) => `/v1/groups/${encodeURIComponent(group)}/taskboard/tasks`;

// TRACKER_ID is a line that is only a tracker id or link: such a line is an
// import (its title comes from the tracker), any other line is a title. A
// lower-case awb id needs a digit, so a one-word title such as "fix-flake"
// typed alone stays a title.
const TRACKER_ID = /^(?:(?:awb|linear):\S+|[a-z][a-z0-9]*-(?=[a-z0-9]*\d)[a-z0-9]{3,}|[A-Z][A-Z0-9]*-\d+|[\w.-]+\/[\w.-]+#\d+|https?:\/\/\S+)$/;

export function isTrackerLine(line) {
  return TRACKER_ID.test(String(line || '').trim());
}

// captureLines splits a capture into its non-empty lines.
export function captureLines(text) {
  return String(text || '').split(/\r?\n/).map((line) => line.trim()).filter(Boolean);
}

// IMPORT_PREFIX turns the import dialog's tracker choice into the refs'
// prefix, as the CLI's --from-awb / --from-linear do.
const IMPORT_PREFIX = { awb: 'awb:', linear: 'linear:', github: '' };

export function importRefs(kind, text) {
  const prefix = IMPORT_PREFIX[kind] ?? '';
  return String(text || '').split(/[\s,]+/).map((id) => id.trim()).filter(Boolean)
    .map((id) => (prefix && !id.includes(':') ? `${prefix}${id}` : id));
}

// IMPORT_BATCH is the most items one import request takes.
export const IMPORT_BATCH = 50;

export function createTaskboardApi({ fetchImpl = (...args) => fetch(...args) } = {}) {
  const call = (method, path, payload) => request(fetchImpl, method, path, payload);
  // importBatches sends a long import in batches of IMPORT_BATCH, one after
  // another, and merges the replies; the results stay in input order.
  const importBatches = async (group, key, items, stage) => {
    const merged = { created: 0, results: [] };
    for (let i = 0; i < items.length; i += IMPORT_BATCH) {
      const out = await call('POST', boardPath(group), { [key]: items.slice(i, i + IMPORT_BATCH), stage });
      merged.created += out?.created || 0;
      merged.results.push(...(out?.results || []));
    }
    return merged;
  };
  return {
    // capture adds the lines of a capture box: one plain line is one task;
    // several lines, or a line that is only a tracker id, go through the
    // import (`add --lines-file`), which resolves ids to titles. Returns
    // { created, results } either way.
    async capture(group, text, { next = false } = {}) {
      const lines = captureLines(text);
      if (!lines.length) return { created: 0, results: [] };
      const stage = next ? 'next' : undefined;
      if (lines.length === 1 && !isTrackerLine(lines[0])) {
        const task = await call('POST', boardPath(group), { title: lines[0], stage });
        return { created: 1, results: [{ input: lines[0], task }] };
      }
      return importBatches(group, 'lines', lines, stage);
    },
    // importRefs adds one task per tracker ref (`add --from-awb` and kin).
    importRefs(group, refs, { next = false } = {}) {
      return importBatches(group, 'refs', refs, next ? 'next' : undefined);
    },
    edit(id, { title, ref, clearRef = false }) {
      const body = {};
      if (title !== undefined) body.title = title;
      if (clearRef) body.clear_ref = true;
      else if (ref !== undefined) body.ref = ref;
      return call('PATCH', taskPath(id), body);
    },
    // move places a task before or after another (crossing the Ideas band
    // promotes or demotes it in the same write), or at the top or bottom.
    move(id, position) {
      return call('POST', taskPath(id, '/move'), position);
    },
    stage(id, stage) {
      return call('POST', taskPath(id, '/stage'), { stage });
    },
    status(id, status, reason = '') {
      return call('POST', taskPath(id, '/status'), reason ? { status, reason } : { status });
    },
  };
}

export const taskboardApi = createTaskboardApi();
