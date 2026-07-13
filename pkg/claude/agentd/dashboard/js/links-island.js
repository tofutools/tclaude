import { h, render } from 'preact';
import { useRef } from 'preact/hooks';
import htm from 'htm';
import { relTime } from './helpers.js';
import { LINK_COLS } from './sort.js';

const html = htm.bind(h);

function SortHeader({ column, state }) {
  if (!column.col) return html`<th>${column.label || ''}</th>`;
  const current = state.sort.value;
  const active = current?.col === column.col;
  const arrow = active ? (current.dir === 'asc' ? '▲' : '▼') : '▾';
  return html`
    <th class=${active ? 'sortable sort-active' : 'sortable'}
      data-sort-table="links" data-sort-col=${column.col}
      title=${`Sort by ${column.label}`}
      onClick=${() => state.cycleSort(column.col)}>
      ${column.label}<span class="sort-arrow">${arrow}</span>
    </th>
  `;
}

export function LinksControls({ state }) {
  const inputRef = useRef(null);
  const current = state.view.value;
  const regularCount = current.query
    ? `${current.filtered} / ${current.total}`
    : `${current.total} link${current.total === 1 ? '' : 's'}`;
  const wizardCount = current.query
    ? `${current.filtered} / ${current.total}`
    : `${current.total} channel${current.total === 1 ? '' : 's'}`;
  return html`
    <input ref=${inputRef} id="filter-links" type="text"
      aria-label="Filter inter-group links" placeholder="Filter (from / to / mode)"
      autocomplete="off" spellcheck=${false} value=${current.query}
      onInput=${(event) => state.setQuery(event.currentTarget.value)} />
    <span class="filter-count" id="filter-links-count" aria-live="polite">
      <span class="theme-copy-regular">${regularCount}</span>
      <span class="theme-copy-wizard">${wizardCount}</span>
    </span>
    <button class="clear-filter" id="filter-links-clear" title="Clear filter"
      aria-label="Clear link filter" onClick=${() => {
        state.setQuery('');
        inputRef.current?.focus();
      }}>×</button>
    <span class="spacer"></span>
    <button id="link-new-open" class="primary"
      title="Add a new inter-group communication link">
      <span class="theme-copy-regular">+ new link</span>
      <span class="theme-copy-wizard">+ weave channel</span>
    </button>
  `;
}

export function LinksList({ state }) {
  const current = state.view.value;
  if (!current.rows.length) {
    return html`<div class="empty">
      <span class="theme-copy-regular">No inter-group links yet. Create one with the <strong>+ new link</strong> button above.</span>
      <span class="theme-copy-wizard">No arcane channels yet. Weave one with the <strong>+ weave channel</strong> button above.</span>
    </div>`;
  }
  return html`
    <table>
      <thead><tr>${LINK_COLS.map((column, index) => html`
        <${SortHeader} key=${column.col || `plain-${index}`} column=${column} state=${state} />
      `)}</tr></thead>
      <tbody>${current.rows.map((link) => html`
        <tr key=${String(link.id)} data-key=${`link-${link.id}`}>
          <td class="id">${link.id}</td>
          <td><span class="rowname">${link.from || '(deleted)'}</span></td>
          <td class="muted">→</td>
          <td><span class="rowname">${link.to || '(deleted)'}</span></td>
          <td><span class="id">${link.mode}</span></td>
          <td><span class="muted">${relTime(link.created_at) || ''}</span></td>
          <td><div class="row-actions">
            <button data-act="link-edit" data-id=${link.id} data-from=${link.from || ''}
              data-to=${link.to || ''} data-mode=${link.mode || ''}
              title="Change this link's mode"><span class="theme-copy-regular">edit</span><span class="theme-copy-wizard">rebind</span></button>
            <button class="danger" data-act="link-delete" data-id=${link.id}
              data-group=${link.from || ''} data-from=${link.from || ''} data-to=${link.to || ''}
              title="Remove this link"><span class="theme-copy-regular">delete</span><span class="theme-copy-wizard">sever</span></button>
          </div></td>
        </tr>
      `)}</tbody>
    </table>
  `;
}

export function mountLinksIsland({ filterHost, listHost, state, registerCleanup }) {
  if (!filterHost || !listHost) throw new TypeError('links island requires filter and list hosts');
  if (!state?.view) throw new TypeError('links island requires state');
  if (typeof registerCleanup !== 'function') throw new TypeError('links island requires registerCleanup');
  state.initialize();
  registerCleanup(() => render(null, listHost));
  registerCleanup(() => render(null, filterHost));
  render(html`<${LinksControls} state=${state} />`, filterHost);
  render(html`<${LinksList} state=${state} />`, listHost);
}
