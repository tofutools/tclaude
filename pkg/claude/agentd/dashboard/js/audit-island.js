import { Fragment, h, render } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { idTooltip, relTime, shortAgentId } from './helpers.js';
import { actorTitle, AUDIT_COLUMNS, AUDIT_PAGE_SIZES, fmtAuditTime, statusView, targetTitle, verbClass } from './audit-model.js';

const html = htm.bind(h); const REPOLL_MS = 30000;

function Actor({ entry }) {
  if (entry.actor_kind === 'human') return html`<span class="audit-actor human" title="the human operator">operator</span>`;
  if (entry.actor_kind === 'agent') { const id = shortAgentId(entry.actor_agent, entry.actor_conv); return html`<${Fragment}><span class="rowname">${entry.actor_label || '(agent)'}</span>${id && html` <span class="id" title=${idTooltip(entry.actor_agent, entry.actor_conv)}>${id}</span>`}</${Fragment}>`; }
  return html`<span class="muted" title="caller identity could not be resolved">${entry.actor_label || 'unknown'}</span>`;
}
function Target({ entry }) {
  if (!entry.group_name && !entry.target_label) return html`<span class="muted">—</span>`;
  return html`<${Fragment}>${entry.group_name && html`<span class="tag">${entry.group_name}</span>`}${entry.target_label && html` <span class="rowname">${entry.target_label}</span>`}</${Fragment}>`;
}
function Header({ current, state, actions }) {
  const activate = (event, key) => { if (!key || (event.type === 'keydown' && event.key !== 'Enter' && event.key !== ' ' && event.key !== 'Spacebar')) return; event.preventDefault(); state.cycleSort(key); actions.load(); };
  return html`<thead><tr>${AUDIT_COLUMNS.map((column) => { const active = column.key === current.sort; return html`<th key=${column.label} class=${column.key ? `audit-sort${active ? ' active' : ''}` : undefined} tabIndex=${column.key ? 0 : undefined} aria-sort=${column.key ? active ? current.dir === 'asc' ? 'ascending' : 'descending' : 'none' : undefined} title=${column.key ? `Sort by ${column.label}` : undefined} onClick=${(event) => activate(event, column.key)} onKeyDown=${(event) => activate(event, column.key)}>${column.label}${active ? current.dir === 'asc' ? ' ▲' : ' ▼' : ''}</th>`; })}</tr></thead>`;
}
function Rows({ current, state, actions }) {
  if (current.request.phase === 'loading') return html`<div class="empty">Loading audit log…</div>`;
  if (current.request.phase === 'error') return html`<div role="alert" class="island-error">Failed to load audit log: ${current.request.error}</div>`;
  if (!current.rows.length) return html`<div class="empty">${current.totalUnfiltered ? 'No events match the filter.' : 'No commands recorded yet. Audit rows are written as agents and the operator run tclaude commands (spawn, message, lifecycle, permissions…).'}</div>`;
  return html`<table class="audit-table"><${Header} current=${current} state=${state} actions=${actions} /><tbody>${current.rows.map((entry) => { const id = shortAgentId(entry.actor_agent, entry.actor_conv); const status = statusView(entry.status); return html`<tr key=${entry.id} data-key=${`audit-${entry.id}`}>
    <td class="audit-nowrap"><span class="last-hook" title=${entry.at}>${fmtAuditTime(entry.at)}</span>${relTime(entry.at) && html` <span class="muted">(${relTime(entry.at)})</span>`}</td>
    <td class="audit-trunc" title=${actorTitle(entry, id)}><${Actor} entry=${entry} /></td>
    <td class="audit-trunc" title=${entry.verb || ''}><span class=${verbClass(entry.verb)}>${entry.verb}</span>${entry.source === 'dashboard' && html` <span class="id" title="run from the dashboard">⊞</span>`}</td>
    <td class="audit-trunc" title=${targetTitle(entry)}><${Target} entry=${entry} /></td>
    <td class="audit-detail"><span class="muted" title=${entry.detail || ''}>${entry.detail || ''}</span></td>
    <td class="audit-nowrap"><span class=${status.className} title=${status.title}>${status.label}</span></td>
  </tr>`; })}</tbody></table>`;
}
function Pager({ current, state, actions }) {
  if (!current.totalUnfiltered) return null; const atStart = current.page <= 1; const atEnd = current.page >= current.pages;
  const go = (value) => { state.setPage(value); actions.load(); };
  return html`<div id="audit-pager" class="audit-pager">${current.pages > 1 && html`<${Fragment}><button title="First page" disabled=${atStart} onClick=${() => go(1)}>«</button><button title="Previous page" disabled=${atStart} onClick=${() => go(current.page - 1)}>‹</button><span class="audit-pager-pos">Page ${current.page} / ${current.pages}</span><button title="Next page" disabled=${atEnd} onClick=${() => go(current.page + 1)}>›</button><button title="Last page" disabled=${atEnd} onClick=${() => go(current.pages)}>»</button></${Fragment}>`}<span class="grow"></span><label class="audit-pager-size" title="Rows per page"><select id="audit-page-size" value=${current.pageSize} onChange=${(event) => { state.setPageSize(event.currentTarget.value); actions.load(); }}>${AUDIT_PAGE_SIZES.map((size) => html`<option key=${size} value=${size}>${size}</option>`)}</select> / page</label></div>`;
}
export function AuditApp({ state, actions }) {
  const current = state.view.value; const searchTimer = useRef(null);
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
  return html`<${Fragment}><div class="filter-bar"><input id="filter-audit" aria-label="Search audit events" type="text" placeholder="Search (actor / verb / target / group / detail) — server-side" autocomplete="off" spellcheck=${false} value=${current.query} onInput=${(event) => filter('query', event.currentTarget.value, true)} /><span class="filter-count" id="filter-audit-count">${current.totalUnfiltered === 0 ? '' : current.total === current.totalUnfiltered ? `${current.total} event${current.total === 1 ? '' : 's'}` : `${current.total} / ${current.totalUnfiltered}`}</span><button class="clear-filter" id="filter-audit-clear" title="Clear search" onClick=${() => filter('query', '')}>×</button><label class="filter-toggle" title="Which command outcomes to show"><span>outcome</span><select id="audit-outcome" value=${current.outcome} onChange=${(event) => filter('outcome', event.currentTarget.value)}><option value="">all</option><option value="success">success</option><option value="failure">denied / error</option></select></label><label class="filter-toggle" title="Which surface the command came in on"><span>source</span><select id="audit-source" value=${current.source} onChange=${(event) => filter('source', event.currentTarget.value)}><option value="">all</option><option value="cli">CLI</option><option value="dashboard">dashboard</option><option value="popup">approval popup</option></select></label><span class="spacer"></span><span id="audit-retention" class="muted">${retention}</span></div><div id="audit-list" aria-busy=${current.request.phase === 'loading' || current.request.phase === 'refreshing'}><${Rows} current=${current} state=${state} actions=${actions} /></div><${Pager} current=${current} state=${state} actions=${actions} /></${Fragment}>`;
}
export function mountAuditIsland({ host, state, actions, registerCleanup }) { render(html`<${AuditApp} state=${state} actions=${actions} />`, host); registerCleanup(() => render(null, host)); }
