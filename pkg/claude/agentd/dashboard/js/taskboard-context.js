// taskboard-context.js -- the dashboard's read of GET /v1/taskboard/context,
// every group task board this viewer can read (docs/taskboard.md):
//
//   { groups: [{ group_id, group, truncated,
//                counts: { live, needs_you, in_progress, next, ideas, blocked, stalled, queued, done_this_week, dropped, in_columns },
//                columns: [{ slug, label, count }],   the board's custom columns (In review, …)
//                tasks: [row], closed: [row] }] }
//
// A row is the task view less its body, plus assignee: { agent_id },
// assigned_by and assigned_at when someone holds it. A group the viewer
// cannot read is absent. The daemon answers 404 while features.taskboard is
// off; the source then reports `unavailable` and the Groups tab draws no Task
// board at all.

import { signal } from '@preact/signals';
import { useEffect } from 'preact/hooks';

export const TASKBOARD_CONTEXT_PATH = '/v1/taskboard/context';

// While the feature is off, look again this rarely rather than on every
// snapshot poll, so turning it on shows up without a reload.
const UNAVAILABLE_RETRY_MS = 30_000;

export function createTaskboardContextSource({ fetchImpl = (...args) => fetch(...args), now = () => Date.now() } = {}) {
  const context = signal(null);
  // idle | ready | unavailable | error
  const status = signal('idle');
  const error = signal('');
  let inflight = null;
  let unavailableAt = 0;

  // refresh reads the context, but while the feature is off only now and
  // then; load always reads.
  function refresh() {
    if (!inflight && status.value === 'unavailable' && now() - unavailableAt < UNAVAILABLE_RETRY_MS) {
      return Promise.resolve();
    }
    return load();
  }

  function load() {
    if (inflight) return inflight;
    inflight = (async () => {
      try {
        const response = await fetchImpl(TASKBOARD_CONTEXT_PATH, { credentials: 'same-origin', cache: 'no-store' });
        if (response.status === 404) {
          context.value = null;
          status.value = 'unavailable';
          unavailableAt = now();
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
        // Keep the last good context on screen.
        status.value = context.value ? 'ready' : 'error';
        error.value = cause?.message || String(cause);
      } finally {
        inflight = null;
      }
    })();
    return inflight;
  }

  // Every mounted reader shares one subscription to the snapshot poll.
  let readers = 0;
  const poll = () => { void refresh(); };
  function attach() {
    readers += 1;
    if (readers === 1) {
      document.addEventListener('tclaude:snapshot', poll);
      void load();
    }
    return () => {
      readers -= 1;
      if (readers === 0) document.removeEventListener('tclaude:snapshot', poll);
    };
  }

  return { context, status, error, refresh, attach };
}

export const taskboardContext = createTaskboardContextSource();

// useTaskboardEntry keeps the source polling while a reader is mounted
// and returns the group's entry, or null (the feature is off, the group is
// hidden from this viewer, or the first read is still out).
export function useTaskboardEntry(groupName, source = taskboardContext) {
  useEffect(() => source.attach(), [source]);
  return taskboardGroupEntry(source.context.value, groupName);
}

export function taskboardGroupEntry(context, groupName) {
  return (context?.groups || []).find((entry) => entry.group === groupName) || null;
}

// taskboardCountParts are the non-zero buckets of a board, in reading order:
// what waits on the operator, what is held up, what is moving, what is next,
// what is only an idea. Only waiting_on_you (the tasks assigned to the
// operator and the open questions) needs the operator; needs_you is the same
// count (tk_3cb51ce0). Blocked and stalled are plain counts. who names the
// operator ('you', or a linked node's operator).
// columns are the board's custom columns with their counts (the context's
// per-group columns): each non-empty one is a part after in progress.
export function taskboardCountParts(counts, who = 'you', columns = []) {
  const c = counts || {};
  const parts = [];
  const waiting = c.waiting_on_you || 0;
  if (waiting) parts.push({ key: 'waiting-on-you', text: `${waiting} waiting on ${who}` });
  if (c.blocked) parts.push({ key: 'blocked', text: `${c.blocked} blocked` });
  if (c.stalled) parts.push({ key: 'stalled', text: `${c.stalled} stalled` });
  if (c.in_progress) parts.push({ key: 'in-progress', text: `${c.in_progress} in progress` });
  for (const column of columns || []) {
    if (column?.count) parts.push({ key: `column-${column.slug}`, text: `${column.count} ${column.label}` });
  }
  if (c.next) parts.push({ key: 'next', text: `${c.next} next` });
  if (c.ideas) parts.push({ key: 'ideas', text: `${c.ideas} idea${c.ideas === 1 ? '' : 's'}` });
  const mine = c.ideas_for_you || 0;
  if (mine) parts.push({ key: 'ideas-for-you', text: `${mine} idea${mine === 1 ? '' : 's'} for ${who}` });
  return parts;
}

// taskboardCountsText is the folded section's one line:
// "9 · 1 waiting on you · 1 blocked · 2 in progress · 6 next · 3 ideas".
export function taskboardCountsText(counts, who = 'you', columns = []) {
  const live = counts?.live || 0;
  if (!live) return counts?.done_this_week ? `0 · ${counts.done_this_week} done this week` : 'empty';
  return [String(live), ...taskboardCountParts(counts, who, columns).map((part) => part.text)].join(' · ');
}

// COLUMN_STATE prefixes the state of a task in one of the board's custom
// columns (In review, In CI, …): `col:<slug>`. The lanes key their custom
// columns the same way.
export const COLUMN_STATE = 'col:';

// columnLabel is a custom column's label from its state or key
// (`col:<slug>`), the slug when the board does not list it.
export function columnLabel(state, columns = []) {
  const slug = String(state || '').slice(COLUMN_STATE.length);
  return (columns || []).find((column) => column.slug === slug)?.label || slug;
}

// taskRowState is what a row's state column says, derived exactly as the
// lanes are (docs/taskboard.md): blocked, stalled (assigned to an agent idle
// past taskboard.idle_grace without reporting), waiting (the operator holds
// it, queued or started), in progress (assigned and started), queued (open
// with an unmet dependency), idea, or open. A task its assignee holds
// without starting it reads as its stage (tk_3826822a).
export function taskRowState(task) {
  if (task.status === 'done' || task.status === 'dropped') return task.status;
  if (task.status === 'blocked') return 'blocked';
  if (task.stalled && task.assignee?.agent_id) return 'stalled';
  const ideaHeld = task.held && task.stage === 'idea';
  if (task.assignee?.operator && !ideaHeld) return 'waiting';
  // A task in a custom column (tcl-plby5h) reads as that column.
  if (task.column && task.status === 'open') return COLUMN_STATE + task.column;
  if (task.assignee?.agent_id && !task.held) return 'in-progress';
  if (task.queued) return 'queued';
  if (task.stage === 'idea') return 'idea';
  return 'open';
}

// actorAgentId reads "agent:<id>" history and assigned_by actors.
export function actorAgentId(actor) {
  return typeof actor === 'string' && actor.startsWith('agent:') ? actor.slice('agent:'.length) : '';
}
