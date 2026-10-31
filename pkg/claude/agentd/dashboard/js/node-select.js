// node-select.js — the one node selector (tcl-vwuc0b): a button naming the
// selection that opens the node list, with a checkbox per node to pick
// several (multiple) or one row per node to pick one. Every view that narrows
// itself by node uses it, so picking nodes reads and behaves the same on
// every tab however many nodes there are.
import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDismissablePopover } from './hover-popover.js';

const html = htm.bind(h);

// NODE_SELECT_FILTER_AT is the list length from which the menu offers a
// type-to-filter box.
export const NODE_SELECT_FILTER_AT = 8;

const nodeName = (n) => `${n.local ? '⌂ ' : ''}${n.name}`;

// pickedNodes are the nodes a value selects, in list order: every node for an
// empty multiple value ("all") or a null single value.
export function pickedNodes(nodes, value, { multiple = false } = {}) {
  if (multiple) return value?.length ? nodes.filter((n) => value.includes(n.id)) : nodes;
  return value ? nodes.filter((n) => n.id === value) : nodes;
}

// nodeSelectSummary is the button's text for a selection.
export function nodeSelectSummary(nodes, value, { multiple = false, allLabel = 'All nodes' } = {}) {
  const picked = pickedNodes(nodes, value, { multiple });
  if (picked.length === nodes.length && (multiple || !value)) return `${allLabel} · ${nodes.length}`;
  if (picked.length <= 2) return picked.map(nodeName).join(', ') || 'no nodes';
  return `${picked.length} of ${nodes.length} nodes`;
}

// toggledValue is a multiple value with one node ticked or unticked; every
// node ticked, or none, is "all" ([]).
export function toggledValue(nodes, value, id) {
  const on = new Set(pickedNodes(nodes, value, { multiple: true }).map((n) => n.id));
  if (on.has(id)) on.delete(id); else on.add(id);
  const ids = nodes.filter((n) => on.has(n.id)).map((n) => n.id);
  return ids.length === nodes.length ? [] : ids;
}

function matches(n, q) {
  return !q || n.name.toLowerCase().includes(q) || n.id.toLowerCase().includes(q);
}

// NodeSelect picks nodes. nodes are { id, name, color, local, online };
// value is an array of node ids for multiple ([] is every node) or one id (''
// or null is every node, offered while allowAll). badge(node) and note(node)
// add a count and a status to a node's row, lock(node) a reason it cannot be
// picked; menuLabel names the open list for assistive tech (label by default).
export function NodeSelect({
  nodes, value, onChange, multiple = false, allowAll = true, label = 'Nodes', allLabel = 'All nodes',
  badge = null, note = null, lock = null, id = undefined, title = undefined, disabled = false, menuLabel = '',
}) {
  const root = useRef(null);
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  // A pick the caller ignores (All while already all) still re-renders, so
  // the boxes show the value again rather than the browser's toggle.
  const [, repaint] = useState(0);
  useDismissablePopover(root, open, (why) => {
    setOpen(false);
    if (why !== 'outside') root.current?.querySelector('.node-select-button')?.focus();
  });
  useEffect(() => {
    if (open) root.current?.querySelector('.node-pop-filter input')?.focus();
    else setQuery('');
  }, [open]);
  const picked = new Set(pickedNodes(nodes, value, { multiple }).map((n) => n.id));
  const all = multiple ? !value?.length : !value;
  const q = query.trim().toLowerCase();
  const list = nodes.length >= NODE_SELECT_FILTER_AT ? nodes.filter((n) => matches(n, q)) : nodes;
  const total = badge ? nodes.filter((n) => picked.has(n.id)).reduce((s, n) => s + (badge(n) || 0), 0) : 0;
  const choose = (next) => { onChange(next); repaint((n) => n + 1); if (!multiple) setOpen(false); };
  const row = (n) => {
    const why = lock?.(n) || '';
    const on = picked.has(n.id) && (multiple || !all);
    const count = badge?.(n) || 0;
    const status = note?.(n) || '';
    const offline = !n.local && n.online === false;
    const pick = () => { if (!why) choose(multiple ? toggledValue(nodes, value, n.id) : n.id); };
    return html`<div key=${n.id} class=${`node-pop-row node-select-row${on ? ' sel' : ''}${offline ? ' offline' : ''}${why ? ' locked' : ''}`}
      style=${n.color ? `--nc:${n.color}` : null} data-node-option=${n.id} title=${why || status || undefined}>
      ${multiple
        ? html`<input type="checkbox" checked=${on} disabled=${!!why} aria-label=${`Include ${n.name}`} onChange=${pick} />`
        : html`<input type="radio" name=${id ? `${id}-pick` : undefined} checked=${on} disabled=${!!why} aria-label=${`Show ${n.name}`} onChange=${pick} />`}
      <span class="node-pop-swatch" aria-hidden="true"></span>
      <button type="button" class="node-pop-name" disabled=${!!why} onClick=${pick}>${nodeName(n)}</button>
      ${multiple && !why && html`<button type="button" class="node-pop-only" tabindex="-1" title=${`Show only ${n.name}`} onClick=${() => choose([n.id])}>only</button>`}
      <span class="node-pop-meta">${count ? html`<span class="mailbox-unread">${count > 99 ? '99+' : count}</span>` : null}${status && html`<span class="node-select-note">${status}</span>`}${!status && offline ? 'offline' : ''}</span>
    </div>`;
  };
  return html`<div class="node-select" ref=${root} id=${id} data-node-select=${multiple ? 'multiple' : 'single'}>
    <button type="button" class=${`node-select-button${all ? '' : ' narrowed'}`} aria-haspopup="true" aria-expanded=${open ? 'true' : 'false'}
      disabled=${disabled} title=${title || label} onClick=${() => setOpen(!open)}>
      <span class="node-select-label">${label}</span>
      <span class="node-select-value">${nodeSelectSummary(nodes, value, { multiple, allLabel })}</span>
      ${total ? html`<span class="mailbox-unread">${total > 99 ? '99+' : total}</span>` : null}
      <span class="node-pick-caret" aria-hidden="true">▾</span>
    </button>
    ${open && html`<div class="node-select-menu" role="group" aria-label=${menuLabel || label}>
      ${nodes.length >= NODE_SELECT_FILTER_AT && html`<label class="node-pop-filter"><input type="text" value=${query} placeholder="type to filter"
        aria-label="Filter nodes" onInput=${(e) => setQuery(e.currentTarget.value)} /></label>`}
      ${(multiple || allowAll) && html`<div class=${`node-pop-row node-select-row node-select-all${all ? ' sel' : ''}`} data-node-option="">
        ${multiple
          ? html`<input type="checkbox" checked=${all} aria-label=${allLabel} onChange=${() => choose([])} />`
          : html`<input type="radio" name=${id ? `${id}-pick` : undefined} checked=${all} aria-label=${allLabel} onChange=${() => choose('')} />`}
        <button type="button" class="node-pop-name" onClick=${() => choose(multiple ? [] : '')}>${allLabel}</button>
        <span class="node-pop-meta">${nodes.length}</span>
      </div>`}
      ${list.map(row)}
      ${list.length === 0 && html`<div class="node-pop-foot">No node matches</div>`}
    </div>`}
  </div>`;
}
