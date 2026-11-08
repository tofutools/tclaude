// taskboard-actions.js -- the Task board section's writes (docs/taskboard.md).
// Each is the route its CLI verb uses, so every dashboard action has a CLI
// form:
//
//   capture / paste   add <group> "title" [--next]  · add --lines-file -
//   + New task ›      add <group> "title" --body … --ref … --resolve-ref
//   Write it myself   [--next] --after tk_…  (then assign --to, if picked)
//   + New task ›      ask <group> --to <agent> "request"
//   Ask an agent
//   + from awb/…      add <group> --from-awb / --from-linear / --from-github
//   edit              edit <task> --title … --ref …|--clear-ref
//   drag reorder      move <task> --before|--after|--top|--bottom
//   ✓ Next / demote   promote <task> · demote <task>
//   block / unblock   block <task> --reason … · unblock <task>
//   done / drop       done <task> · drop <task> [--reason …]
//   reopen            (route only: status open on a done or dropped task)
//   Assign ▾ / drag   assign <task> --to <agent>
//   onto an agent
//   Unassign / drag   unassign <task>
//   onto "unassign"
//   drag onto another move-to-group <task> <group> [--clone] [--stage next]
//   group (Ctrl/Cmd
//   clones)
//   … on another     transfer-in on that node, then moved-away here
//   node             (move-to-group <task> <group>@<peer>)
//   files            add --attach … · ask --attach … · attach <task> … ·
//                    detach <task> <name|#id>
//
// Every write carries an idempotency key, so a retried click never applies
// twice.

import { idempotentRequestHeaders } from './request-idempotency.js';
import { randomUUID } from './insecure-origin.js';
import { nodeUpload } from './node-api.js';

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
  return answer(await fetchImpl(path, init));
}

// answer reads a route's JSON reply, throwing its refusal.
async function answer(response) {
  const out = await response.json().catch(() => null);
  if (!response.ok) {
    throw new TaskboardRequestError(out?.message || out?.error || `${response.status} ${response.statusText || ''}`.trim(),
      { status: response.status, code: out?.error || out?.code || '' });
  }
  return out;
}

// taskForm is a request with files, as the CLI's --attach sends it: a "task"
// part with the JSON body (none for a bare attach) and one "file" part per
// file.
export function taskForm(task, files, part = 'task') {
  const form = new FormData();
  if (task) form.append(part, new Blob([JSON.stringify(task)], { type: 'application/json' }));
  for (const file of files) form.append('file', file.file || file, file.name || file.file?.name || 'file');
  return form;
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

// TASK_FILE_LIMITS are a task's file limits (docs/taskboard.md "Files on a
// task"): 10 files of 4 MiB each, of the types mail attachments may have.
export const TASK_FILE_MAX = 10;
export const TASK_FILE_MAX_BYTES = 4 << 20;
const TASK_FILE_TYPES = new Set(['.png', '.jpg', '.jpeg', '.gif', '.webp', '.txt', '.log', '.md', '.csv', '.json',
  '.yaml', '.yml', '.diff', '.patch', '.pdf']);

// taskFileRefusal is why the server would refuse a file's name, or ''.
export function taskFileRefusal(_file, name) {
  const dot = String(name || '').lastIndexOf('.');
  const ext = dot > 0 ? String(name).slice(dot).toLowerCase() : '';
  return TASK_FILE_TYPES.has(ext) ? '' : 'type not allowed: images, text, markdown, csv, json, yaml, diff/patch or pdf';
}

// taskFileLimits are the intake's limits (attachment-intake.js) for a task
// that already holds `held` files.
export function taskFileLimits(held = 0) {
  return { maxFiles: Math.max(0, TASK_FILE_MAX - held), maxBytes: TASK_FILE_MAX_BYTES, accept: taskFileRefusal };
}

// createTaskboardApi is the board's write client. upload posts a request
// with files: to this node directly, and to another node (the page's own on
// a peer's page) through this node's staged upload (node-api.js nodeUpload),
// since a peer view request carries 32 KiB. A node's board passes its own.
export function createTaskboardApi({ fetchImpl = (...args) => fetch(...args), upload = null } = {}) {
  const call = (method, path, payload) => request(fetchImpl, method, path, payload);
  const send = upload || ((path, init) => nodeUpload('', path, init, { fetchImpl }));
  const post = async (path, task, files) => answer(await send(path, { body: taskForm(task, files) }));
  // importBatches sends a long import in batches of IMPORT_BATCH, one after
  // another, and merges the replies; the results stay in input order. When a
  // later batch fails, the batches already added stand: the merged result
  // carries `failed` with the error and the items not sent, so the caller
  // never sends the added ones again. A failing first batch throws.
  const importBatches = async (group, key, items, stage) => {
    const merged = { created: 0, results: [] };
    for (let i = 0; i < items.length; i += IMPORT_BATCH) {
      let out;
      try {
        out = await call('POST', boardPath(group), { [key]: items.slice(i, i + IMPORT_BATCH), stage });
      } catch (cause) {
        if (i === 0) throw cause;
        merged.failed = { message: cause?.message || String(cause), remaining: items.slice(i) };
        return merged;
      }
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
    // create adds one task from the New task form (`add` with its flags);
    // resolveRef links a bare awb or Linear id through its tracker
    // (`--resolve-ref`). Fields left empty are not sent.
    // create adds one task in its start state, in one call: status open
    // (the default), blocked (blockReason) or done, and `to` its assignee
    // (in progress, or who did it).
    create(group, { title, body = '', ref = '', stage = 'idea', after = [], resolveRef = false, status = '', blockReason = '', to = '', files = [] }) {
      const payload = { title };
      if (body) payload.body = body;
      if (ref) payload.ref = ref;
      if (ref && resolveRef) payload.resolve_ref = true;
      if (stage === 'next') payload.stage = 'next';
      if (after.length) payload.after = after;
      if (status && status !== 'open') payload.status = status;
      if (status === 'blocked') payload.block_reason = blockReason;
      if (to) payload.to = to;
      if (files.length) return post(boardPath(group), payload, files);
      return call('POST', boardPath(group), payload);
    },
    // ask mails a member a prose request to turn into tasks on the board;
    // files go with the mail.
    ask(group, to, prompt, files = []) {
      const path = `/v1/groups/${encodeURIComponent(group)}/taskboard/ask`;
      if (files.length) return post(path, { to, prompt }, files);
      return call('POST', path, { to, prompt });
    },
    // task reads one task in full (`show`): the board's context leaves the
    // brief out.
    task(id) {
      return call('GET', taskPath(id));
    },
    // activity is the task's stream, oldest first (`activity`): changes and
    // comments, each comment in its current state.
    activity(id) {
      return call('GET', taskPath(id, '/activity'));
    },
    // comment adds a comment (`comment add`), with its files as multipart.
    // key makes a retry safe: the same key and body return the first
    // comment, so a Send whose answer was lost is never a second comment.
    async comment(id, body, key, files = []) {
      const path = taskPath(id, '/comments');
      if (files.length) return answer(await send(path, { body: taskForm({ body, key }, files, 'comment') }));
      return call('POST', path, { body, key });
    },
    // editComment and deleteComment are the author's own (`comment edit` /
    // `comment rm`); a delete leaves a tombstone.
    editComment(id, commentId, body) {
      return call('PATCH', taskPath(id, `/comments/${encodeURIComponent(commentId)}`), { body });
    },
    deleteComment(id, commentId) {
      return call('DELETE', taskPath(id, `/comments/${encodeURIComponent(commentId)}`));
    },
    // attach adds files to a task (`attach`); detach removes one by id.
    attach(id, files) {
      return post(taskPath(id, '/attachments'), null, files);
    },
    detach(id, attachmentId) {
      return call('DELETE', taskPath(id, `/attachments/${encodeURIComponent(attachmentId)}`));
    },
    // fileData reads one file as { name, content_type, data (base64) }: the
    // form a peer view carries, so it works for any node's board.
    fileData(id, attachmentId) {
      return call('GET', taskPath(id, `/attachments/${encodeURIComponent(attachmentId)}?format=json`));
    },
    // resolveRef links a bare awb or Linear id through its tracker, as
    // create's does (`edit --resolve-ref`).
    edit(id, { title, body, ref, clearRef = false, resolveRef = false, addDeps, rmDeps }) {
      const payload = {};
      if (title !== undefined) payload.title = title;
      if (body !== undefined) payload.body = body;
      if (addDeps !== undefined) payload.add_deps = addDeps;
      if (rmDeps !== undefined) payload.rm_deps = rmDeps;
      if (clearRef) payload.clear_ref = true;
      else if (ref !== undefined) payload.ref = ref;
      if (resolveRef && !clearRef) payload.resolve_ref = true;
      return call('PATCH', taskPath(id), payload);
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
    // assign gives the task to a member (an idea is promoted first); the
    // assignee gets the brief in its inbox from the operator.
    assign(id, agentId) {
      return call('POST', taskPath(id, '/assign'), { to: agentId });
    },
    unassign(id) {
      return call('POST', taskPath(id, '/unassign'), {});
    },
    // reopen puts a done or dropped task back on the board (`taskboard
    // reopen`): where it was, or in stage, and with to as its assignee ('' takes
    // it off; undefined keeps it). A plain reopen goes through status, which
    // a node from before the reopen route answers too.
    reopen(id, { stage = '', to, before = '', after = '' } = {}) {
      if (!stage && to === undefined && !before && !after) return call('POST', taskPath(id, '/status'), { status: 'open' });
      const body = {};
      if (stage) body.stage = stage;
      if (to !== undefined) body.to = to;
      if (before) body.before = before;
      if (after) body.after = after;
      return call('POST', taskPath(id, '/reopen'), body);
    },
    // moveToGroup moves the task to another group of its node, or with
    // clone copies it there (groups-taskboard-transfer.js).
    moveToGroup(id, group, { clone = false, stage = '' } = {}) {
      return call('POST', taskPath(id, '/move-to-group'), { group, clone, ...(stage ? { stage } : {}) });
    },
    // transferIn creates, on this node's group, a task moved or cloned in
    // from another node; origin is { node, group, task } there.
    transferIn(group, { title, body = '', ref = '', refLabel = '', stage = 'idea', clone = false, origin }) {
      return call('POST', `/v1/groups/${encodeURIComponent(group)}/taskboard/transfer-in`,
        { title, body, ref, ref_label: refLabel, stage, clone, origin });
    },
    // movedAway drops the original of a task moved to another node, noting
    // where it went: to is { node, group, task }.
    // With check, it only asks the gates, before the task is created
    // elsewhere.
    movedAway(id, to, { check = false } = {}) {
      return call('POST', taskPath(id, '/moved-away'), check ? { check: true } : { to });
    },
  };
}

export const taskboardApi = createTaskboardApi();
