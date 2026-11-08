// task-route.js -- a task's own dashboard URL (docs/taskboard.md "A task's
// link"): /groups?task=<task-id>[@<node>]:<group>. Opening it unfolds the
// group and its Task board, scrolls to the task and opens its edit form;
// opening or closing the form records or steps back a history entry, so
// Back closes it and a shared link opens it.
//
// It is an overlay route (overlay-routes.js), the reader's and the library
// manager's mechanism: this module holds the task the location asks for, a
// board that holds that task opens its form (groups-taskboard.js
// TaskboardRows), and the board announces the form opening and closing.
//
// The id and the node come first and the group last, as is: an id (tk_…)
// and a node (inst_…) have a fixed alphabet, a group name may hold ':' or
// '@'.

import { createContext } from 'preact';
import { signal } from '@preact/signals';
import { announceOverlay, registerOverlayRoute } from './overlay-routes.js';
import { revealDropTarget } from './drop-landing.js';
import { writeClipboard } from './insecure-origin.js';

const TASK_ID = /^tk_[0-9a-z]+$/;
const NODE_ID = /^inst_[0-9a-z]+$/;

// parseTaskRoute reads a `task` value: { taskId, node, group }, or null.
export function parseTaskRoute(value) {
  const text = String(value || '');
  const colon = text.indexOf(':');
  if (colon <= 0) return null;
  const head = text.slice(0, colon);
  const group = text.slice(colon + 1);
  const [taskId, node = '', extra] = head.split('@');
  if (extra !== undefined || !TASK_ID.test(taskId) || (node && !NODE_ID.test(node)) || !group) return null;
  return { taskId, node, group };
}

// formatTaskRoute is the `task` value for a task on a group's board; node is
// the linked node's id, '' for this one.
export function formatTaskRoute({ taskId, node = '', group }) {
  return `${taskId}${node ? `@${node}` : ''}:${group}`;
}

// taskPath is a task's dashboard path, for an <a href> or a copied link.
export function taskPath(target) {
  return `/groups?${new URLSearchParams({ task: formatTaskRoute(target) })}`;
}

// taskURL is the whole link, on this dashboard's origin.
export function taskURL(target, origin = globalThis.location?.origin || '') {
  return `${origin}${taskPath(target)}`;
}

// sameTask says whether two route targets name one task on one board.
export function sameTask(a, b) {
  return !!a && !!b && a.taskId === b.taskId && (a.node || '') === (b.node || '') && a.group === b.group;
}

// TaskLink is the board a task title sits on, { group, node }, so the
// title's <a href> and the ⋯ Copy link can name the task. A board
// (TaskboardRows) provides it; outside one a title has no link.
export const TaskLink = createContext(null);

// copyLinkItem is the ⋯ menu's Copy link for a task on `board`.
export function copyLinkItem(taskId, board) {
  return {
    label: 'Copy link',
    attrs: { 'data-task-menu': 'copy-link', title: "The task's dashboard link: it opens the board at the task, its edit form open" },
    run: () => { writeClipboard(taskURL({ taskId, node: board.node, group: board.group })); },
  };
}

// taskRoute is the task the location asks for (its form open), or null.
export const taskRoute = signal(null);

// openTaskRoute records that a board opened a task's form: a history step,
// unless the location already names it (a restore, or the form reopened on
// itself).
export function openTaskRoute(target) {
  if (sameTask(taskRoute.value, target)) return;
  taskRoute.value = { ...target };
  announceOverlay();
}

// closeTaskRoute records that the form for `target` closed: Back, when the
// entry before is the one without it.
export function closeTaskRoute(target) {
  if (!sameTask(taskRoute.value, target)) return;
  taskRoute.value = null;
  announceOverlay({ closed: true });
}

// A link (or Back/Forward) asks for a task: unfold its group and Task board
// so the board draws it, and let the board open the form.
registerOverlayRoute('task', {
  value: () => (taskRoute.value ? formatTaskRoute(taskRoute.value) : ''),
  restore: (value) => {
    const target = parseTaskRoute(value);
    taskRoute.value = target;
    if (target) {
      try { revealDropTarget({ group: target.group, node: target.node, section: 'taskboard' }); } catch (_) { /* a link must never break the page */ }
    }
  },
});
