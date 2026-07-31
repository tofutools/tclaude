import { Fragment, h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { idTooltip, relTime, shortAgentId } from './helpers.js';
import { actorTitle, AUDIT_COLUMNS, AUDIT_PAGE_SIZES, fmtAuditTime, statusView, targetTitle, verbClass } from './audit-model.js';

const html = htm.bind(h); const REPOLL_MS = 30000;

function Actor({ entry }) {
  if (entry.actor_kind === 'human') return html`<span class="audit-actor human" title="the human operator">operator</span>`;
  if (entry.actor_kind === 'agent') { const id = shortAgentId(entry.actor_agent, entry.actor_conv); return html`<${Fragment}><span class="rowname">${entry.actor_label || '(agent)'}</span>${id && html` <span class="id" title=${idTooltip(entry.actor_agent, entry.actor_conv)}>${id}</span>`}</${Fragment}>`; }
  if (entry.actor_kind === 'system') return html`<span class="audit-actor" title="tclaude system observer">tclaude</span>`;
  return html`<span class="muted" title="caller identity could not be resolved">${entry.actor_label || 'unknown'}</span>`;
}
function Target({ entry }) {
  const id = shortAgentId(entry.target_agent, entry.target_conv);
  if (!entry.group_name && !entry.target_label && !id) return html`<span class="muted">—</span>`;
  return html`<${Fragment}>${entry.group_name && html`<span class="tag">${entry.group_name}</span>`}${entry.target_label && html` <span class="rowname">${entry.target_label}</span>`}${id && html` <span class="id" title=${idTooltip(entry.target_agent, entry.target_conv)}>${id}</span>`}</${Fragment}>`;
}
function prettyAuditValue(value, raw = '') {
  if (raw) return raw;
  if (value === undefined) return '(not captured)';
  try { return JSON.stringify(value, null, 2); } catch (_) { return String(value); }
}
function SpawnDetails({ entry }) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef(null);
  const popoverRef = useRef(null);
  const closeRef = useRef(null);
  const details = entry.spawn;
  const panelID = `audit-spawn-details-${entry.id}`;
  const dismiss = (restoreFocus = true) => {
    setOpen(false);
    if (restoreFocus) triggerRef.current?.focus();
  };
  const closeOnEscape = (event) => {
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); dismiss(); }
  };
  useEffect(() => {
    if (!open || !details) return undefined;
    closeRef.current?.focus();
    const closeOnOutsidePointer = (event) => {
      if (!popoverRef.current?.contains(event.target) && !triggerRef.current?.contains(event.target)) dismiss(false);
    };
    document.addEventListener('pointerdown', closeOnOutsidePointer);
    return () => document.removeEventListener('pointerdown', closeOnOutsidePointer);
  }, [open, details]);
  if (!details) return null;
  return html`<span class="audit-spawn-info">
    <button type="button" class="spawn-field-help-trigger audit-spawn-info-trigger"
      ref=${triggerRef}
      aria-label="Show spawn request, resolved parameters, and response"
      aria-controls=${open ? panelID : undefined} aria-expanded=${open ? 'true' : 'false'} title="Show spawn request, resolved parameters, and response"
      onKeyDown=${closeOnEscape} onClick=${() => open ? dismiss() : setOpen(true)}>?</button>
    ${open && html`<div id=${panelID} ref=${popoverRef} class="audit-spawn-popover" role="dialog" aria-label="Spawn details" tabIndex="-1" onKeyDown=${closeOnEscape}>
      <div class="audit-spawn-popover-head"><strong>Spawn details</strong><button ref=${closeRef} type="button" class="audit-spawn-popover-close" aria-label="Close spawn details" title="Close" onClick=${() => dismiss()}>×</button></div>
      ${details.snapshot_truncated && html`<div class="audit-spawn-popover-note">The request snapshot exceeded the audit size limit; the resolved values and response are retained where possible.</div>`}
      <section><h4>Request input</h4><pre>${prettyAuditValue(details.input, details.input_raw)}</pre></section>
      <section><h4>Resolved parameters and profiles</h4><pre>${prettyAuditValue(details.resolved)}</pre></section>
      <section><h4>Command response${details.response_truncated ? ' (truncated)' : ''}</h4><pre>${prettyAuditValue(details.response, details.response_raw)}</pre></section>
    </div>`}
  </span>`;
}
function Header({ current, state, actions }) {
  const activate = (event, key) => { if (!key || (event.type === 'keydown' && event.key !== 'Enter' && event.key !== ' ' && event.key !== 'Spacebar')) return; event.preventDefault(); state.cycleSort(key); actions.load(); };
  return html`<thead><tr>${AUDIT_COLUMNS.map((column) => { const active = column.key === current.sort; return html`<th key=${column.label} class=${column.key ? `audit-sort${active ? ' active' : ''}` : undefined} tabIndex=${column.key ? 0 : undefined} aria-sort=${column.key ? active ? current.dir === 'asc' ? 'ascending' : 'descending' : 'none' : undefined} title=${column.key ? `Sort by ${column.label}` : undefined} onClick=${(event) => activate(event, column.key)} onKeyDown=${(event) => activate(event, column.key)}>${column.label}${active ? current.dir === 'asc' ? ' ▲' : ' ▼' : ''}</th>`; })}</tr></thead>`;
}
function Rows({ current, state, actions }) {
  if (current.request.phase === 'loading') return html`<div class="empty">Loading audit log…</div>`;
  if (current.request.phase === 'error') return html`<div role="alert" class="island-error">Failed to load audit log: ${current.request.error}</div>`;
  if (!current.rows.length) return html`<div class="empty">${current.totalUnfiltered ? 'No events match the filter.' : 'No audit events recorded yet. Rows are written for coordination commands and managed pane exits.'}</div>`;
  return html`<table class="audit-table"><${Header} current=${current} state=${state} actions=${actions} /><tbody>${current.rows.map((entry) => { const id = shortAgentId(entry.actor_agent, entry.actor_conv); const status = statusView(entry.status); return html`<tr key=${entry.id} data-key=${`audit-${entry.id}`}>
    <td class="audit-nowrap"><span class="last-hook" title=${entry.at}>${fmtAuditTime(entry.at)}</span>${relTime(entry.at) && html` <span class="muted">(${relTime(entry.at)})</span>`}</td>
    <td class="audit-trunc" title=${actorTitle(entry, id)}><${Actor} entry=${entry} /></td>
    <td class="audit-trunc" title=${entry.verb || ''}><span class=${verbClass(entry.verb)}>${entry.verb}</span>${entry.source === 'dashboard' && html` <span class="id" title="run from the dashboard">⊞</span>`}</td>
    <td class="audit-trunc" title=${targetTitle(entry)}><${Target} entry=${entry} /></td>
    <td class="audit-detail"><span class="muted" title=${entry.detail || ''}>${entry.detail || ''}</span><${SpawnDetails} entry=${entry} /></td>
    <td class="audit-nowrap"><span class=${status.className} title=${status.title}>${status.label}</span></td>
  </tr>`; })}</tbody></table>`;
}
function Pager({ current, state, actions }) {
  if (!current.totalUnfiltered) return null; const atStart = current.page <= 1; const atEnd = current.page >= current.pages;
  const go = (value) => { state.setPage(value); actions.load(); };
  return html`<div id="audit-pager" class="audit-pager">${current.pages > 1 && html`<${Fragment}><button title="First page" disabled=${atStart} onClick=${() => go(1)}>«</button><button title="Previous page" disabled=${atStart} onClick=${() => go(current.page - 1)}>‹</button><span class="audit-pager-pos">Page ${current.page} / ${current.pages}</span><button title="Next page" disabled=${atEnd} onClick=${() => go(current.page + 1)}>›</button><button title="Last page" disabled=${atEnd} onClick=${() => go(current.pages)}>»</button></${Fragment}>`}<span class="grow"></span><label class="audit-pager-size" title="Rows per page"><select id="audit-page-size" value=${current.pageSize} onChange=${(event) => { state.setPageSize(event.currentTarget.value); actions.load(); }}>${AUDIT_PAGE_SIZES.map((size) => html`<option key=${size} value=${size}>${size}</option>`)}</select> / page</label></div>`;
}
export function AuditApp({ state, actions }) {
  const current = state.view.value; const searchTimer = useRef(null); const filterRef = useRef(null);
  useEffect(() => {
    if (!current.active) { clearTimeout(searchTimer.current); return undefined; }
    actions.load();
    const snapshot = () => { if (state.refreshDue(REPOLL_MS)) actions.load(); };
    document.addEventListener('tclaude:snapshot', snapshot);
    return () => document.removeEventListener('tclaude:snapshot', snapshot);
  }, [current.active]);
  useEffect(() => {
    const reselected = (event) => { if (event.detail?.tab === 'audit' && state.view.value.active) actions.load(); };
    document.addEventListener('tclaude:tab-reselected', reselected);
    return () => document.removeEventListener('tclaude:tab-reselected', reselected);
  }, []);
  useEffect(() => () => clearTimeout(searchTimer.current), []);
  const filter = (name, value, debounce = false) => { state.setFilter(name, value); clearTimeout(searchTimer.current); if (debounce) searchTimer.current = setTimeout(actions.load, 300); else actions.load(); };
  const retention = current.response ? current.response.pruning_on ? `keeping ${current.response.retention_days} day${current.response.retention_days === 1 ? '' : 's'}` : 'kept forever' : '';
  return html`<${Fragment}><div class="filter-bar"><input ref=${filterRef} id="filter-audit" aria-label="Search audit events" type="text" placeholder="Search (actor / verb / target / group / detail) — server-side" autocomplete="off" spellcheck=${false} value=${current.query} onInput=${(event) => filter('query', event.currentTarget.value, true)} /><span class="filter-count" id="filter-audit-count" aria-live="polite">${current.totalUnfiltered === 0 ? '' : current.total === current.totalUnfiltered ? `${current.total} event${current.total === 1 ? '' : 's'}` : `${current.total} / ${current.totalUnfiltered}`}</span><button class="clear-filter" id="filter-audit-clear" title="Clear search" aria-label="Clear audit search" onClick=${() => { filter('query', ''); filterRef.current?.focus(); }}>×</button><label class="filter-toggle" title="Which audit source or observer to show"><span>source</span><select id="audit-source" value=${current.source} onChange=${(event) => filter('source', event.currentTarget.value)}><option value="">all</option><option value="cli">CLI</option><option value="dashboard">dashboard</option><option value="popup">approval popup</option><option value="tmux">pane bootstrap observer</option><option value="hook">SessionEnd hook</option><option value="reaper">reaper</option><option value="reconcile">startup reconciliation</option></select></label><label class="filter-toggle" title="Which command outcomes to show"><span>outcome</span><select id="audit-outcome" value=${current.outcome} onChange=${(event) => filter('outcome', event.currentTarget.value)}><option value="">all</option><option value="success">success</option><option value="failure">denied / error</option></select></label><span class="spacer"></span><span id="audit-retention" class="muted">${retention}</span></div><div id="audit-list" aria-busy=${current.request.phase === 'loading' || current.request.phase === 'refreshing'}><${Rows} current=${current} state=${state} actions=${actions} /></div><${Pager} current=${current} state=${state} actions=${actions} /></${Fragment}>`;
}
export function mountAuditIsland({ host, state, actions, registerCleanup }) { render(html`<${AuditApp} state=${state} actions=${actions} />`, host); registerCleanup(() => render(null, host)); }
