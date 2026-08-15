import { Fragment, h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { JOBS_COLS } from './sort.js';
import { JOBS_KINDS, JOBS_PAGE_SIZES, jobsLocation } from './jobs-state.js';
import { toPath } from './nav-history-core.js';
import { formatJobInterval } from './jobs-format.js';
import { EXPORT_STEPS, activeExportStepIndex, fmtBytes } from './export-progress.js';
import { idTooltip, isModifiedClick, relTime, shortAgentId } from './helpers.js';
import { AsyncLoadState } from './async-load-state.js';
import { JobsCronDialogRoot } from './jobs-dialog-island.js';
import { JobsStandingOrderDialogRoot } from './jobs-standing-order-dialog-island.js';
import { TriggerDialogRoot, TriggerWorkspace } from './jobs-triggers.js';
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
  const immediate = job.run_immediately
    ? html`<div class="muted" title="This job opted into an immediate run on create or when the setting was turned on">immediate opt-in</div>`
    : null;
  const offline = job.queue_when_offline
    ? html`<div class="muted" title="Scheduled messages are retained when recipients are offline">queues offline</div>`
    : null;
  if (job.cron_expr) {
    return html`<${Fragment}><span class="id" title=${job.cron_desc || ''}>cron: ${job.cron_expr}</span>${immediate}${offline}</${Fragment}>`;
  }
  return html`<${Fragment}><span class="id">every ${formatJobInterval(job.interval_seconds)}</span>${immediate}${offline}</${Fragment}>`;
}

function CronStatus({ status }) {
  if (!status) return html`<span class="state-pill state-offline" title="never run">never run</span>`;
  const failed = /denied|failed|error|deadline|feature_disabled/.test(status);
  const cls = ['ok', 'spawned', 'replace_stopped'].includes(status)
    ? 'state-working'
    : failed ? 'state-error' : 'state-awaiting';
  return html`<span class=${`state-pill ${cls}`} title=${status}>${status}</span>`;
}

function CronActionSummary({ job }) {
  if (job.action_kind !== 'spawn') return html`<span class="cron-action-kind message">✉ message</span>`;
  const roles = (job.spawn_roles || []).join(', ');
  const policy = job.spawn_concurrency_policy || 'Forbid';
  return html`<${Fragment}>
    <span class="cron-action-kind spawn">⚡ spawn</span>
    <div class="muted cron-spawn-summary">${job.spawn_profile || 'profile missing'}
      ${roles ? ` · roles ${roles}` : ''} · ${policy}
      ${policy === 'Allow' ? ` up to ${job.spawn_max_live_workers || 1}` : ''}
    </div>
  </${Fragment}>`;
}

function CronRunInspector({ id, runs, loading, error, onRetry }) {
  return html`<div class="cron-run-inspector" id=${id}>
    <div class="cron-run-inspector-head"><strong>Recent runs</strong>
      <span class="muted">Recorded scheduler outcomes; worker identity appears when a spawn reached a managed worker.</span></div>
    ${error && html`<div class="jobs-error" role="alert">${error}<button type="button" onClick=${onRetry}>retry</button></div>`}
    ${loading ? html`<div class="muted">Loading run history…</div>` : !error && runs.length === 0
      ? html`<div class="muted">No runs recorded.</div>`
      : html`<div class="cron-runs">${runs.map((run) => html`<div class="cron-run" key=${run.id}>
          <span>${run.fired_at ? new Date(run.fired_at).toLocaleString() : 'unknown time'}</span>
          <${CronStatus} status=${run.status || 'unknown'} />
          <span class="muted cron-run-detail">${run.error_msg || ''}</span>
          <span class="cron-run-worker">${run.worker_agent
            ? html`<code title=${run.worker_agent}>${shortAgentId(run.worker_agent, '')}</code>`
            : html`<span class="muted">no worker</span>`}
            ${run.worker_id ? html`<span class="muted"> · worker #${run.worker_id}</span>` : null}</span>
        </div>`)}</div>`}
  </div>`;
}

function CronRow({ job, actions, triggersEnabled }) {
  const [expanded, setExpanded] = useState(false);
  const [runs, setRuns] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const isSpawn = job.action_kind === 'spawn';
  const actionSummary = isSpawn ? job.spawn_instruction_template : job.body;
  const bodySummary = (actionSummary || '').replace(/\s+/g, ' ').trim();
  const editBlocked = isSpawn && !triggersEnabled;
  const loadRuns = async () => {
    setLoading(true); setError('');
    try { setRuns(await actions.loadCronLogs(job.id)); }
    catch (err) { setError(err?.message || String(err)); }
    finally { setLoading(false); }
  };
  const toggleRuns = () => {
    const next = !expanded;
    setExpanded(next);
    if (next && runs == null && !loading) void loadRuns();
  };
  return html`<${Fragment}>
  <tr data-key=${`cron-${job.id}`} class=${expanded ? 'cron-row expanded' : 'cron-row'}>
    <td>${job.enabled
      ? html`<span class="online" title="enabled">●</span>`
      : html`<span class="offline" title="disabled">○</span>`}</td>
    <td><span class="tag">⏰ cron</span></td>
    <td class="id">${job.id}</td>
    <td title=${bodySummary}>
      <div class="rowname">${job.name}</div>
      ${job.subject && html`<div class="muted">${job.subject}</div>`}
      <${CronActionSummary} job=${job} />
      ${editBlocked && html`<div class="cron-feature-note">features.triggers is off · editing unavailable</div>`}
    </td>
    <td>
      <${CronTarget} job=${job} />
      <div class="muted" title=${job.operator_authored ? 'Fires as the human operator (recipients see “the human operator”)' : idTooltip(job.owner_agent, job.owner_conv)}>
        by ${job.operator_authored ? 'the operator' : (job.owner_label || shortAgentId(job.owner_agent, job.owner_conv))}
      </div>
    </td>
    <td><${CronStatus} status=${job.last_run_status} /></td>
    <td><span class="last-hook">${relTime(job.last_run_at) || '—'}</span></td>
    <td><${CronSchedule} job=${job} /></td>
    <td><div class="row-actions">
      <button onClick=${() => actions.runCron(job)} title="Fire this job immediately (also stamps last_run_at)">run now</button>
      <button onClick=${toggleRuns} aria-expanded=${expanded ? 'true' : 'false'} aria-controls=${`cron-run-inspector-${job.id}`}
        title="Show recorded run outcomes">${expanded ? 'hide logs' : 'logs'}</button>
      <button disabled=${editBlocked} onClick=${() => actions.openCronEdit(job)}
        title=${editBlocked ? 'Enable features.triggers to edit this spawn job' : 'Edit this cron job'}>edit</button>
      <button disabled=${editBlocked} onClick=${() => actions.openCronDuplicate(job)}
        title=${editBlocked ? 'Enable features.triggers to duplicate this spawn job' : 'Duplicate this cron job'}>duplicate</button>
      <button class=${job.enabled ? 'warn' : ''} onClick=${() => actions.toggleCron(job)}
        title=${job.enabled ? 'Pause this cron job' : 'Re-enable this cron job'}>
        ${job.enabled ? 'disable' : 'enable'}
      </button>
      <button class="danger" onClick=${() => actions.deleteCron(job)} title="Delete this cron job">delete</button>
    </div></td>
  </tr>
  ${expanded && html`<tr class="cron-run-inspector-row"><td colspan="9">
    <${CronRunInspector} id=${`cron-run-inspector-${job.id}`} runs=${runs || []} loading=${loading} error=${error} onRetry=${loadRuns} />
  </td></tr>`}
  </${Fragment}>`;
}

function StandingOrderTarget({ order }) {
  const target = order.target || {};
  if (target.kind === 'global') {
    return html`<${Fragment}>
      <span class="tag">global</span>
      ${(order.additional_groups || []).map((group) => html`
        <div class="muted" key=${group.group_id}>also group:${group.group_name || ('#' + group.group_id)}</div>
      `)}
    </${Fragment}>`;
  }
  if (target.kind === 'group') {
    return html`<${Fragment}>
      <span class="tag">group:${target.group_name || ('#' + target.group_id)}</span>
      ${target.role && html`<div class="muted">role:${target.role}</div>`}
      ${(order.additional_groups || []).map((group) => html`
        <div class="muted" key=${group.group_id}>also group:${group.group_name || ('#' + group.group_id)}</div>
      `)}
    </${Fragment}>`;
  }
  if (target.agent) {
    return html`<${Fragment}>
      <span class="rowname" title=${idTooltip(target.agent, target.conv)}>
        ${shortAgentId(target.agent, target.conv)}
      </span>
      ${(order.additional_groups || []).map((group) => html`
        <div class="muted" key=${group.group_id}>also group:${group.group_name || ('#' + group.group_id)}</div>
      `)}
    </${Fragment}>`;
  }
  return html`<span class="muted">(no target)</span>`;
}

function StandingOrderOutcome({ evaluation }) {
  if (!evaluation) return html`<span class="state-pill state-offline" title="never evaluated">never evaluated</span>`;
  const outcome = evaluation.outcome || 'unknown';
  const cls = outcome === 'delivered'
    ? 'state-working'
    : outcome === 'not-evaluated-trimmed'
      ? 'state-error'
      : evaluation.problem
        ? 'state-awaiting'
        : 'state-offline';
  return html`<${Fragment}>
    <span class=${`state-pill ${cls}`} title=${evaluation.detail || outcome}>${outcome}</span>
    ${evaluation.detail && html`<div class="muted order-evaluation-detail" title=${evaluation.detail}>${evaluation.detail}</div>`}
  </${Fragment}>`;
}

function StandingOrderCapability({ capability }) {
  if (!capability) {
    return html`<${Fragment}>
      <span class="state-pill state-offline" title="Target harnesses could not be resolved">unknown</span>
      <div class="muted">target capability</div>
    </${Fragment}>`;
  }
  const value = capability;
  const status = value.status;
  const cls = status === 'supported' ? 'state-working' : status === 'degraded' ? 'state-awaiting' : 'state-error';
  return html`<${Fragment}>
    <span class=${`state-pill ${cls}`} title=${value.detail || status}>${status}</span>
    <div class="muted">${value.transport || 'none'}</div>
  </${Fragment}>`;
}

function StandingOrderRow({ order, actions }) {
  const summary = (order.summary || '').replace(/\s+/g, ' ').trim();
  return html`<tr data-key=${`standing-order-${order.id}`}>
    <td>${order.enabled
      ? html`<span class="online" title="enabled">●</span>`
      : html`<span class="offline" title=${order.disabled_reason || 'disabled'}>○</span>`}</td>
    <td><span class="tag">📌 order</span></td>
    <td class="id">${order.id}</td>
    <td title=${summary}>
      <div class="rowname">${order.name}</div>
      <div class="muted">${summary}</div>
      <div class="muted">revision ${order.revision || 1} · ${order.cadence || 'always'}
        ${Number(order.cooldown_seconds) > 0 ? ` · ${order.cooldown_seconds}s minimum interval` : ''}
        ${Number(order.debounce_seconds) > 0 ? ` · ${order.debounce_seconds}s trailing-edge debounce` : ''}</div>
    </td>
    <td>
      <${StandingOrderTarget} order=${order} />
      <div class="muted" title=${order.operator_authored ? 'Authored by the human operator' : idTooltip(order.owner_agent, order.owner_conv)}>
        by ${order.operator_authored ? 'the operator' : shortAgentId(order.owner_agent, order.owner_conv)}
      </div>
    </td>
    <td><${StandingOrderOutcome} evaluation=${order.last_evaluation} /></td>
    <td><span class="last-hook">${relTime(order.last_evaluation?.at || order.updated_at) || '—'}</span></td>
    <td>
      <div class="rowname">${order.trigger?.label || order.trigger?.event || '—'}</div>
      <div class="muted">requires ${order.timing || 'next-turn'}</div>
      <${StandingOrderCapability} capability=${order.capability} />
    </td>
    <td><div class="row-actions">
      <button onClick=${() => actions.openStandingOrderEdit(order)}
        title="Edit this standing order">edit</button>
      <button class=${order.enabled ? 'warn' : ''} onClick=${() => actions.toggleStandingOrder(order)}
        title=${order.enabled
          ? 'Disable this standing order'
          : order.disabled_reason
            ? `Re-enable and clear automatic marker: ${order.disabled_reason}`
            : 'Re-enable this standing order'}>
        ${order.enabled ? 'disable' : 'enable'}
      </button>
      <button class="danger" onClick=${() => actions.deleteStandingOrder(order)}
        title="Delete this standing order and its evaluation history">delete</button>
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

function EmptyJobs({ kind }) {
  if (kind === 'standing-order') {
    return html`<div class="empty">No standing orders yet. Use <strong>+ new standing order</strong>
      above to add a session-boundary reminder.</div>`;
  }
  if (kind === 'cron') {
    return html`<div class="empty">No cron jobs yet. Use <strong>
      <span class="cron-open-label-regular">+ new cron job</span>
      <span class="cron-open-label-wizard">⏳ Bind a recurring ritual</span></strong> above.
    </div>`;
  }
  if (kind === 'export') {
    return html`<div class="empty">No export jobs yet. Start one from an agent row's ⚙ menu →
      <strong>📋 summary…</strong>.
    </div>`;
  }
  return html`<div class="empty">No exports, cron jobs, or standing orders yet. Agent exports appear here
    when started (an agent row's ⚙ menu → <strong>📋 summary…</strong>); create a cron job with the <strong>
    <span class="cron-open-label-regular">+ new cron job</span>
    <span class="cron-open-label-wizard">⏳ Bind a recurring ritual</span></strong> button above.
  </div>`;
}

const JOB_KIND_LABELS = {
  all: 'All',
  'export': 'Exports',
  cron: 'Cron jobs',
  'standing-order': 'Standing orders',
  trigger: 'Triggers',
};
const JOB_KIND_COUNT_LABELS = {
  'export': ['export', 'exports'],
  cron: ['cron job', 'cron jobs'],
  'standing-order': ['standing order', 'standing orders'],
};

export function JobsApp({ state, actions }) {
  const current = state.view.value;
  const triggersKnown = current.dashboard != null;
  const triggersEnabled = current.dashboard?.triggers_enabled === true;
  const visibleKinds = !triggersKnown || triggersEnabled
    ? JOBS_KINDS : JOBS_KINDS.filter((kind) => kind !== 'trigger');
  const displayKind = triggersKnown && !triggersEnabled && current.kind === 'trigger' ? 'all' : current.kind;
  const inputRef = useRef(null);
  const refreshTimer = useRef(null);
  useEffect(() => () => clearTimeout(refreshTimer.current), []);
  useEffect(() => {
    if (!triggersKnown || triggersEnabled || current.kind !== 'trigger') return;
    if (state.setKind('all')) void actions.refresh();
    document.dispatchEvent(new CustomEvent('tclaude:navigated', {
      detail: { location: state.location.value },
    }));
  }, [triggersKnown, triggersEnabled, current.kind]);

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
  const count = displayKind === 'trigger' ? '' : current.query
    ? `${total} / ${totalAll}`
    : `${totalAll} ${displayKind === 'all'
      ? `item${totalAll === 1 ? '' : 's'}`
      : JOB_KIND_COUNT_LABELS[displayKind][totalAll === 1 ? 0 : 1]}`;

  const activateKind = (value) => {
    if (state.setKind(value)) void actions.refresh();
    document.dispatchEvent(new CustomEvent('tclaude:navigated', {
      detail: { location: state.location.value },
    }));
  };
  const selectKind = (event, value) => {
    if (isModifiedClick(event)) return;
    event.preventDefault();
    activateKind(value);
  };
  const keyDownKind = (event, value) => {
    if (event.key !== ' ' && event.key !== 'Spacebar') return;
    event.preventDefault();
    activateKind(value);
  };

  return html`<div class="jobs-island">
    <div class="jobs-subnav" role="tablist" aria-label="Automation views">
      ${visibleKinds.map((kind) => html`<a href=${toPath(jobsLocation(kind))}
        class=${`jobs-subtab${displayKind === kind ? ' active' : ''}`}
        role="tab" aria-selected=${displayKind === kind ? 'true' : 'false'}
        onClick=${(event) => selectKind(event, kind)}
        onKeyDown=${(event) => keyDownKind(event, kind)}>${JOB_KIND_LABELS[kind]}</a>`)}
    </div>
    ${displayKind !== 'trigger' && html`<div class="filter-bar">
      <input ref=${inputRef} id="filter-jobs" type="text" aria-label="Filter automations"
        placeholder="Filter this view (name + agent/owner/target + subject + body + status)"
        autocomplete="off" spellcheck=${false} value=${current.query}
        onInput=${(event) => onQuery(event.currentTarget.value)} />
      <span class="filter-count" id="filter-jobs-count" aria-live="polite">${count}</span>
      <button class="clear-filter" id="filter-jobs-clear" title="Clear filter" aria-label="Clear automation filter"
        onClick=${() => { onQuery(''); inputRef.current?.focus(); }}>×</button>
      <span class="spacer"></span>
      ${(displayKind === 'all' || displayKind === 'cron') && html`
        <button id="cron-create-open" class="primary" title="Schedule a new recurring cron job"
          onClick=${() => actions.openCronCreate({})}>
          <span class="cron-open-label-regular">+ new cron job</span>
          <span class="cron-open-label-wizard">⏳ Bind a recurring ritual</span>
        </button>`}
      ${(displayKind === 'all' || displayKind === 'standing-order') && html`
        <button id="standing-order-create-open" class="primary"
          title="Create a standing instruction triggered at session boundaries"
          onClick=${() => actions.openStandingOrderCreate({})}>+ new standing order</button>`}
    </div>`}
    ${displayKind === 'trigger' ? html`<${TriggerWorkspace} state=${state} actions=${actions} />` : html`<${Fragment}>
    <${AsyncLoadState} label="Automations" request=${current.request}
      retry=${() => void actions.refresh()} errorClass="jobs-error" />
    <div id="jobs-list" aria-busy=${current.request.phase === 'loading' ? 'true' : 'false'}>
      ${!current.request.hasLoaded
        ? null
        : current.rows.length === 0
          ? html`<${EmptyJobs} kind=${displayKind} />`
          : html`<${Fragment}>
            <table>
              <${SortHead} active=${current.sort} onSort=${(col) => state.cycleSort(col)} />
              <tbody>${current.rows.map((row) => row.kind === 'cron'
                ? html`<${CronRow} key=${`cron-${row.cron?.id}`} job=${row.cron || {}} actions=${actions}
                    triggersEnabled=${triggersEnabled} />`
                : row.kind === 'standing-order'
                  ? html`<${StandingOrderRow} key=${`standing-order-${row.order?.id}`}
                    order=${row.order || {}} actions=${actions} />`
                  : html`<${ExportRow} key=${`export-${row.export?.id}`} job=${row.export || {}} actions=${actions} />`
              )}</tbody>
            </table>
            <${Pager} state=${state} paging=${paging} refresh=${actions.refresh}
              disabled=${(paging.offset || 0) !== state.offset.value} />
          </${Fragment}>`}
    </div></${Fragment}>`}
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
  const restore = (event) => {
    const loc = event.detail?.location;
    if (loc?.tab === 'jobs' && state.applyLocation(loc)) void actions.refresh();
  };
  document.addEventListener('tclaude:restore-location', restore);
  registerCleanup(() => document.removeEventListener('tclaude:restore-location', restore));
  const controller = {
    openCreate: state.openCronCreate,
    openEdit: state.openCronEdit,
    openDuplicate: state.openCronDuplicate,
    openStandingOrderCreate: state.openStandingOrderCreate,
  };
  let unregister = null;
  let cleaned = false;
  const cleanup = () => {
    if (cleaned) return;
    const failures = [];
    const attempt = (step) => { try { step(); } catch (error) { failures.push(error); } };
    attempt(() => { unregister?.(); unregister = null; });
    attempt(() => state.closeCronDialog());
    attempt(() => state.closeStandingOrderDialog());
    attempt(() => state.closeTriggerDialog());
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
    render(html`<${Fragment}>
      <${JobsCronDialogRoot} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>
      <${JobsStandingOrderDialogRoot} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>
      <${TriggerDialogRoot} state=${state} actions=${actions}/>
    </${Fragment}>` , dialogHost);
    registerCleanup(cleanup);
  } catch (error) {
    try { cleanup(); } catch (cleanupError) {
      throw new AggregateError([error, cleanupError], 'Jobs initialization failed');
    }
    throw error;
  }
}
