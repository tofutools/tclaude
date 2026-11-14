// groups-taskboard-columns.js -- a board's custom columns in the lanes view
// (docs/taskboard.md "Custom columns"): a group's own columns, such as In
// review or In CI, between In progress and Blocked. A custom column's head
// has a ⋯ (rename, move left or right, remove) and the last head a
// "＋ column"; each is the route its CLI verb uses:
//
//   ＋ column          taskboard columns add <group> "<label>"
//   ⋯ Rename…          taskboard columns rename <group> <slug> "<label>"
//   ⋯ Move left/right  taskboard columns move <group> <slug> --before …|--last
//   ⋯ Remove           taskboard columns rm <group> <slug> (its tasks go
//                      back to next)
//
// Managing columns needs taskboard.columns on the group (taskboard.manage
// covers it; owners hold both); a refusal is the board's usual toast. Hiding
// and cordoning are groups-taskboard-colvis.js.

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Dialog } from './ui-dialog.js';
import { Button, Input } from './ui-components.js';
import { RowMenu } from './row-menu.js';
import { HIDEABLE_BUILTINS, columnStateItems } from './groups-taskboard-colvis.js';
import { boardNode, openBoardOptions } from './groups-taskboard-options-route.js';
import { readyMenuItem, useReadyGrouping } from './groups-taskboard-readiness.js';

const html = htm.bind(h);

// COLUMNS_CAP is how many custom columns a board holds (the daemon's cap).
export const COLUMNS_CAP = 4;
const LABEL_MAX = 40;

// ColumnNameDialog asks a column's name: a new column's, or a new name for
// one. onSubmit(label) runs the write and resolves true when it landed.
export function ColumnNameDialog({ title, line, value = '', ok, busy = false, onSubmit, onClose }) {
  const [label, setLabel] = useState(value);
  const inputRef = useRef(null);
  const sending = useRef(false);
  const invalid = !label.trim();
  const submit = async () => {
    if (invalid || busy || sending.current) {
      inputRef.current?.focus();
      return;
    }
    sending.current = true;
    try {
      if (await onSubmit(label.trim())) onClose();
      else inputRef.current?.focus();
    } finally {
      sending.current = false;
    }
  };
  const onKeyDown = (event) => {
    if (event.key !== 'Enter' || event.isComposing || event.keyCode === 229 || event.ctrlKey || event.metaKey) return;
    event.preventDefault();
    void submit();
  };
  return html`<${Dialog} id="taskboard-column-modal" dialogClass="cron-create-modal taskboard-column-modal"
    title=${title} onClose=${onClose} onSubmitHotkey=${() => void submit()} busy=${busy} initialFocusRef=${inputRef} cancel="Cancel"
    footer=${html`<${Button} variant="primary" id="taskboard-column-submit" busy=${busy} disabled=${busy || invalid}
      onClick=${() => { void submit(); }}>${ok}<//>`}>
    <p class="modal-hint ui-hint">${line}</p>
    <label class="cron-create-row">
      <span class="cron-create-label">Name</span>
      <${Input} domRef=${inputRef} id="taskboard-column-input" value=${label} invalid=${invalid} disabled=${busy} maxLength=${LABEL_MAX}
        aria-label="Column name" placeholder="e.g. In review" data-select-on-focus="" autocomplete="off" spellcheck="false"
        onInput=${(event) => setLabel(event.currentTarget.value)} onKeyDown=${onKeyDown} />
    </label>
  <//>`;
}

// columnMenuItems are a custom column's ⋯ items. columns are the board's
// custom columns in order; onRename opens the name dialog.
export function columnMenuItems({ column, columns, group, actions, onRename }) {
  if (!column.custom || !actions || actions.busy || typeof actions.api?.changeColumn !== 'function') return [];
  const i = columns.findIndex((c) => c.slug === column.slug);
  const run = (label, fn) => () => actions.run('columns', label, fn);
  const item = (key, label, title, rest) => ({ key, label, attrs: { 'data-column-menu': key, title }, ...rest });
  const left = columns[i - 1];
  const right = columns[i + 1];
  return [
    item('rename', 'Rename…', `taskboard columns rename ${group.name} ${column.slug}`, { run: () => onRename(column) }),
    item('left', 'Move left', `taskboard columns move ${group.name} ${column.slug} --before ${left?.slug || '…'}`, {
      disabled: !left, run: run('Move column', (api) => api.changeColumn(group.name, column.slug, { before: left.slug })),
    }),
    item('right', 'Move right', `taskboard columns move ${group.name} ${column.slug}`, {
      disabled: !right,
      run: run('Move column', (api) => api.changeColumn(group.name, column.slug, columns[i + 2] ? { before: columns[i + 2].slug } : { toEnd: true })),
    }),
    item('remove', 'Remove', `taskboard columns rm ${group.name} ${column.slug}: its tasks go back to Next (In progress when started)`, {
      danger: true, run: run('Remove column', (api) => api.removeColumn(group.name, column.slug)),
    }),
  ];
}

// optionsItems are a lane head's limit and options items: "Set limit…"
// (In progress and each custom column, for a viewer who changes the board)
// and "Column in Board options", the board's options drawer at the Columns
// tab, on that column. A board whose node serves no options
// (entry.options absent) has neither.
function optionsItems(column, group, entry, limits) {
  const lane = column.key === 'progress' ? 'in_progress' : column.custom ? column.slug : '';
  if (!lane || !entry?.options) return [];
  const items = [];
  if (limits?.can) {
    items.push({ key: 'limit', label: 'Set limit…', attrs: { 'data-column-menu': 'limit', title: `How many tasks ${column.label} holds at once (taskboard options set ${group.name} ${lane === 'in_progress' ? 'limits.in_progress.max' : `limits.columns.${lane}.max`}=…)` },
      run: () => limits.open(lane, column.label) });
  }
  items.push({ key: 'options', label: 'Column in Board options ↗', attrs: { 'data-column-menu': 'options', title: `The ${column.label} column in the board's options: its limit and whether it is idle-checked` },
    run: () => openBoardOptions({ group: group.name, node: boardNode(group), section: 'columns', column: lane }) });
  return items;
}

// ColumnHeadMenu is a column head's ⋯: a custom column's rename, move and
// remove, and any hideable column's hide and cordon items
// (groups-taskboard-colvis.js), then its limit and the column in the Board
// options, and last this viewer's Group by readiness
// (groups-taskboard-readiness.js). limits is the head's useLimitDialog.
export function ColumnHeadMenu({ column, columns, group, actions, entry = null, onRename, limits = null }) {
  const ready = readyMenuItem(column, useReadyGrouping());
  const manage = columnMenuItems({ column, columns, group, actions, onRename });
  // In progress is always shown: its ⋯ has only the limit items.
  const hideable = column.custom || HIDEABLE_BUILTINS.some((b) => b.key === column.key);
  const state = actions?.busy || !hideable ? [] : columnStateItems(column, entry);
  const items = [...manage, ...state, ...optionsItems(column, group, entry, actions?.busy ? null : limits), ...(ready ? [ready] : [])];
  if (!items.length) return null;
  return html`<span class="tb-col-menu"><${RowMenu} label=${`the ${column.label} column`} items=${items} buttonClass="tb-col-more"
    title=${column.custom ? `Rename, move, hide, limit or remove the ${column.label} column` : `More for the ${column.label} column`} /></span>`;
}

// useColumnNaming drives the add and rename dialogs: open('add') or
// open('rename', column), and the dialog to draw (null when none is up).
export function useColumnNaming(group, actions) {
  const [naming, setNaming] = useState(null);
  if (!actions || typeof actions.api?.addColumn !== 'function') return { open: () => {}, dialog: null, can: false };
  const close = () => setNaming(null);
  let dialog = null;
  if (naming?.kind === 'add') {
    dialog = html`<${ColumnNameDialog} title=${`New column on ${group.name}'s board`} ok="Add column" busy=${actions.busy === 'columns'}
      line="A column of the board's own, between In progress and Blocked: drag a card into it, or taskboard move --stage. A task there keeps its assignee and never stalls."
      onSubmit=${(label) => actions.run('columns', 'Add column', (api) => api.addColumn(group.name, label))} onClose=${close} />`;
  } else if (naming?.kind === 'rename') {
    const { column } = naming;
    dialog = html`<${ColumnNameDialog} title=${`Rename the ${column.label} column`} ok="Rename" value=${column.label} busy=${actions.busy === 'columns'}
      line=${`Its slug stays ${column.slug}, so taskboard move --stage ${column.slug} still finds it.`}
      onSubmit=${(label) => actions.run('columns', 'Rename column', (api) => api.changeColumn(group.name, column.slug, { label }))} onClose=${close} />`;
  }
  return { can: true, dialog, open: (kind, column = null) => setNaming({ kind, column }) };
}

// AddColumnButton is the "＋ column" on the last column head, while the
// board has room for another.
export function AddColumnButton({ naming, columns }) {
  if (!naming.can || columns.length >= COLUMNS_CAP) return null;
  return html`<${Button} size="sm" variant="link" class="tb-col-add" title="Add a column of the board's own, such as In review (taskboard columns add)"
    onClick=${(event) => { event.stopPropagation(); naming.open('add'); }}>＋ column<//>`;
}
