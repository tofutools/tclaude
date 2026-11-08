// groups-taskboard-colvis.js -- which of a board's columns are drawn
// (docs/taskboard.md "Hiding and cordoning columns", tk_bf5d2b02).
//
// A board-level state rides the board's columns (entry.columns[].state for
// a custom one, entry.builtin_columns for Ideas and Done):
//
//   hidden    the board does not draw it, for everyone; its tasks keep it
//             (taskboard columns hide|show <group> <column>)
//   cordoned  drawn greyed; it takes no new task, its tasks may leave
//             (taskboard columns cordon|uncordon <group> <column>; custom
//             columns only)
//
// On top of that a viewer hides columns for themselves, remembered per group
// in dashPrefs like the view and the lanes' order. Next, In progress and
// Blocked are always drawn. A hidden column's tasks are counted in a "◌ N in
// hidden columns" tag, so nothing on the board goes quietly missing.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { dashPrefs } from './prefs.js';
import { COLUMN_STATE } from './taskboard-context.js';
import { ViewMenu, ViewMenuHeading, ViewMenuSep, ViewToggle } from './view-menu.js';

const html = htm.bind(h);

// HIDEABLE_BUILTINS are the built-in columns a board or a viewer may hide:
// the lanes key and the slug the daemon takes.
export const HIDEABLE_BUILTINS = [
  { key: 'ideas', slug: 'ideas', label: 'Ideas' },
  { key: 'done', slug: 'done', label: 'Done this week' },
];

const MINE_PREF = (group) => `tclaude.dash.groups.taskboard.columns-hidden.${group}`;

// mineHidden reads the column keys a viewer hides on group's board.
export function mineHidden(group) {
  try {
    const list = JSON.parse(dashPrefs.getItem(MINE_PREF(group)) || '[]');
    if (Array.isArray(list)) return list.filter((key) => typeof key === 'string' && (key === 'ideas' || key === 'done' || key.startsWith(COLUMN_STATE)));
  } catch (_) { /* a malformed value reads as none */ }
  return [];
}

export function setMineHidden(group, keys) {
  if (keys.length) dashPrefs.setItem(MINE_PREF(group), JSON.stringify([...new Set(keys)]));
  else dashPrefs.removeItem(MINE_PREF(group));
}

// boardHidden are the column keys the board itself hides.
export function boardHidden(entry) {
  const out = [];
  for (const b of HIDEABLE_BUILTINS) if (entry?.builtin_columns?.[b.slug] === 'hidden') out.push(b.key);
  for (const c of entry?.columns || []) if (c.state === 'hidden') out.push(COLUMN_STATE + c.slug);
  return out;
}

// hideableColumns are the columns a ⋯ or the Columns menu may hide: Ideas,
// the custom ones, Done; each with its key, slug, label and board state.
export function hideableColumns(entry) {
  const custom = (entry?.columns || []).map((c) => ({ key: COLUMN_STATE + c.slug, slug: c.slug, label: c.label, custom: true, state: c.state || '' }));
  const builtin = (b) => ({ ...b, custom: false, state: entry?.builtin_columns?.[b.slug] || '' });
  return [builtin(HIDEABLE_BUILTINS[0]), ...custom, builtin(HIDEABLE_BUILTINS[1])];
}

// withColumnView is the entry the views draw: hiddenColumns lists the keys
// not drawn (the board's and the viewer's), and colvis carries the menus'
// writes. mine are the viewer's hidden keys; onMine stores a new list.
export function withColumnView(entry, { group, mine = [], onMine = () => {}, actions = null }) {
  if (!entry) return entry;
  const hidden = [...new Set([...boardHidden(entry), ...mine])];
  const canBoard = !!actions && typeof actions.api?.setColumnState === 'function';
  const colvis = {
    mine,
    board: boardHidden(entry),
    hideMine: (key, on) => onMine(on ? [...mine, key] : mine.filter((k) => k !== key)),
    // setBoard(slug, state): shown | hidden | cordoned, for the whole board.
    setBoard: canBoard ? (slug, state, label = 'Column') => actions.run('columns', label, (api) => api.setColumnState(group, slug, state)) : null,
  };
  return { ...entry, hiddenColumns: hidden, colvis };
}

// hiddenTally counts the board's tasks standing in hidden columns, per
// column, in the lanes' terms (columnOf is laneColumn).
export function hiddenTally(entry, columnOf, now = Date.now()) {
  const hidden = new Set(entry?.hiddenColumns || []);
  const per = new Map();
  if (!hidden.size) return { total: 0, per };
  for (const task of [...(entry.tasks || []), ...(entry.closed || [])]) {
    const key = columnOf(task, now);
    if (key && hidden.has(key)) per.set(key, (per.get(key) || 0) + 1);
  }
  let total = 0;
  for (const n of per.values()) total += n;
  return { total, per };
}

// columnStateItems are the ⋯ items a column head adds for its visibility:
// hide for me, and for a viewer who manages the board's columns hide on the
// board and (a custom column) cordon or lift the cordon.
export function columnStateItems(column, entry) {
  const colvis = entry?.colvis;
  if (!colvis) return [];
  const item = (key, label, title, run) => ({ key, label, attrs: { 'data-column-menu': key, title }, run });
  const slug = column.slug || column.key;
  const items = [item('hide-mine', 'Hide for me', 'Only on this dashboard; the board still shows it to others', () => colvis.hideMine(column.key, true))];
  if (colvis.setBoard) {
    items.push(item('hide-board', 'Hide on the board', `taskboard columns hide <group> ${slug}: for everyone; its tasks keep it`,
      () => colvis.setBoard(slug, 'hidden', 'Hide column')));
    if (column.custom) {
      items.push(column.cordoned
        ? item('uncordon', 'Lift the cordon', `taskboard columns uncordon <group> ${slug}: it takes tasks again`, () => colvis.setBoard(slug, 'shown', 'Lift cordon'))
        : item('cordon', 'Cordon', `taskboard columns cordon <group> ${slug}: it takes no new task; its tasks may leave`,
          () => colvis.setBoard(slug, 'cordoned', 'Cordon column')));
    }
  }
  return items;
}

// HiddenColumnsTag is the toolbar's "◌ N in hidden columns", with the
// columns in its tooltip. Nothing when no hidden column holds a task.
export function HiddenColumnsTag({ entry, tally }) {
  if (!tally.total) return null;
  const labels = new Map(hideableColumns(entry).map((c) => [c.key, c.label]));
  const why = [...tally.per].map(([key, n]) => `${n} in ${labels.get(key) || key}`).join(', ');
  return html`<span class="ui-tag tb-hidden-tag" data-tb-hidden=${tally.total} title=${`Hidden columns hold ${why}. Show them from Columns ▾.`}>
    ◌ ${tally.total} in hidden column${tally.per.size === 1 ? '' : 's'}</span>`;
}

// ColumnsMenu is the toolbar's Columns ▾: which columns this viewer sees,
// and, for a viewer who manages the board's columns, which the board hides
// or cordons. Its badge counts the columns not drawn.
export function ColumnsMenu({ entry, group }) {
  const [open, setOpen] = useState(false);
  const colvis = entry?.colvis;
  if (!colvis) return null;
  const columns = hideableColumns(entry);
  const id = `tb-cols-${String(group).replace(/[^a-zA-Z0-9-]/g, (c) => `_${c.codePointAt(0).toString(16)}_`)}`;
  const safe = (key) => key.replace(':', '-');
  return html`<${ViewMenu} open=${open} onOpenChange=${setOpen} label="Columns ▾" badge=${(entry.hiddenColumns || []).length} buttonClass="tb-columns-menu"
      ids=${{ button: `${id}-btn`, menu: `${id}-menu`, badge: `${id}-badge` }} title="Choose which columns the board shows">
    <${ViewMenuHeading}>For me<//>
    ${columns.map((c) => html`<${ViewToggle} key=${c.key} id=${`${id}-mine-${safe(c.key)}`}
      title=${c.state === 'hidden' ? 'The board hides it for everyone' : 'Only on this dashboard'}
      checked=${!colvis.mine.includes(c.key) && c.state !== 'hidden'}
      onChange=${(on) => { if (c.state !== 'hidden') colvis.hideMine(c.key, !on); }}>${c.label}${c.state === 'hidden' ? ' (hidden on the board)' : ''}<//>`)}
    ${colvis.setBoard && html`<${ViewMenuSep} />
      <${ViewMenuHeading}>On the board, for everyone<//>
      ${columns.map((c) => html`<${ViewToggle} key=${`b-${c.key}`} id=${`${id}-board-${safe(c.key)}`}
        title=${`taskboard columns ${c.state === 'hidden' ? 'show' : 'hide'} ${group} ${c.slug}`}
        checked=${c.state !== 'hidden'} onChange=${(on) => colvis.setBoard(c.slug, on ? 'shown' : 'hidden', on ? 'Show column' : 'Hide column')}>${c.label}<//>`)}
      ${columns.some((c) => c.custom) && html`<${ViewMenuSep} />
        <${ViewMenuHeading}>Takes new tasks<//>
        ${columns.filter((c) => c.custom).map((c) => html`<${ViewToggle} key=${`c-${c.key}`} id=${`${id}-cordon-${safe(c.key)}`}
          title=${`taskboard columns ${c.state === 'cordoned' ? 'uncordon' : 'cordon'} ${group} ${c.slug}`}
          checked=${c.state !== 'cordoned'} onChange=${(on) => colvis.setBoard(c.slug, on ? 'shown' : 'cordoned', on ? 'Lift cordon' : 'Cordon column')}>${c.label}<//>`)}`}`}
  <//>`;
}
