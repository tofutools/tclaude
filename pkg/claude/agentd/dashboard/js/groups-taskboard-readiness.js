// groups-taskboard-readiness.js -- a column grouped by readiness
// (docs/taskboard.md "Grouping by readiness", tk_3876b835): the ready cards
// first, then the waiting ones, then the blocked ones, each group in the
// board's own order. It is a view, the viewer's own: nothing is written, the
// order the board keeps (Position) stays as it is, and a card joins Ready by
// itself once what it waited on is done.
//
//   ready    open, no dependency still to finish, not in line for a full
//            lane
//   waiting  open, but queued: on its dependencies (unmet) or for a full
//            lane (queued_for)
//   blocked  blocked
//
// In the Lanes a viewer groups a column from its head's ⋯ (Group by
// readiness); a grouped head carries "◧ by readiness", which ungroups it. In
// the List the Filter's "group by readiness" groups each section. Both are
// remembered per board in dashPrefs, as the other view choices are.
//
// While grouped, a card moves within its group: the ⋯ and Alt+↑/↓ moves and
// a drag stay among the cards of its group, so the order underneath keeps
// meaning what the viewer sees.

import { createContext, h } from 'preact';
import { useContext } from 'preact/hooks';
import htm from 'htm';
import { dashPrefs } from './prefs.js';

const html = htm.bind(h);

export const READINESS = [
  { key: 'ready', label: 'Ready', title: 'Open, nothing to wait on: it can be taken now' },
  { key: 'waiting', label: 'Waiting', title: 'Waits on a dependency, or in line for a full lane' },
  { key: 'blocked', label: 'Blocked', title: 'Blocked' },
];

// LIST is the key the List's grouping is kept under, beside the Lanes'
// column keys.
export const LIST = 'list';

// readiness is a task's group: ready, waiting or blocked; '' for a closed
// task, which no grouping holds.
export function readiness(task) {
  if (task?.status === 'blocked') return 'blocked';
  if (task?.status !== 'open') return '';
  return task.queued || task.unmet?.length ? 'waiting' : 'ready';
}

// groupable reports a Lanes column a viewer may group: not Blocked (it holds
// only blocked cards) nor Done (ordered by close time).
export function groupable(column) {
  return !!column && column.key !== 'blocked' && column.key !== 'done';
}

// byReadiness is list grouped: the ready tasks, the waiting ones, the
// blocked ones, then any other (closed), each in list's order (a stable
// partition). heads maps the first task of each group to that group and
// its count, for a cell to draw a heading above it; there are none when
// every task is ready, so a cell with nothing waiting reads as it did.
export function byReadiness(list) {
  const tasks = list || [];
  const parts = new Map([...READINESS.map((r) => [r.key, []]), ['', []]]);
  for (const task of tasks) parts.get(readiness(task)).push(task);
  const heads = new Map();
  const allReady = parts.get('ready').length === tasks.length;
  if (!allReady) {
    for (const r of READINESS) {
      const part = parts.get(r.key);
      if (part.length) heads.set(part[0].id, { ...r, count: part.length });
    }
  }
  return { list: [...parts.values()].flat(), heads };
}

// readinessPeers are the tasks of list in task's readiness group: what a
// grouped card moves among.
export function readinessPeers(list, task) {
  const mine = readiness(task);
  return (list || []).filter((t) => readiness(t) === mine);
}

// sameGroup reports whether a drag of task onto anchor stays in its group.
export function sameGroup(task, anchor) {
  return readiness(task) === readiness(anchor);
}

const PREF = (group) => `tclaude.dash.groups.taskboard.readiness.${group}`;

// readyGrouped are the keys (Lanes column keys, and LIST) a viewer groups
// on group's board.
export function readyGrouped(group) {
  try {
    const list = JSON.parse(dashPrefs.getItem(PREF(group)) || '[]');
    if (Array.isArray(list)) return list.filter((key) => typeof key === 'string' && key);
  } catch (_) { /* a malformed value reads as none */ }
  return [];
}

export function setReadyGrouped(group, key, on) {
  const now = readyGrouped(group).filter((k) => k !== key);
  const next = on ? [...now, key] : now;
  if (next.length) dashPrefs.setItem(PREF(group), JSON.stringify(next));
  else dashPrefs.removeItem(PREF(group));
}

// ReadyGrouping carries a board's grouping to its views: has(key) and
// set(key, on). The default groups nothing and offers no toggle.
export const ReadyGrouping = createContext({ has: () => false, set: null });

export const useReadyGrouping = () => useContext(ReadyGrouping);

// readyMenuItem is a column head's ⋯ item that groups or ungroups it; none
// for a column that is not groupable or a board without the toggle.
export function readyMenuItem(column, grouping) {
  if (!groupable(column) || typeof grouping?.set !== 'function') return null;
  const on = grouping.has(column.key);
  return {
    key: 'readiness', label: on ? 'Show in board order' : 'Group by readiness',
    attrs: { 'data-column-menu': 'readiness', title: on
      ? 'Draw the cards in the board\'s order again'
      : 'Ready cards first, then waiting, then blocked; each in the board\'s order. Only on this dashboard; nothing is moved' },
    run: () => grouping.set(column.key, !on),
  };
}

// ReadyChip is a grouped column head's "◧ by readiness": a click ungroups it.
export function ReadyChip({ column }) {
  const grouping = useReadyGrouping();
  if (!groupable(column) || !grouping.has(column.key)) return null;
  const off = typeof grouping.set === 'function' ? () => grouping.set(column.key, false) : null;
  return html`<button type="button" data-ui-exempt class="tb-ready-chip" data-tb-ready=${column.key} disabled=${!off}
    title="Grouped by readiness, only on this dashboard: ready, then waiting, then blocked. Click to draw the board's order again."
    onClick=${off ? (event) => { event.stopPropagation(); off(); } : undefined}>◧ by readiness</button>`;
}

// ReadyHead is the heading a grouped cell draws above a group's first card.
export function ReadyHead({ head }) {
  return html`<div class=${`tb-ready-head tb-ready-${head.key}`} data-tb-ready-head=${head.key} title=${head.title}>
    <span class="tb-ready-label">${head.label}</span> <span class="tb-ready-count">${head.count}</span></div>`;
}

// groupedCell is what a cell draws for list in a column: the list (grouped
// when the viewer groups the column), the heads, and the peers a card moves
// among (its group's, when grouped).
export function groupedCell(list, on) {
  if (!on) return { list, heads: null, peers: () => list };
  const grouped = byReadiness(list);
  return { ...grouped, peers: (task) => readinessPeers(grouped.list, task) };
}
