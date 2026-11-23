// phone-automations-island.js -- the Automations tab on a phone
// (data-layout="phone"): its automations as lists a thumb can work, and the
// process runs it can read and decide.
//
//   chips  one per kind: All, Exports, Cron, Standing orders, Triggers, and
//          Runs in place of the desktop's Processes subtab (the templates and
//          their editor stay on the desktop).
//   list   one row per automation: its name, when it fires, where to, and how
//          it last went, with an on/off switch, and ▶ Run now on a cron job
//          (behind the desktop's confirmation). An export is read-only:
//          download or dismiss.
//   page   tapping a row pushes its page (js/phone-page.js): the details, and
//          its recent runs or firings. Creating and editing are desktop only.
//
// Two islands render it, as on the desktop: the Jobs island (#jobs-root) for
// the kinds, the Processes island (#processes-root) for Runs. The chip row
// switches between them through the hidden nav anchors, so the router keeps
// its single recording path (automations-subnav.js). Every action is the
// desktop's (jobs-actions.js), over the same state (jobs-state.js).

import { h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { PROCESSES_SUBTAB, automationsSubnavEntries, openAutomationsEntry } from './automations-subnav.js';
import { cronRunSucceeded, formatJobInterval } from './jobs-format.js';
import { actionSummary, scopeSummary, triggerState, whenSummary } from './jobs-triggers.js';
import { groupScopeLabel, localStamp } from './cell-format.js';
import { relTime, shortAgentId } from './helpers.js';
import { formatUsageTime } from './usage-history-model.js';
import { pageTriggersReadable } from './feature-availability.js';
import { dashboardState } from './snapshot-store.js';
import { processContext } from './process-context.js';
import { companionAgents } from './companion-model.js';
import { usePhonePage } from './phone-page.js';
import { RunPage, RunsList, gateItems, runRequest } from './phone-process-runs.js';
import { DesktopOnlyNote, desktopSite } from './phone-desktop-only.js';
import { Button, Input, Switch } from './ui-components.js';
import { ChipRow, Empty, ErrorNote, errorText, Section } from './phone-ui.js';
import { Chevron } from './chevron-icon.js';

const html = htm.bind(h);

// Stamp is a time on a phone page as the mock writes it, short and relative
// ("3h ago", "in 16h"), with the full local date and time on hover.
function Stamp({ iso, empty = 'unknown time' }) {
  const full = localStamp(iso, { empty: '' });
  if (!full) return empty;
  const at = new Date(iso).getTime();
  return html`<time datetime=${iso} title=${full}>${at > Date.now() ? formatUsageTime(at) : relTime(iso)}</time>`;
}

const CHIP_LABELS = Object.freeze({
  all: 'All', export: 'Exports', cron: 'Cron', 'standing-order': 'Standing orders', trigger: 'Triggers',
  [PROCESSES_SUBTAB]: 'Runs',
});


// cronSchedule says when a cron job fires, in words where the daemon could
// render them.
export function cronSchedule(job) {
  if (job.cron_expr) return job.cron_desc || `cron ${job.cron_expr}`;
  return `every ${formatJobInterval(job.interval_seconds)}`;
}

// cronTarget names where a cron job sends to.
export function cronTarget(job) {
  if (job.target_kind === 'group') {
    const group = groupScopeLabel(job.group_name, job.group_id);
    return job.target_role ? `${group} (${job.target_role})` : group;
  }
  if (job.target_conv) return job.target_label || shortAgentId(job.target_agent, job.target_conv);
  return 'no target';
}

// cronOutcome is a cron job's last run, in the mock's words: "✓ ran 6h ago",
// "✗ failed 1d ago", coloured by how it went (the desktop's status pill, in
// a line).
export function cronOutcome(job) {
  if (!job.last_run_status) return { text: job.enabled ? 'never run' : 'off · never run', tone: '' };
  const status = job.last_run_status;
  const ok = cronRunSucceeded(status);
  const failed = /denied|failed|error|deadline|feature_disabled|profile_disabled|profile_operator_only/.test(status);
  const when = relTime(job.last_run_at);
  const word = status === 'ok' ? 'ran' : status.replace(/_/g, ' ');
  const text = `${ok ? '✓' : failed ? '✗' : '•'} ${word}${when ? ` ${when}` : ''}`;
  return { text: job.enabled ? text : `off · ${text}`, tone: ok ? 'ok' : failed ? 'bad' : 'warn' };
}

// orderTarget names where a standing order applies.
export function orderTarget(order) {
  const target = order.target || {};
  if (target.kind === 'global') return 'every agent';
  if (target.kind === 'group') {
    const group = groupScopeLabel(target.group_name, target.group_id);
    return target.role ? `${group} (${target.role})` : group;
  }
  if (target.agent) return shortAgentId(target.agent, target.conv);
  return 'no target';
}

// orderOutcome is a standing order's last evaluation, as a line.
export function orderOutcome(order) {
  const evaluation = order.last_evaluation;
  if (!evaluation) return { text: order.enabled ? 'never evaluated' : 'off · never evaluated', tone: '' };
  const outcome = evaluation.outcome || 'unknown';
  const tone = outcome === 'delivered' ? 'ok' : outcome === 'not-evaluated-trimmed' ? 'bad' : evaluation.problem ? 'warn' : '';
  const when = relTime(evaluation.at || order.updated_at);
  const text = `${outcome}${when ? ` · ${when}` : ''}`;
  return { text: order.enabled ? text : `off · ${text}`, tone };
}

// exportOutcome is an export's state, as a line.
export function exportOutcome(job) {
  if (job.status === 'ready') return { text: `✓ ready · ${relTime(job.created_at) || ''}`.replace(/ · $/, ''), tone: 'ok' };
  if (job.status === 'failed') return { text: `✗ failed${job.error ? ` · ${job.error}` : ''}`, tone: 'bad' };
  return { text: `${job.status || 'queued'}…`, tone: 'warn' };
}

// AutomationChips is the kind row, Runs included, shared by both islands.
// The row scrolls sideways; the chip in use is brought into sight whenever
// it changes and whenever its island comes on screen (a hidden island cannot
// scroll), so Runs, last in the row, shows that it is the one picked.
export function AutomationChips({ snapshot, active, onSelect, waiting = 0, shown = true }) {
  const entries = automationsSubnavEntries(snapshot);
  const row = useRef(null);
  useLayoutEffect(() => {
    if (shown) revealChip(row.current?.querySelector('.phone-chips'));
  }, [active, entries.length, shown]);
  return html`<div class="phone-auto-chips" ref=${row}><${ChipRow} label="Automation views" value=${active} onSelect=${onSelect}
    items=${entries.map((key) => ({ key, label: CHIP_LABELS[key], badge: key === PROCESSES_SUBTAB ? waiting : 0 }))} /></div>`;
}

// revealChip scrolls a chip row sideways, and only sideways, so its picked
// chip is in sight.
export function revealChip(strip) {
  const chip = strip?.querySelector?.('[aria-selected="true"]');
  if (!chip) return;
  const box = chip.getBoundingClientRect();
  const left = box.left - strip.getBoundingClientRect().left + strip.scrollLeft;
  const right = left + box.width;
  if (left < strip.scrollLeft) strip.scrollLeft = Math.max(0, left - 12);
  else if (right > strip.scrollLeft + strip.clientWidth) strip.scrollLeft = right - strip.clientWidth + 12;
}

function Line({ outcome }) {
  return html`<span class=${`phone-auto-meta${outcome.tone ? ` ${outcome.tone}` : ''}`}>${outcome.text}</span>`;
}

// OnOff is a row's switch. It shows what was asked at once and settles on
// what the server says when the next snapshot lands; a refused toggle puts it
// back.
function OnOff({ on, label, onToggle }) {
  const [pending, setPending] = useState(null);
  useEffect(() => setPending(null), [on]);
  const shown = pending ?? on;
  return html`<${Switch} class="phone-auto-switch" checked=${shown} aria-label=${`${shown ? 'Turn off' : 'Turn on'} ${label}`}
    onClick=${(event) => event.stopPropagation()}
    onChange=${async (event) => {
      const next = event.currentTarget.checked;
      setPending(next);
      const ok = await onToggle();
      if (!ok) setPending(null);
    }} />`;
}

function Row({ title, sub, outcome, onOpen, children, off = false, rowKey }) {
  return html`<li class=${`phone-auto-row${off ? ' off' : ''}`} data-key=${rowKey}>
    <button data-ui-exempt type="button" class="phone-auto-open" onClick=${onOpen} disabled=${!onOpen}>
      <span class="phone-auto-title">${title}</span>
      ${sub && html`<span class="phone-auto-sub">${sub}</span>`}
      <${Line} outcome=${outcome} />
    </button>
    <span class="phone-auto-controls">${children}</span>
  </li>`;
}

function CronRow({ job, actions, open }) {
  return html`<${Row} rowKey=${`cron-${job.id}`} title=${job.name} off=${!job.enabled}
    sub=${`${cronSchedule(job)} · → ${cronTarget(job)}`} outcome=${cronOutcome(job)} onOpen=${() => open('cron', job)}>
    <button data-ui-exempt type="button" class="phone-auto-run" aria-label=${`Run ${job.name} now`} title="Run now"
      onClick=${() => actions.runCron(job)}>▶</button>
    <${OnOff} on=${!!job.enabled} label=${job.name} onToggle=${() => actions.toggleCron(job)} />
  <//>`;
}

function OrderRow({ order, actions, open }) {
  const when = order.trigger?.label || order.trigger?.event || '';
  return html`<${Row} rowKey=${`standing-order-${order.id}`} title=${order.name} off=${!order.enabled}
    sub=${[when, `→ ${orderTarget(order)}`].filter(Boolean).join(' · ')} outcome=${orderOutcome(order)}
    onOpen=${() => open('standing-order', order)}>
    <${OnOff} on=${!!order.enabled} label=${order.name} onToggle=${() => actions.toggleStandingOrder(order)} />
  <//>`;
}

function ExportRow({ job, actions }) {
  return html`<${Row} rowKey=${`export-${job.id}`} title=${job.title || job.artifact_name || `(${job.preset || 'untitled'})`}
    sub=${job.conv_label || ''} outcome=${exportOutcome(job)}>
    ${job.ready && html`<${Button} size="sm" onClick=${() => actions.downloadExport(job)}>Download<//>`}
    <${Button} size="sm" variant="ghost danger" onClick=${() => actions.dismissExport(job)}>Dismiss<//>
  <//>`;
}

function TriggerRow({ rule, actions, open, onChanged }) {
  const state = triggerState(rule);
  const last = rule.firings?.[0];
  const outcome = last
    ? { text: `${state.label} · ${last.outcome || 'fired'} ${relTime(last.started_at) || ''}`.trim(), tone: last.outcome === 'ok' ? 'ok' : 'warn' }
    : { text: `${state.label} · never fired`, tone: '' };
  return html`<${Row} rowKey=${`trigger-${rule.id}`} title=${rule.name} off=${!rule.enabled}
    sub=${`${whenSummary(rule)} · ${scopeSummary(rule)}`} outcome=${outcome} onOpen=${() => open('trigger', rule)}>
    <${OnOff} on=${!!rule.enabled} label=${rule.name} onToggle=${async () => {
      const ok = await actions.toggleTrigger(rule);
      if (ok) onChanged();
      return ok;
    }} />
  <//>`;
}

// useTriggers reads the trigger rules (their own REST collection, not part of
// the jobs snapshot), again whenever the desktop state says they changed.
function useTriggers(state, actions, enabled) {
  const [rules, setRules] = useState(null);
  const [error, setError] = useState('');
  const [nonce, setNonce] = useState(0);
  const revision = state.triggerRevision.value;
  useEffect(() => {
    if (!enabled) return undefined;
    let live = true;
    actions.loadTriggers().then((list) => { if (live) { setRules(list); setError(''); } })
      .catch((cause) => { if (live) setError(cause?.message || String(cause)); });
    return () => { live = false; };
  }, [enabled, revision, nonce, actions]);
  return { rules, error, reload: () => setNonce((n) => n + 1) };
}

function Facts({ rows }) {
  return html`<dl class="phone-auto-facts">${rows.filter(([, value]) => value).map(([label, value]) => html`
    <dt key=${`${label}-dt`}>${label}</dt><dd key=${`${label}-dd`}>${value}</dd>`)}</dl>`;
}

// History reads a page's recent runs or firings once it opens.
function History({ load, render: renderItem, empty }) {
  const [items, setItems] = useState(null);
  const [error, setError] = useState('');
  const fetchAgain = async () => {
    setError('');
    try { setItems(await load()); } catch (cause) { setError(cause?.message || String(cause)); }
  };
  useEffect(() => { void fetchAgain(); }, []);
  if (error) return html`<${ErrorNote} onRetry=${fetchAgain}>${errorText('its history', error)}<//>`;
  if (!items) return html`<${Empty}>Loading…<//>`;
  if (!items.length) return html`<${Empty}>${empty}<//>`;
  return html`<ul class="phone-auto-history">${items.map(renderItem)}</ul>`;
}

function CronPage({ job, actions }) {
  const sends = job.action_kind === 'spawn'
    ? `spawns ${job.spawn_profile || 'a worker'}${(job.spawn_roles || []).length ? ` as ${job.spawn_roles.join(', ')}` : ''}`
    : [job.subject, (job.body || '').replace(/\s+/g, ' ').trim()].filter(Boolean).join(' — ');
  return html`<div class="phone-auto-page">
    <${Facts} rows=${[
      ['Schedule', job.cron_expr ? `${cronSchedule(job)} (${job.cron_expr})` : cronSchedule(job)],
      ['Target', cronTarget(job)],
      [job.action_kind === 'spawn' ? 'Does' : 'Sends', sends],
      ['Next', job.enabled && job.next_due_at ? html`<${Stamp} iso=${job.next_due_at} />` : ''],
      ['By', job.operator_authored ? 'the operator' : (job.owner_label || shortAgentId(job.owner_agent, job.owner_conv))],
      ['On', html`<${OnOff} on=${!!job.enabled} label=${job.name} onToggle=${() => actions.toggleCron(job)} />`],
    ]} />
    <${Section} title="Recent runs">
      <${History} load=${() => actions.loadCronLogs(job.id)} empty="No runs recorded."
        render=${(run) => {
          const outcome = cronOutcome({ enabled: true, last_run_status: run.status || 'unknown', last_run_at: run.fired_at });
          return html`<li key=${run.id}><span class=${`phone-auto-meta ${outcome.tone}`}><${Stamp} iso=${run.fired_at} /> · ${run.status || 'unknown'}</span>
            ${run.error_msg && html`<span class="phone-auto-log">${run.error_msg}</span>`}</li>`;
        }} />
    <//>
    <${DesktopOnlyNote} what="Editing, duplicating and deleting are" />
  </div>`;
}

function OrderPage({ order, actions }) {
  const evaluation = order.last_evaluation;
  return html`<div class="phone-auto-page">
    <p class="phone-auto-summary">${order.summary}</p>
    <${Facts} rows=${[
      ['When', order.trigger?.label || order.trigger?.event || ''],
      ['Applies to', orderTarget(order)],
      ['Timing', order.timing || 'next-turn'],
      ['Cadence', order.cadence || 'always'],
      ['Last', evaluation ? `${orderOutcome(order).text}${evaluation.detail ? ` — ${evaluation.detail}` : ''}` : 'never evaluated'],
      ['Off because', !order.enabled ? order.disabled_reason : ''],
      ['On', html`<${OnOff} on=${!!order.enabled} label=${order.name} onToggle=${() => actions.toggleStandingOrder(order)} />`],
    ]} />
    <${DesktopOnlyNote} what="Editing and deleting are" />
  </div>`;
}

function TriggerPage({ rule, actions, onChanged }) {
  return html`<div class="phone-auto-page">
    <${Facts} rows=${[
      ['When', whenSummary(rule)],
      ['Where', scopeSummary(rule)],
      ['Then', (rule.actions || []).map(actionSummary).join('; ')],
      ['State', triggerState(rule).label],
      ['On', html`<${OnOff} on=${!!rule.enabled} label=${rule.name} onToggle=${async () => {
        const ok = await actions.toggleTrigger(rule);
        if (ok) onChanged();
        return ok;
      }} />`],
    ]} />
    <${Section} title="Recent firings">
      <${History} load=${() => actions.loadTriggerFirings(rule.id)} empty="Never fired."
        render=${(firing) => html`<li key=${firing.id}><span class=${`phone-auto-meta ${firing.outcome === 'ok' ? 'ok' : 'warn'}`}><${Stamp} iso=${firing.started_at} /> · ${firing.outcome || 'fired'}</span>
          ${firing.detail && html`<span class="phone-auto-log">${firing.detail}</span>`}</li>`} />
    <//>
    <${DesktopOnlyNote} what="Editing and deleting are" />
  </div>`;
}

// openedItem finds the open page's automation in what is listed now, so its
// switch and last run stay current; one that has left the list (a filter, a
// page turn) keeps showing as it was last seen.
export function openedItem(opened, rows, rules) {
  if (!opened) return null;
  const { kind, item } = opened;
  if (kind === 'trigger') return (rules || []).find((rule) => rule.id === item.id) || item;
  const field = kind === 'cron' ? 'cron' : 'order';
  return rows.find((row) => row.kind === kind && row[field]?.id === item.id)?.[field] || item;
}

export function PhoneJobsApp({ state, actions, activeTab = dashboardState.activeTab, context = processContext }) {
  const current = state.view.value;
  const snapshot = current.dashboard;
  const triggersReady = pageTriggersReadable(snapshot);
  const kind = snapshot != null && !triggersReady && current.kind === 'trigger' ? 'all' : current.kind;
  const active = activeTab.value === 'jobs';
  const [opened, setOpened] = useState(null);
  const triggers = useTriggers(state, actions, active && kind === 'trigger' && triggersReady);
  const refreshTimer = useRef(null);
  useEffect(() => () => clearTimeout(refreshTimer.current), []);
  useEffect(() => { if (!active) setOpened(null); }, [active]);

  const select = (key) => {
    setOpened(null);
    if (key === PROCESSES_SUBTAB) { openAutomationsEntry(key); return; }
    if (state.setKind(key)) void actions.refresh();
    document.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: state.location.value } }));
  };
  const onQuery = (value) => {
    state.setQuery(value);
    clearTimeout(refreshTimer.current);
    refreshTimer.current = setTimeout(() => void actions.refresh(), 250);
  };
  const open = (pageKind, item) => {
    globalThis.scrollTo?.(0, 0);
    setOpened({ kind: pageKind, item });
  };
  const item = openedItem(opened, current.rows, triggers.rules);
  usePhonePage(item ? {
    title: item.name,
    onBack: () => setOpened(null),
    actions: opened.kind === 'cron' ? [{ key: 'run', icon: '▶ Run now', label: `Run ${item.name} now`, onClick: () => actions.runCron(item) }] : [],
  } : null, active);

  if (item) {
    if (opened.kind === 'cron') return html`<${CronPage} job=${item} actions=${actions} />`;
    if (opened.kind === 'standing-order') return html`<${OrderPage} order=${item} actions=${actions} />`;
    return html`<${TriggerPage} rule=${item} actions=${actions} onChanged=${triggers.reload} />`;
  }

  const waiting = gateItems(context.context.value).length;
  let body;
  if (kind === 'trigger') {
    if (triggers.error) body = html`<${ErrorNote} onRetry=${triggers.reload}>${errorText('the triggers', triggers.error)}<//>`;
    else if (!triggers.rules) body = html`<${Empty}>Loading triggers…<//>`;
    else if (!triggers.rules.length) body = html`<${Empty}>No triggers.<//>`;
    else {
      body = html`<ul class="phone-list phone-auto-list">${triggers.rules.map((rule) => html`<${TriggerRow} key=${rule.id}
        rule=${rule} actions=${actions} open=${open} onChanged=${triggers.reload} />`)}</ul>`;
    }
  } else if (current.request.phase === 'error' && !current.request.hasLoaded) {
    body = html`<${ErrorNote} onRetry=${() => void actions.refresh()}>${errorText('the automations', current.request.error)}<//>`;
  } else if (!current.request.hasLoaded) {
    body = html`<${Empty}>Loading…<//>`;
  } else if (!current.rows.length) {
    body = html`<${Empty}>${current.query ? 'No automations match.' : 'Nothing here yet.'}<//>`;
  } else {
    body = html`<ul class="phone-list phone-auto-list">${current.rows.map((row) => (row.kind === 'cron'
      ? html`<${CronRow} key=${`cron-${row.cron?.id}`} job=${row.cron || {}} actions=${actions} open=${open} />`
      : row.kind === 'standing-order'
        ? html`<${OrderRow} key=${`standing-order-${row.order?.id}`} order=${row.order || {}} actions=${actions} open=${open} />`
        : html`<${ExportRow} key=${`export-${row.export?.id}`} job=${row.export || {}} actions=${actions} />`))}</ul>`;
  }
  const paging = current.paging;
  const total = paging.total || 0;
  const offset = paging.offset || 0;
  const size = state.limit.value;
  const turn = (action) => { if (state.page(action, total)) void actions.refresh(); };
  return html`<div class="phone-auto">
    <${AutomationChips} snapshot=${snapshot} active=${kind} onSelect=${select} waiting=${waiting} shown=${active} />
    ${kind !== 'trigger' && html`<div class="phone-auto-tools">
      <${Input} type="search" class="phone-auto-search" aria-label="Filter automations" placeholder="Filter"
        value=${current.query} onInput=${(event) => onQuery(event.currentTarget.value)} />
    </div>`}
    ${body}
    ${kind !== 'trigger' && total > size && html`<div class="phone-auto-pager">
      <${Button} size="sm" disabled=${offset === 0} onClick=${() => turn('prev')}><${Chevron} dir="left" />Previous<//>
      <span>${offset + 1}–${Math.min(total, offset + size)} of ${total}</span>
      <${Button} size="sm" disabled=${offset + size >= total} onClick=${() => turn('next')}>Next<${Chevron} dir="right" /><//>
    </div>`}
    <${DesktopOnlyNote} what="Creating and editing automations is" />
  </div>`;
}

export function mountPhoneJobsIsland({ host, state, actions, registerCleanup }) {
  state.initialize();
  const restore = (event) => {
    const loc = event.detail?.location;
    if (loc?.tab === 'jobs' && state.applyLocation(loc)) void actions.refresh();
  };
  document.addEventListener('tclaude:restore-location', restore);
  registerCleanup(() => document.removeEventListener('tclaude:restore-location', restore));
  render(html`<${PhoneJobsApp} state=${state} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}

export function PhoneRunsApp({ activeTab = dashboardState.activeTab, snapshot = dashboardState.snapshot, context = processContext }) {
  const active = activeTab.value === 'processes';
  const [runId, setRunId] = useState('');
  useEffect(() => { if (!active) setRunId(''); }, [active]);
  // A run link (openRunLink) opens its run, or the list.
  const asked = runRequest.value;
  useEffect(() => {
    if (asked === null || !active) return;
    runRequest.value = null;
    setRunId(asked);
  }, [asked, active]);
  const select = (key) => {
    setRunId('');
    if (key !== PROCESSES_SUBTAB) openAutomationsEntry(key);
  };
  if (runId) {
    const agents = companionAgents(snapshot.value);
    return html`<${RunPage} runId=${runId} active=${active} back=${() => setRunId('')} source=${context}
      nameOf=${(id) => agents.get(id)?.name || ''} />`;
  }
  return html`<div class="phone-auto">
    <${AutomationChips} snapshot=${snapshot.value} active=${PROCESSES_SUBTAB} onSelect=${select}
      waiting=${gateItems(context.context.value).length} shown=${active} />
    <${RunsList} source=${context} desktopSite=${desktopSite} nameOf=${(id) => companionAgents(snapshot.value).get(id)?.name || ''} onOpen=${(id) => { globalThis.scrollTo?.(0, 0); setRunId(id); }} />
  </div>`;
}

export function mountPhoneProcessesIsland({ host, registerCleanup }) {
  render(html`<${PhoneRunsApp} />`, host);
  registerCleanup(() => render(null, host));
}
