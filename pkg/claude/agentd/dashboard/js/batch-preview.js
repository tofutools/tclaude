// batch-preview.js — a reusable "these items will change" preview with
// per-item select/deselect, for operator batch changes (the harness_config
// re-pin repair first; re-applying a spawn-profile edit wants it next).
//
// The caller owns the rows and the selection; this renders them and reports
// selection changes, so an apply step can send exactly what was left ticked.
import { h } from 'preact';
import htm from 'htm';
import { Button } from './ui-components.js';

const html = htm.bind(h);

// selectionOf returns the keys of every row, the default "change them all".
export function selectionOf(rows, rowKey, include = () => true) {
  return new Set((rows || []).filter(include).map(rowKey));
}

// toggled returns a new selection with key turned on or off.
export function toggled(selected, key, on) {
  const next = new Set(selected);
  if (on) next.add(key); else next.delete(key);
  return next;
}

// selectedRows keeps the rows still selected, in row order.
export function selectedRows(rows, rowKey, selected) {
  return (rows || []).filter((row) => selected.has(rowKey(row)));
}

// BatchPreview lists rows with a checkbox each. columns are
// { key, label, render?(row) }; notes are shown under the list, for the
// limits of what the batch covers.
export function BatchPreview({
  rows = [], rowKey, columns = [], selected, onChange, disabled = false,
  empty = 'Nothing to change.', notes = [], label = 'items',
}) {
  const all = selectionOf(rows, rowKey);
  const count = selectedRows(rows, rowKey, selected).length;
  return html`<div class="batch-preview">
    ${rows.length === 0 ? html`<p class="ui-empty-block">${empty}</p>` : html`
      <div class="batch-preview-head">
        <span class="batch-preview-count">${count} of ${rows.length} ${label} selected</span>
        <${Button} size="sm" variant="link" disabled=${disabled || count === rows.length} onClick=${() => onChange(all)}>Select all<//>
        <${Button} size="sm" variant="link" disabled=${disabled || count === 0} onClick=${() => onChange(new Set())}>Select none<//>
      </div>
      <table class="batch-preview-table ui-compact">
        <thead><tr><th></th>${columns.map((c) => html`<th key=${c.key}>${c.label}</th>`)}</tr></thead>
        <tbody>${rows.map((row) => {
          const key = rowKey(row);
          return html`<tr key=${key} data-key=${key}>
            <td><input type="checkbox" aria-label=${`Select ${key}`} checked=${selected.has(key)} disabled=${disabled}
              onChange=${(e) => onChange(toggled(selected, key, e.currentTarget.checked))} /></td>
            ${columns.map((c) => html`<td key=${c.key}>${c.render ? c.render(row) : row[c.key]}</td>`)}
          </tr>`;
        })}</tbody>
      </table>`}
    ${notes.length > 0 && html`<ul class="batch-preview-notes">${notes.map((n) => html`<li key=${n}>${n}</li>`)}</ul>`}
  </div>`;
}
