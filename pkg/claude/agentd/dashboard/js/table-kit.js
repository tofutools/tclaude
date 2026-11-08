// table-kit.js -- the structure dashboard tables share, built from one
// column list: the header row. Each table keeps its own look and its own
// sort state; this only renders what they all rendered by hand.

import { h } from 'preact';
import htm from 'htm';
import { SortTh } from './ui-components.js';

const html = htm.bind(h);

// TableHead renders a column list as <thead><tr>: a SortTh for a sortable
// column, a plain <th> for the rest (the controls, the actions).
//
//   cols        the column specs, in order.
//   activeCol   the sort key of the sorted column, or null; dir its direction.
//   onSort      (col) => void, run when a header is clicked or keyed. Leave it
//               out for a table whose header clicks are delegated through
//               data-sort-table / data-sort-col (the Groups view).
//   sortTable   the delegated sort table's name; sets data-sort-table and,
//               unless sortColAttr says otherwise, data-sort-col.
//   isSortable  which columns sort (default: those with a col).
//   colOf       a column's sort key (default: its col).
//   label       a column's header content; sortLabel the "Sort by …" name.
//   lead        content for the label-less controls (ctl) column's header.
//   theadClass  a class for the <thead>.
//   trailing    a cell rendered after the columns (an untitled extra column).
export function TableHead({
  cols, activeCol = null, dir, onSort, sortTable, sortColAttr = !!sortTable,
  isSortable = (c) => !!c.col, colOf = (c) => c.col,
  label = (c) => c.label || '', sortLabel = (c) => c.label,
  lead = null, theadClass, trailing = null,
}) {
  return html`<thead class=${theadClass}><tr>${cols.map((c, i) => {
    const key = c.key || (isSortable(c) && colOf(c)) || c.label || `plain-${i}`;
    if (!isSortable(c)) return html`<th key=${key}>${lead && c.key === 'ctl' ? lead : label(c)}</th>`;
    const col = colOf(c);
    return html`<${SortTh}
      key=${key}
      active=${activeCol != null && col === activeCol}
      dir=${dir}
      data-sort-table=${sortTable}
      data-sort-col=${sortColAttr ? col : undefined}
      sortLabel=${sortLabel(c)}
      onSort=${onSort ? () => onSort(col) : undefined}
    >${label(c)}<//>`;
  })}${trailing}</tr></thead>`;
}
