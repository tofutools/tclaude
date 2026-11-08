// groups-taskboard-stage.js — the one picker for where a task goes, wherever
// it is chosen by hand: the New task form's Starts in, the edit form's Where
// it is, and a List row's ⋯ Move to… and Reopen… (TaskMoveDialog). It is the
// in-form type-to-filter choice (ChoiceCombobox on FilterCombobox, as the
// After picker's): the board's own places first (Idea, Next, In progress,
// …), then the board's custom columns (In review, In CI, …), so a board with
// many columns still picks in a few keys.
//
// A custom column's key is `col:<slug>` (COLUMN_STATE), as the List filter
// and the Lanes view name it. A column the board hides or has cordoned
// (column.state, tk_bf5d2b02) is not offered.

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Dialog } from './ui-dialog.js';
import { Button } from './ui-components.js';
import { ChoiceCombobox } from './choice-combobox.js';
import { COLUMN_STATE } from './taskboard-context.js';

const html = htm.bind(h);

// columnSlug is the slug in a `col:<slug>` key, or ''.
export function columnSlug(key) {
  return String(key || '').startsWith(COLUMN_STATE) ? String(key).slice(COLUMN_STATE.length) : '';
}

// offeredColumns are the custom columns a picker offers: not the ones the
// board hides or has disabled.
export function offeredColumns(columns = []) {
  return (columns || []).filter((column) => column && column.slug && !column.hidden && !column.disabled
    && column.state !== 'hidden' && column.state !== 'cordoned');
}

// stageItems are the picker's rows: `places` ({ key, label, wizard?, hint? })
// first, then the offered columns. off(key) says why a row cannot be picked
// (a reason string), or ''.
export function stageItems(places, columns = [], { wizard = false, off = () => '' } = {}) {
  const row = (key, label, hint, group) => {
    const why = off(key) || '';
    return { key, label, hint: why ? '' : hint || '', group, disabled: !!why, reason: why || undefined, title: why || hint || label };
  };
  return [
    ...places.map((place) => row(place.key, wizard ? place.wizard || place.label : place.label, place.hint, 'Board')),
    ...offeredColumns(columns).map((column) => row(COLUMN_STATE + column.slug, column.label, '', 'Columns')),
  ];
}

// StagePicker is the field. value is a key ('' only where noneLabel says
// what none means, as Reopen's "where it was").
// floating false keeps the list in the flow (a short dialog would clip it).
export function StagePicker({ id, value, onChange, places, columns = [], wizard = false, off, disabled = false, ariaLabel = 'Where it goes', noneLabel = '', floating = true, attrs = {} }) {
  return html`<${ChoiceCombobox} id=${id} className="tb-stage-pick" value=${value} onChange=${(key) => onChange(key)}
    items=${stageItems(places, columns, { wizard, off })} noneLabel=${noneLabel} floating=${floating} ariaLabel=${ariaLabel} disabled=${disabled}
    placeholder="Type to find a column…" emptyHint="No such column on this board." attrs=${{ 'data-stage-pick': id, ...attrs }} />`;
}

// movePlaces are what Move to… offers a live task: the stages, by name.
const MOVE_PLACES = [
  { key: 'idea', label: 'Idea', wizard: 'Vision', hint: 'back to the ideas' },
  { key: 'next', label: 'Next', wizard: 'Next', hint: 'the queue (an assigned task: in progress)' },
];

// REOPEN_PLACES add where the task was, the default.
const REOPEN_PLACES = [
  { key: 'was', label: 'Where it was', wizard: 'Where it was', hint: 'its stage and place, with its assignee' },
  ...MOVE_PLACES,
];

// moveOffs says why a row cannot be picked for task: its own place, or an
// idea for an assigned task (reopen takes its assignee off when the viewer
// may unassign).
function moveOffs(task, { reopen, canAssign }) {
  const assigned = !!task.assignee?.agent_id;
  return (key) => {
    if (!reopen && (key === COLUMN_STATE + task.column || (!task.column && key === task.stage))) return 'it is here';
    if (reopen && key === 'idea' && assigned && !canAssign) return 'an assigned task reopens in next; taking its assignee off needs taskboard.assign';
    return '';
  };
}

// moveWrite is the write that moves task to key: a live one through
// `move --stage` (a column, or idea/next), a closed one through `reopen`
// (its stage or column in the same write; an idea without its assignee).
export function moveWrite(task, key, { reopen = false } = {}) {
  const slug = columnSlug(key);
  if (!reopen) return { name: 'Move', run: (api) => api.stage(task.id, slug || key) };
  if (key === 'was' || !key) return { name: 'Reopen', run: (api) => api.reopen(task.id, {}) };
  const opts = { stage: slug || key };
  if (key === 'idea' && task.assignee?.agent_id) opts.to = '';
  return { name: 'Reopen', run: (api) => api.reopen(task.id, opts) };
}

// moveCommand is the CLI line for the move, as the forms show theirs.
export function moveCommand(task, key, reopen) {
  const slug = columnSlug(key);
  if (!reopen) return `taskboard move ${task.id} --stage ${slug || key || '…'}`;
  if (!key || key === 'was') return `taskboard reopen ${task.id}`;
  if (slug) return `taskboard reopen ${task.id} --stage ${slug}`;
  return `taskboard reopen ${task.id} --${key}${key === 'idea' && task.assignee?.agent_id ? ' --unassign' : ''}`;
}

// TaskMoveDialog is ⋯ Move to… (a live task) and Reopen… (a closed one):
// the picker, and Enter moves it there. onSubmit(write) runs the write and
// resolves true when it landed.
export function TaskMoveDialog({ task, columns = [], reopen = false, canAssign = false, wizard = false, busy = false, onSubmit, onClose }) {
  const places = reopen ? REOPEN_PLACES : MOVE_PLACES;
  const off = moveOffs(task, { reopen, canAssign });
  const first = reopen ? 'was' : '';
  const [value, setValue] = useState(first);
  const boxRef = useRef(null);
  // The dialog focuses the picker's box, which ChoiceCombobox renders.
  const focusRef = useRef({ get current() { return boxRef.current?.querySelector('input') || null; } }).current;
  const sending = useRef(false);
  const chosen = value && !off(value);
  const submit = async () => {
    if (!chosen || busy || sending.current) return;
    sending.current = true;
    try {
      if (await onSubmit(moveWrite(task, value, { reopen }))) onClose();
    } finally {
      sending.current = false;
    }
  };
  const title = reopen ? (wizard ? 'Revive quest' : 'Reopen task') : (wizard ? 'Move quest' : 'Move task');
  return html`<${Dialog} id="taskboard-move-modal" dialogClass="cron-create-modal taskboard-move-modal"
    title=${title} onClose=${onClose} onSubmitHotkey=${() => void submit()} busy=${busy} cancel="Cancel"
    initialFocusRef=${focusRef}
    footer=${html`<${Button} variant="primary" id="taskboard-move-submit" busy=${busy} disabled=${busy || !chosen}
      onClick=${() => { void submit(); }}>${reopen ? 'Reopen' : 'Move'}<//>`}>
    <div class="modal-meta tb-reason-task" data-task-form=${reopen ? 'reopen' : 'move'}>“${task.title || task.id}”</div>
    <div class="cron-create-row tb-new-row" ref=${boxRef}><span class="cron-create-label">${reopen ? 'Reopen in' : 'Move to'}</span>
      <span class="tb-new-field">
        <${StagePicker} id="taskboard-move-pick" value=${value} onChange=${(key) => setValue(key || first)} places=${places} columns=${columns}
          wizard=${wizard} off=${off} disabled=${busy} floating=${false} ariaLabel=${reopen ? 'Reopen in' : 'Move to'} />
        <span class="ui-hint tb-new-hint">${moveCommand(task, value, reopen)} · Ctrl/Cmd+Enter</span>
      </span>
    </div>
  <//>`;
}
