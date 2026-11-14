// groups-taskboard-backlog.js -- the Task board's Backlog (tk_5d2547b0,
// docs/taskboard.md "The backlog"): parked work before the ideas, never
// started, queued or nudged, and out of the way by default.
//
// The board's toolbar always carries the Backlog chip, after the List |
// Lanes switch: "Backlog · N ▸" while the backlog is hidden, "▾" while it is
// shown. One click shows or hides it, remembered per viewer and group. The
// chip is a drop target either way: a card or row dragged onto it is parked
// (`taskboard backlog <task>`). Shown, the Lanes draw a Backlog column before
// the lanes (groups-taskboard-lanes.js BacklogColumn) and the List a Backlog
// band after the ideas (groups-taskboard.js).

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { dashPrefs } from './prefs.js';

const html = htm.bind(h);

export const BACKLOG = 'backlog';
const TASK_DRAG_TYPE = 'application/x-tclaude-task';
const BACKLOG_PREF = (group) => `tclaude.dash.groups.taskboard.backlog.${group}`;

// isParked reports a live task in the backlog.
export function isParked(task) {
  return task?.stage === BACKLOG && (task.status === 'open' || task.status === 'blocked');
}

// backlogTasks are a board's parked tasks, in the board's order.
export function backlogTasks(entry) {
  return (entry?.tasks || []).filter(isParked);
}

// backlogSupported says whether the board's node knows the backlog: an
// older linked node sends no backlog count, and would refuse a park.
export function backlogSupported(entry) {
  return entry?.counts != null && Object.prototype.hasOwnProperty.call(entry.counts, 'backlog');
}

// backlogShown reads whether this viewer shows the group's backlog: hidden
// until they show it.
export function backlogShown(group) {
  try {
    return dashPrefs.getItem(BACKLOG_PREF(group)) === '1';
  } catch (_) {
    return false;
  }
}

export function setBacklogShown(group, on) {
  try {
    if (on) dashPrefs.setItem(BACKLOG_PREF(group), '1');
    else dashPrefs.removeItem(BACKLOG_PREF(group));
  } catch (_) { /* a viewer without storage keeps the default */ }
}

// parkable says whether the viewer may park task: a live task, not an epic,
// not already parked, on a board the viewer writes.
export function parkable(task, actions) {
  return !!actions && !!task && (task.status === 'open' || task.status === 'blocked') && task.kind !== 'epic' && task.stage !== BACKLOG;
}

// park and unpark are the writes: `taskboard backlog <task>` and `taskboard
// move <task> --stage idea`.
export const park = (actions, task) => actions.run(`${task.id}:stage`, 'Move to backlog', (api) => api.stage(task.id, BACKLOG));
export const unpark = (actions, task) => actions.run(`${task.id}:stage`, 'Back to ideas', (api) => api.stage(task.id, 'idea'));

// backlogItems are a task's ⋯ items for the backlog: "Move to backlog" on a
// live task, "Back to ideas" on a parked one.
export function backlogItems(task, actions) {
  if (!actions || actions.busy || !task || task.kind === 'epic') return [];
  const item = (key, label, title, run) => ({ label, attrs: { 'data-task-menu': key, title }, run });
  if (isParked(task)) {
    return [item('unpark', 'Back to ideas', 'taskboard promote: out of the backlog, to the top of the ideas', () => unpark(actions, task))];
  }
  if (!parkable(task, actions)) return [];
  return [item('park', 'Move to backlog', 'taskboard backlog: park it, out of the way; never started or queued', () => park(actions, task))];
}

// BacklogChip is the toolbar's Backlog toggle and drop target. count is the
// board's backlog count; onToggle shows or hides the backlog. tasks find
// the dragged task by its id on a drop.
export function BacklogChip({ count = 0, open = false, onToggle, actions = null, tasks = [] }) {
  const [over, setOver] = useState(false);
  const [flash, setFlash] = useState(false);
  const accepts = (event) => {
    if (!actions) return false;
    const types = event.dataTransfer?.types;
    return !!types && Array.from(types).includes(TASK_DRAG_TYPE);
  };
  const label = 'Backlog';
  const title = open
    ? `Hide the ${label.toLowerCase()} (parked work: never started, queued or nudged). Drop a card here to park it.`
    : `Show the ${label.toLowerCase()} (parked work: never started, queued or nudged). Drop a card here to park it.`;
  return html`<button data-ui-exempt type="button" class=${`tb-backlog-chip${open ? ' tb-backlog-open' : ''}${over ? ' tb-backlog-over' : ''}${flash ? ' tb-backlog-flash' : ''}`}
      aria-pressed=${open ? 'true' : 'false'} aria-label=${`${label}, ${count} parked`} title=${title} data-tb-backlog=${count}
      onClick=${() => onToggle?.(!open)}
      onDragOver=${(event) => {
        if (!accepts(event)) return;
        event.preventDefault();
        event.stopPropagation();
        if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
        if (!over) setOver(true);
      }}
      onDragLeave=${() => setOver(false)}
      onDrop=${async (event) => {
        setOver(false);
        if (!accepts(event)) return;
        event.preventDefault();
        event.stopPropagation();
        const id = event.dataTransfer?.getData(TASK_DRAG_TYPE);
        const task = (tasks || []).find((t) => t.id === id);
        if (!parkable(task, actions)) return;
        const ok = await park(actions, task);
        if (ok) { setFlash(true); setTimeout(() => setFlash(false), 700); }
      }}>
    <span class="tb-backlog-label">${over ? `⤓ Park in ${label.toLowerCase()}` : label}</span>
    <span class="ui-badge tb-backlog-count">${count}</span>
    <span class="tb-backlog-caret" aria-hidden="true">${open ? '▾' : '▸'}</span>
  </button>`;
}
