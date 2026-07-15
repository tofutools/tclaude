import { h, render } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { registerLinksController } from './links-controller.js';
import { relTime } from './helpers.js';
import { LINK_COLS } from './sort.js';

const html = htm.bind(h);

function Words({ plain, wizard }) {
  return html`<span class="theme-copy-regular">${plain}</span><span class="theme-copy-wizard">${wizard}</span>`;
}

function errorMessage(error) { return error?.message || String(error); }

function ErrorBanner({ error }) {
  if (!error) return html`<div class="cron-create-error" id="link-modal-error" role="alert"></div>`;
  return html`<div class="cron-create-error" id="link-modal-error" role="alert">${error}</div>`;
}

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

export function LinksControls({ state, actions }) {
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
      onClick=${() => actions.openCreate()}
      title="Add a new inter-group communication link">
      <${Words} plain="+ new link" wizard="+ weave channel" />
    </button>
  `;
}

export function LinksList({ state, actions }) {
  const current = state.view.value;
  const [deleting, setDeleting] = useState('');
  const remove = async (link) => {
    const key = String(link.id);
    if (deleting) return;
    setDeleting(key);
    try { await actions.deleteLink({ ...link, scope: link.from || link.to }); }
    finally { setDeleting(''); }
  };
  if (!current.rows.length) {
    return html`<div class="empty">
      <span class="theme-copy-regular">No inter-group links yet. Create one with the <strong>+ new link</strong> button above.</span>
      <span class="theme-copy-wizard">No arcane channels yet. Weave one with the <strong>+ weave channel</strong> button above.</span>
    </div>`;
  }
  return html`
    <table aria-busy=${!!deleting}>
      <thead><tr>${LINK_COLS.map((column, index) => html`
        <${SortHeader} key=${column.col || `plain-${index}`} column=${column} state=${state} />
      `)}</tr></thead>
      <tbody>${current.rows.map((link) => {
        const rowBusy = deleting === String(link.id);
        return html`
          <tr key=${String(link.id)} data-key=${`link-${link.id}`}>
            <td class="id">${link.id}</td>
            <td><span class="rowname">${link.from || '(deleted)'}</span></td>
            <td class="muted">→</td>
            <td><span class="rowname">${link.to || '(deleted)'}</span></td>
            <td><span class="id">${link.mode}</span></td>
            <td><span class="muted">${relTime(link.created_at) || ''}</span></td>
            <td><div class="row-actions">
              <button type="button" disabled=${!!deleting}
                onClick=${() => actions.openEdit({ id: link.id, from: link.from || '', to: link.to || '', mode: link.mode || '' })}
                title="Change this link's mode"><${Words} plain="edit" wizard="rebind" /></button>
              <button type="button" class="danger" disabled=${!!deleting}
                onClick=${() => remove(link)}
                title="Remove this link"><${Words} plain=${rowBusy ? 'deleting…' : 'delete'} wizard=${rowBusy ? 'severing…' : 'sever'} /></button>
            </div></td>
          </tr>
        `;
      })}</tbody>
    </table>
  `;
}

function LinksManager({ state, actions, confirmDiscard }) {
  return html`
    <${Overlay}
      id="links-manage-modal"
      manage=${true}
      labelledby="links-manage-title"
      onClose=${actions.closeManager}
      confirmDiscard=${confirmDiscard}
    >
      <h3 id="links-manage-title"><${Words} plain="Inter-group links" wizard="Arcane channels between parties" /></h3>
      <p class="manage-intro"><${Words}
        plain="Communication edges between groups: members of FROM may message members of TO. Direction matters — add a reverse edge for two-way reach."
        wizard="Missive channels between parties: familiars of FROM may whisper to familiars of TO. Direction matters — weave a reverse channel for two-way reach."
      /></p>
      <div class="filter-bar" id="links-filter-root"><${LinksControls} state=${state} actions=${actions} /></div>
      <div id="links-list"><${LinksList} state=${state} actions=${actions} /></div>
      <div class="modal-buttons">
        <span class="spacer"></span>
        <button id="links-manage-close" type="button" onClick=${actions.closeManager}><${Words} plain="Close" wizard="Dispel" /></button>
      </div>
    </${Overlay}>
  `;
}

function optionsFor(groups, ...selected) {
  return [...new Set([...(groups || []), ...selected.filter(Boolean)])];
}

function LinkEditor({ descriptor, groups, actions, confirmDiscard }) {
  const edit = descriptor.kind === 'edit';
  const preset = edit ? descriptor : descriptor.preset;
  const initialFrom = preset.from || groups[0] || '';
  const initialTo = preset.to || groups.find((name) => name !== initialFrom) || '';
  const initialMode = preset.linkMode || 'members->members';
  const [from, setFrom] = useState(initialFrom);
  const [to, setTo] = useState(initialTo);
  const [mode, setMode] = useState(initialMode);
  const [bidir, setBidir] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const dirty = from !== initialFrom || to !== initialTo || mode !== initialMode || bidir;
  const groupOptions = optionsFor(groups, from, to);

  const submit = async () => {
    if (busy) return;
    setError('');
    if (!edit && (!from || !to)) {
      setError('from and to are required');
      return;
    }
    if (!edit && from === to) {
      setError('from and to must differ — use group membership for intra-group comm');
      return;
    }
    setBusy(true);
    try {
      if (edit) await actions.updateLink({ id: descriptor.id, from, to, mode });
      else await actions.createLink({ from, to, mode, bidir });
    } catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  };

  return html`
    <${Overlay}
      id="link-modal"
      labelledby="link-modal-title"
      onClose=${actions.closeEditor}
      onSubmitHotkey=${submit}
      dirty=${dirty}
      blocked=${busy}
      confirmDiscard=${confirmDiscard}
    >
      <h3 id="link-modal-title"><${Words}
        plain=${edit ? 'Edit link mode' : 'Add inter-group link'}
        wizard=${edit ? 'Rebind an arcane channel' : 'Weave an arcane channel'}
      /></h3>
      ${edit && html`<div class="modal-meta" id="link-modal-meta">#${descriptor.id} · ${from} → ${to}</div>`}
      <label class="cron-create-row">
        <span class="cron-create-label">From</span>
        <select id="link-modal-from" value=${from} disabled=${busy || edit || !!descriptor.preset?.from}
          onChange=${(event) => setFrom(event.currentTarget.value)}>
          ${groupOptions.map((name) => html`<option key=${name} value=${name}>${name}</option>`)}
        </select>
      </label>
      <label class="cron-create-row">
        <span class="cron-create-label">To</span>
        <select id="link-modal-to" value=${to} disabled=${busy || edit}
          onChange=${(event) => setTo(event.currentTarget.value)}>
          ${groupOptions.map((name) => html`<option key=${name} value=${name}>${name}</option>`)}
        </select>
      </label>
      <label class="cron-create-row">
        <span class="cron-create-label">Mode</span>
        <select id="link-modal-mode" value=${mode} disabled=${busy}
          onChange=${(event) => setMode(event.currentTarget.value)}>
          <option value="members->members">members → members (any member of FROM may message any member of TO)</option>
          <option value="owners->members">owners → members (only owners of FROM may message members of TO)</option>
        </select>
      </label>
      ${!edit && html`
        <label class="cron-create-enabled" id="link-modal-bidir-row" title="Also create the reverse link (TO → FROM) with the same mode in one call.">
          <input id="link-modal-bidir" type="checkbox" checked=${bidir} disabled=${busy}
            onChange=${(event) => setBidir(event.currentTarget.checked)} />
          <${Words} plain="Also create reverse link (TO → FROM)" wizard="Also weave a reverse channel (TO → FROM)" />
        </label>
      `}
      <${ErrorBanner} error=${error} />
      <div class="modal-buttons">
        <button id="link-modal-cancel" type="button" disabled=${busy} onClick=${actions.closeEditor}><${Words} plain="Cancel" wizard="Dispel" /></button>
        <span class="spacer"></span>
        <button id="link-modal-submit" class="primary" type="button" disabled=${busy} onClick=${submit}>
          <${Words}
            plain=${busy ? (edit ? 'Saving…' : 'Creating…') : (edit ? 'Save changes' : 'Create link')}
            wizard=${busy ? (edit ? 'Rebinding…' : 'Weaving…') : (edit ? 'Rebind channel' : 'Weave channel')}
          />
        </button>
      </div>
    </${Overlay}>
  `;
}

export function LinksApp({ state, actions, confirmDiscard }) {
  const current = state.view.value;
  return html`
    ${current.managerOpen && html`<${LinksManager} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} />`}
    ${current.editor && html`<${LinkEditor} key=${current.editor.key} descriptor=${current.editor} groups=${current.groups} actions=${actions} confirmDiscard=${confirmDiscard} />`}
  `;
}

export function mountLinksIsland({ host, state, actions, confirmDiscard, registerCleanup }) {
  if (!host) throw new TypeError('links island requires a host');
  if (!state?.view) throw new TypeError('links island requires state');
  if (!actions) throw new TypeError('links island requires actions');
  if (typeof confirmDiscard !== 'function') throw new TypeError('links island requires confirmDiscard');
  if (typeof registerCleanup !== 'function') throw new TypeError('links island requires registerCleanup');
  state.initialize();
  render(html`<${LinksApp} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} />`, host);
  const unregister = registerLinksController(actions);
  registerCleanup(() => { unregister(); render(null, host); });
}
