// groups-taskboard-deps.js — a task's dependencies in the New and Edit task
// forms (`add --after`, `edit --add-dep / --rm-dep`): the chosen ones as
// chips (✕ takes one off), and the shared type-to-filter list
// (FilterCombobox) of the board's other live tasks to add, which scrolls
// rather than growing with the board.

import { h } from 'preact';
import { useMemo } from 'preact/hooks';
import htm from 'htm';
import { FilterCombobox } from './filter-combobox.js';

const html = htm.bind(h);

// DEPS_CAP bounds the rows the list renders; a board holds at most 500 live
// tasks, and the list scrolls.
const DEPS_CAP = 200;

// waitersOf is every task that depends on id, directly or through others:
// choosing one of them as id's dependency would make a cycle.
export function waitersOf(id, tasks) {
  const out = new Set();
  if (!id) return out;
  const dependents = new Map();
  for (const task of tasks) {
    for (const dep of task.depends_on || []) {
      if (!dependents.has(dep)) dependents.set(dep, []);
      dependents.get(dep).push(task.id);
    }
  }
  const queue = [id];
  while (queue.length) {
    for (const next of dependents.get(queue.shift()) || []) {
      if (!out.has(next) && next !== id) { out.add(next); queue.push(next); }
    }
  }
  return out;
}

// depChoices are the tasks the picker offers: the board's live tasks but
// the task itself and the tasks that wait on it.
export function depChoices(tasks, taskId = '') {
  const waiting = waitersOf(taskId, tasks);
  return tasks.filter((task) => task.id !== taskId && !waiting.has(task.id))
    .map((task) => ({ key: task.id, label: task.title || task.id, hint: task.id, keywords: task.id }));
}

// DepsPicker edits value, the dependency ids. tasks are the board's live
// tasks; closed its recently closed ones (a dependency may be done already,
// and its chip still shows its title); taskId the task being edited ('' for
// a new one).
export function DepsPicker({ tasks = [], closed = [], value = [], onChange, disabled = false, taskId = '' }) {
  const items = useMemo(() => depChoices(tasks, taskId), [tasks, taskId]);
  const title = (id) => [...tasks, ...closed].find((t) => t.id === id)?.title || id;
  const set = (next) => onChange([...new Set(next)]);
  if (!items.length && !value.length) return html`<span class="ui-hint">No other live task to wait on.</span>`;
  return html`<div class="tb-deps-picker">
    ${value.length > 0 && html`<span class="tb-edit-deps">${value.map((id) => html`<span key=${id} class="tb-edit-dep" data-dep=${id} title=${id}>
      ${title(id)}
      <button data-ui-exempt type="button" class="tb-edit-dep-rm" disabled=${disabled} aria-label=${`No longer after ${title(id)}`}
        title="No longer wait for it" onClick=${() => set(value.filter((v) => v !== id))}>✕</button>
    </span>`)}</span>`}
    <${FilterCombobox} items=${items} value=${value} onChange=${set} cap=${DEPS_CAP} disabled=${disabled}
      ariaLabel="Add a dependency" placeholder=${`Type to find a task to wait on (${items.length})…`} emptyHint="No task matches." />
  </div>`;
}
