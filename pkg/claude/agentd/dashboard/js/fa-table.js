// fa-table.js -- the Fleet admin pages' table: the house compact table with
// the fa-table look (dashboard.css .fa-table), a plain text header row and
// one body. Tables that need more (a sortable header, a key/value layout, no
// header) still write their own <table class="fa-table …">.

import { h } from 'preact';
import htm from 'htm';
import { Button } from './ui-components.js';

const html = htm.bind(h);

// FaTable renders <table class="fa-table ui-compact" id=…> with a <th> per
// head label (an empty label for an untitled column, such as the actions)
// and children as the <tbody> rows.
export function FaTable({ id, head, children }) {
  return html`<table class="fa-table ui-compact" id=${id}>
    <thead><tr>${head.map((label, i) => html`<th key=${i}>${label}</th>`)}</tr></thead>
    <tbody>${children}</tbody>
  </table>`;
}

// ChangeTable lists what an import or an accepted offer would change, one
// row per item with a checkbox to skip it: "new", "overwrites yours" (a
// warning) or "unchanged" (which cannot be picked), and a security badge on
// an item that decides what this node's agents may do. onSkip gets the new
// skip list; locked disables every checkbox.
export function ChangeTable({ id, changes, skip, onSkip, locked = false }) {
  return html`<${FaTable} id=${id} head=${['', 'Item', 'Change', '']}>${changes.map((c) => html`<tr key=${c.item} data-item=${c.item}>
    <td><input type="checkbox" aria-label=${`include ${c.item}`} checked=${!skip.includes(c.item)} disabled=${locked || c.action === 'unchanged'}
      onChange=${(e) => onSkip(e.currentTarget.checked ? skip.filter((x) => x !== c.item) : [...skip, c.item])} /></td>
    <td><code>${c.item}</code></td>
    <td class=${c.action === 'replace' ? 'fa-warn' : c.action === 'unchanged' ? 'muted' : ''}>${c.action === 'replace' ? 'overwrites yours' : c.action === 'create' ? 'new' : 'unchanged'}</td>
    <td>${c.security && c.action !== 'unchanged' ? html`<span class="ui-tag ui-tag-accent fa-badge fa-sensitive" title="decides what this node's agents may do">security</span>` : ''}</td>
  </tr>`)}<//>`;
}

// CopyCode shows a value to read out or paste elsewhere (an instance ID, a
// fingerprint) as code with a copy link; the toast says it was copied, or
// shows the value when the clipboard is unavailable.
export function CopyCode({ value, what, copy, toast, class: cls }) {
  const onCopy = () => copy(value).then(() => toast(`${what} copied`, false)).catch(() => toast(`${what}: ${value}`, false));
  return html`<span class=${cls}><code>${value}</code> <${Button} size="sm" variant="link" class="fa-link" onClick=${onCopy} title=${`Copy the ${what.toLowerCase()}`}>copy<//></span>`;
}
