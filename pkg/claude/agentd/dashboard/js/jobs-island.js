import { Fragment, h, render } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { JOBS_COLS } from './sort.js';
import { JOBS_PAGE_SIZES } from './jobs-state.js';
import { formatJobInterval } from './jobs-format.js';
import { EXPORT_STEPS, activeExportStepIndex, fmtBytes } from './export-progress.js';
import { idTooltip, relTime, shortAgentId } from './helpers.js';
import { AsyncLoadState } from './async-load-state.js';
import { JobsCronDialogRoot } from './jobs-dialog-island.js';
import { registerJobsController } from './jobs-controller.js';

const html = htm.bind(h);

function ExportSpinner() {
  return html`<span class="export-spinner" style=${`animation-delay:-${Date.now() % 800}ms`} aria-hidden="true"></span>`;
}

function ExportStepper({ status }) {
  const active = activeExportStepIndex(status);
  return html`<span class="export-stepper">
    ${EXPORT_STEPS.map((step, index) => html`
      ${index > 0 && html`<span class="export-chip-sep" aria-hidden="true">→</span>`}
      <span class=${`export-chip ${index < active ? 'done' : index === active ? 'active' : 'pending'}`}>
        ${index < active && '✓ '}${index === active && html`<${ExportSpinner} />`}${step.short}
      </span>
    `)}
  </span>`;
}

function CronTarget({ job }) {
  if (job.target_kind === 'group') {
    return html`<span class="tag">group:${job.group_name || ('#' + job.group_id)}</span>`;
  }
  if (job.target_conv) {
    return html`<${Fragment}>
      <span class="rowname" title=${idTooltip(job.target_agent, job.target_conv)}>
        ${shortAgentId(job.target_agent, job.target_conv)}
      </span>
      ${job.target_label && html`<div class="muted">${job.target_label}</div>`}
    </${Fragment}>`;
  }
  return html`<span class="muted">(no target)</span>`;
}

function CronSchedule({ job }) {
  if (job.cron_expr) {
    return html`<span class="id" title=${job.cron_desc || ''}>cron: ${job.cron_expr}</span>`;
  }
  return html`<span class="id">every ${formatJobInterval(job.interval_seconds)}</span>`;
}

function CronStatus({ status }) {
  if (!status) return html`<span class="state-pill state-offline" title="never run">never run</span>`;
  const cls = status === 'ok' ? 'state-working' : 'state-awaiting';
  return html`<span class=${`state-pill ${cls}`} title=${status}>${status}</span>`;
}

function CronRow({ job, actions }) {
  const bodySummary = (job.body || '').replace(/\s+/g, ' ').trim();
  return html`<tr data-key=${`cron-${job.id}`}>
    <td>${job.enabled
      ? html`<span class="online" title="enabled">●</span>`
      : html`<span class="offline" title="disabled">○</span>`}</td>
    <td><span class="tag">⏰ cron</span></td>
    <td class="id">${job.id}</td>
    <td title=${bodySummary}>
      <div class="rowname">${job.name}</div>
      ${job.subject && html`<div class="muted">${job.subject}</div>`}
    </td>
    <td>
      <${CronTarget} job=${job} />
      <div class="muted" title=${idTooltip(job.owner_agent, job.owner_conv)}>
        by ${job.owner_label || shortAgentId(job.owner_agent, job.owner_conv)}
      </div>
    </td>
    <td><${CronStatus} status=${job.last_run_status} /></td>
    <td><span class="last-hook">${relTime(job.last_run_at) || '—'}</span></td>
    <td><${CronSchedule} job=${job} /></td>
    <td><div class="row-actions">
      <button onClick=${() => actions.runCron(job)} title="Fire this job immediately (also stamps last_run_at)">run now</button>
      <button onClick=${() => actions.openCronEdit(job)} title="Edit this cron job">edit</button>
      <button onClick=${() => actions.openCronDuplicate(job)} title="Duplicate this cron job">duplicate</button>
      <button class=${job.enabled ? 'warn' : ''} onClick=${() => actions.toggleCron(job)}
        title=${job.enabled ? 'Pause this cron job' : 'Re-enable this cron job'}>
        ${job.enabled ? 'disable' : 'enable'}
      </button>
      <button class="danger" onClick=${() => actions.deleteCron(job)} title="Delete this cron job">delete</button>
    </div></td>
  </tr>`;
}

function ExportName({ job }) {
  const name = job.title || job.artifact_name;
  if (!name) return html`<span class="muted">(${job.preset || 'untitled'})</span>`;
  return html`<${Fragment}>
    <div class="rowname">${name}</div>
    ${job.title && job.artifact_name && html`<div class="muted">${job.artifact_name}</div>`}
  </${Fragment}>`;
}

function ExportStatus({ job }) {
  if (job.status === 'ready') return html`<span class="ej-status ready">✓ ready</span>`;
  if (job.status === 'failed') {
    return html`<${Fragment}>
      <span class="ej-status failed">✗ failed</span>
      ${job.error && html`<div class="ej-error" title=${job.error}>${job.error}</div>`}
    </${Fragment}>`;
  }
  return html`<${ExportStepper} status=${job.status} />`;
}

function ExportRow({ job, actions }) {
  const settled = job.status === 'ready' || job.status === 'failed';
  return html`<tr data-key=${`export-${job.id}`}>
    <td>${!settled
      ? html`<span class="online" title="in flight">◐</span>`
      : job.status === 'ready'
        ? html`<span class="online" title="ready">●</span>`
        : html`<span class="offline" title="failed">○</span>`}</td>
    <td><span class="tag">📋 export</span></td>
    <td class="id">${job.id}</td>
    <td><${ExportName} job=${job} /></td>
    <td><span class="rowname" title=${job.conv_id || ''}>${job.conv_label || '(unknown)'}</span></td>
    <td><${ExportStatus} job=${job} /></td>
    <td><span class="last-hook">${relTime(job.created_at) || '—'}</span></td>
    <td>${job.artifact_size ? fmtBytes(job.artifact_size) : html`<span class="muted">—</span>`}</td>
    <td><div class="row-actions">
      ${job.ready && html`<button onClick=${() => actions.downloadExport(job)} title="Download this export">⤓ download</button>`}
      <button class="danger" onClick=${() => actions.dismissExport(job)}
        title="Dismiss — removes this export job from the list and deletes its file (if one was delivered)">dismiss</button>
    </div></td>
  </tr>`;
}

function SortHead({ active, onSort }) {
  const activate = (event, col) => {
    if (event.type === 'keydown' && event.key !== 'Enter' && event.key !== ' ' && event.key !== 'Spacebar') return;
    event.preventDefault();
    onSort(col);
  };
  return html`<thead><tr>${JOBS_COLS.map((column) => {
    if (!column.col) return html`<th>${column.label || ''}</th>`;
    const selected = active?.col === column.col;
    const arrow = selected ? (active.dir === 'asc' ? '▲' : '▼') : '▾';
    return html`<th class=${selected ? 'sortable sort-active' : 'sortable'}
      tabIndex="0" aria-sort=${selected ? (active.dir === 'asc' ? 'ascending' : 'descending') : 'none'}
      title=${`Sort by ${column.label}`} onClick=${(event) => activate(event, column.col)}
      onKeyDown=${(event) => activate(event, column.col)}>
      ${column.label}<span class="sort-arrow">${arrow}</span>
    </th>`;
  })}</tr></thead>`;
}

function Pager({ state, paging, refresh, disabled = false }) {
  const total = paging.total || 0;
  const size = state.limit.value;
  const off = paging.offset || 0;
  if (total <= size && off === 0) return null;
  const from = total === 0 ? 0 : off + 1;
  const to = Math.min(off + size, total);
  const atFirst = off <= 0;
  const atLast = off + size >= total;
  const move = (action) => {
    if (state.page(action, total)) void refresh();
  };
  const button = (action, glyph, title, atBoundary) => html`
    <button type="button" class="list-pager-btn" disabled=${disabled || atBoundary} title=${title}
      aria-label=${title} onClick=${() => move(action)}>${glyph}</button>`;
  return html`<div class="list-pager">
    ${button('first', '«', 'First page', atFirst)}
    ${button('prev', '‹', 'Previous page', atFirst)}
    <span class="list-pager-count">${from}–${to} of ${total}</span>
    ${button('next', '›', 'Next page', atLast)}
    ${button('last', '»', 'Last page', atLast)}
    <select class="list-pager-size" title="Rows per page" aria-label="Rows per page" disabled=${disabled}
      value=${size} onChange=${(event) => { state.setPageSize(event.currentTarget.value); void refresh(); }}>
      ${JOBS_PAGE_SIZES.map((value) => html`<option value=${value}>${value}/page</option>`)}
    </select>
  </div>`;
}

function EmptyJobs() {
  return html`<div class="empty">No jobs yet. Agent exports appear here when started (an agent row's ⚙ menu →
    <strong>📋 summary…</strong>); schedule a cron job with the <strong>
    <span class="cron-open-label-regular">+ new cron job</span>
    <span class="cron-open-label-wizard">⏳ Bind a recurring ritual</span></strong> button above.
  </div>`;
}

export function JobsApp({ state, actions }) {
  const current = state.view.value;
  const inputRef = useRef(null);
  const refreshTimer = useRef(null);
  useEffect(() => () => clearTimeout(refreshTimer.current), []);

  const queueRefresh = () => {
    clearTimeout(refreshTimer.current);
    refreshTimer.current = setTimeout(() => void actions.refresh(), 250);
  };
  const onQuery = (value) => {
    state.setQuery(value);
    queueRefresh();
  };
  const paging = current.paging;
  const total = paging.total || 0;
  const totalAll = paging.total_unfiltered || 0;
  const count = current.query
    ? `${total} / ${totalAll}`
    : `${totalAll} job${totalAll === 1 ? '' : 's'}`;

  return html`<div class="jobs-island">
    <div class="filter-bar">
      <input ref=${inputRef} id="filter-jobs" type="text" aria-label="Filter jobs"
        placeholder="Filter (kind + name + agent/owner/target + subject + body + status)"
        autocomplete="off" spellcheck=${false} value=${current.query}
        onInput=${(event) => onQuery(event.currentTarget.value)} />
      <span class="filter-count" id="filter-jobs-count" aria-live="polite">${count}</span>
      <button class="clear-filter" id="filter-jobs-clear" title="Clear filter" aria-label="Clear job filter"
        onClick=${() => { onQuery(''); inputRef.current?.focus(); }}>×</button>
      <span class="spacer"></span>
      <button id="cron-create-open" class="primary" title="Schedule a new recurring cron job"
        onClick=${() => actions.openCronCreate({})}>
        <span class="cron-open-label-regular">+ new cron job</span>
        <span class="cron-open-label-wizard">⏳ Bind a recurring ritual</span>
      </button>
    </div>
    <${AsyncLoadState} label="Jobs" request=${current.request}
      retry=${() => void actions.refresh()} errorClass="jobs-error" />
    <div id="jobs-list" aria-busy=${current.request.phase === 'loading' ? 'true' : 'false'}>
      ${!current.request.hasLoaded
        ? null
        : current.rows.length === 0
          ? html`<${EmptyJobs} />`
          : html`<${Fragment}>
            <table>
              <${SortHead} active=${current.sort} onSort=${(col) => state.cycleSort(col)} />
              <tbody>${current.rows.map((row) => row.kind === 'cron'
                ? html`<${CronRow} key=${`cron-${row.cron?.id}`} job=${row.cron || {}} actions=${actions} />`
                : html`<${ExportRow} key=${`export-${row.export?.id}`} job=${row.export || {}} actions=${actions} />`
              )}</tbody>
            </table>
            <${Pager} state=${state} paging=${paging} refresh=${actions.refresh}
              disabled=${(paging.offset || 0) !== state.offset.value} />
          </${Fragment}>`}
    </div>
  </div>`;
}

export function JobsBadge({ state }) {
  const count = state.view.value.activeExports;
  return html`<span id="jobs-badge" class="tab-badge count" hidden=${count === 0}>${count}</span>`;
}

export function mountJobsIsland({
  host, badgeHost, dialogHost, state, actions, confirmDiscard, registerCleanup,
}) {
  state.initialize();
  const controller = {
    openCreate: state.openCronCreate,
    openEdit: state.openCronEdit,
    openDuplicate: state.openCronDuplicate,
  };
  let unregister = null;
  let cleaned = false;
  const cleanup = () => {
    if (cleaned) return;
    const failures = [];
    const attempt = (step) => { try { step(); } catch (error) { failures.push(error); } };
    attempt(() => { unregister?.(); unregister = null; });
    attempt(() => state.closeCronDialog());
    attempt(() => render(null, dialogHost));
    attempt(() => render(null, badgeHost));
    attempt(() => render(null, host));
    if (failures.length) throw new AggregateError(failures, 'Jobs cleanup failed');
    cleaned = true;
  };
  try {
    unregister = registerJobsController(controller);
    render(html`<${JobsApp} state=${state} actions=${actions} />`, host);
    render(html`<${JobsBadge} state=${state} />`, badgeHost);
    render(html`<${JobsCronDialogRoot} state=${state} actions=${actions}
      confirmDiscard=${confirmDiscard}/>` , dialogHost);
    registerCleanup(cleanup);
  } catch (error) {
    try { cleanup(); } catch (cleanupError) {
      throw new AggregateError([error, cleanupError], 'Jobs initialization failed');
    }
    throw error;
  }
}
