import { h, render } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { isModifiedClick, shortAgentId } from './helpers.js';
import { isWizardActive } from './slop.js';
import { COST_SPANS, dayKey, fmtUSD } from './costs-model.js';
import {
  formatUsageDuration, formatUsageResetCountdown, formatUsageTime, usageForecastOf, usageForecastView,
  usageProviderLabel, usageSeriesKeyOf, usageWindowScopeLabel,
} from './usage-history-model.js';
import { usageLocation, withCostsPeriod } from './usage-hub-state.js';
import { Sparkline, SpendChart, topCostAgents } from './usage-overview-charts.js';
import { FusedUsageSummary, usePeerUsages } from './usage-nodes.js';
import { fleetState } from './fleet-state.js';
import { tickedNodes } from './fleet-scope.js';
import { dashboardState } from './snapshot-store.js';
import { openUsageRulesManager } from './modal-usage-rules.js';
import { barTone, fmtAmount, periodLabel } from './usage-rules-model.js';

export { topCostAgents };

const html = htm.bind(h);

// Same repaint-on-the-wizard-edge hook the Quota island uses (its copy lands
// in attributes and SVG text, where the two-span CSS trick cannot reach).
function useWizardTheme() {
  const [wizard, setWizard] = useState(isWizardActive());
  useEffect(() => {
    const onWizard = (event) => setWizard(
      event.detail?.active == null ? isWizardActive() : Boolean(event.detail.active),
    );
    document.addEventListener('tclaude:wizard', onWizard);
    return () => document.removeEventListener('tclaude:wizard', onWizard);
  }, []);
  return wizard;
}

const SUBVIEW_LABELS = {
  overview: ['Overview', 'Overview'],
  quota: ['Quota', 'Reserves'],
  costs: ['Costs', 'Coffers'],
};

// ---- Pure derivations (exported for tests) --------------------------------

// Statuses whose forecast says the window runs out before it resets (or has
// already): the ones an operator should look at first.
const AT_RISK = new Set(['before_reset', 'projected', 'limit']);

// quotaSummaries reduces each quota series to what a compact card shows: the
// latest included sample, the forecast the series' own Prediction choice
// selects, and its rendered status line.
export function quotaSummaries(quota, wizard = false) {
  const payload = quota?.payload;
  if (!payload) return [];
  const now = new Date(payload.generated_at).getTime();
  return (quota.series || []).map((series) => {
    const key = usageSeriesKeyOf(series);
    const points = (series.points || []).filter((point) => !point.excluded);
    const latest = points[points.length - 1] || null;
    const forecast = usageForecastOf(series, quota.spanFor?.(key)?.algo);
    return {
      key, series, latest, points, forecast, now,
      status: forecast?.status || '',
      atRisk: AT_RISK.has(forecast?.status),
      view: usageForecastView(forecast, now, latest?.at, wizard),
      reset: latest?.resets_at ? formatUsageResetCountdown(latest.resets_at, now, wizard) : '',
    };
  });
}

// nextQuotaWall is the soonest predicted limit among the at-risk windows; a
// window already at its limit wins outright.
export function nextQuotaWall(summaries) {
  let best = null;
  for (const item of summaries) {
    if (!item.atRisk) continue;
    const at = item.status === 'limit' ? -Infinity : new Date(item.forecast?.hits_limit_at).getTime();
    if (!Number.isFinite(at) && at !== -Infinity) continue;
    if (!best || at < best.at) best = { at, item };
  }
  return best?.item || null;
}

export function spanLabel(costs) {
  if (costs.span === 'calmonth') return costs.monthLabel;
  return COST_SPANS.find((span) => span.key === costs.span)?.label || 'This month';
}

// todaySpend reads today's slice out of the shown span; null when the span
// does not include today (an earlier calendar month).
export function todaySpend(costs, now = new Date()) {
  const key = dayKey(now);
  const day = (costs.narrowed?.days || []).find((item) => item.day === key);
  if (!day) return null;
  const top = topCostAgents((costs.rows || []).filter((row) => row.day === key), Infinity);
  return { cost: day.cost_usd || 0, agents: top.length, top: top[0] || null };
}

// gapBeforeReset is how long a window predicted to hit its limit before it
// resets stays without quota; '' when the forecast does not say.
export function gapBeforeReset(forecast) {
  if (forecast?.status !== 'before_reset') return '';
  const gap = new Date(forecast.reset_at).getTime() - new Date(forecast.hits_limit_at).getTime();
  return Number.isFinite(gap) && gap > 0 ? formatUsageDuration(gap) : '';
}

// quotaStatusLine is the compact card's one-line prediction. The full
// sentence (and its rate) stays on the Quota view.
export function quotaStatusLine(item, wizard = false) {
  const w = (plain, wizardly) => (wizard ? wizardly : plain);
  const forecast = item.forecast;
  if (item.status === 'limit') return w('⚠ limit reached', '⚠ the reserves are spent');
  if (item.status === 'before_reset' || item.status === 'projected') {
    const gap = gapBeforeReset(forecast);
    return `⚠ ${w('limit', 'dry')} ${formatUsageTime(forecast.hits_limit_at, item.now)}${gap ? ` · ${gap} ${w('without quota', 'without mana')}` : ''}`;
  }
  if (item.status === 'after_reset' || item.status === 'flat') return `✓ ${w('resets before the limit', 'the reserves hold')}`;
  return item.view.headline.replace(/^(Prediction|Prophecy): /, '');
}

// ---- Views -----------------------------------------------------------------

function QuotaCard({ item, wizard, onOpen }) {
  const { series, latest } = item;
  const tone = item.atRisk ? 'danger' : item.view.tone;
  return html`<a class=${`usage-ov-card usage-ov-quota${item.atRisk ? ' at-risk' : ''}`} href="/usage/quota"
    data-series=${item.key} onClick=${onOpen}>
    <div class="usage-ov-card-head"
      title=${`${usageProviderLabel(series.provider)} ${usageWindowScopeLabel(series, wizard)} — open the full forecast`}>
      <div><span class="usage-provider">${usageProviderLabel(series.provider)}</span>
        <strong class="usage-ov-window">${usageWindowScopeLabel(series, wizard)}</strong></div>
      <span class="usage-ov-pct">${latest ? `${latest.pct.toFixed(0)}%` : '—'}</span>
    </div>
    <span class="usage-ov-meta">${item.reset || (latest ? formatUsageTime(latest.at, item.now) : '')}</span>
    <${Sparkline} points=${item.points} forecast=${item.forecast} now=${item.now} wizard=${wizard} />
    <span class=${`usage-ov-status ${tone}`} title=${item.view.headline}>${quotaStatusLine(item, wizard)}</span>
  </a>`;
}

function Kpi({ label, value, meta, tone = '', onOpen, href }) {
  return html`<a class=${`usage-ov-card usage-ov-kpi${tone ? ` ${tone}` : ''}`} href=${href} onClick=${onOpen}>
    <span class="usage-provider">${label}</span>
    <span class="usage-ov-value">${value}</span>
    <span class="usage-ov-meta">${meta}</span>
  </a>`;
}

function Overview({ quota, costs, wizard, open }) {
  const w = (plain, wizardly) => (wizard ? wizardly : plain);
  const summaries = quotaSummaries(quota, wizard);
  const wall = nextQuotaWall(summaries);
  const atRisk = summaries.filter((item) => item.atRisk).length;
  const today = todaySpend(costs);
  const top = topCostAgents(costs.rows);
  const goQuota = open('quota');
  const goCosts = open('costs');
  const costsLoaded = costs.request.hasLoaded && costs.narrowed;
  const quotaLoaded = quota.request.hasLoaded;
  return html`<div class="usage-overview">
    <div class="usage-ov-kpis">
      <${Kpi} label=${`${w('Spend', 'Coffers')} · ${spanLabel(costs)}`} href="/usage/costs" onOpen=${goCosts} tone="money"
        value=${costsLoaded ? fmtUSD(costs.narrowed.total_usd) : '…'}
        meta=${costsLoaded ? [
          costs.hasWhatIf && `${fmtUSD(costs.narrowed.real_total_usd || 0)} real + ≈${fmtUSD(costs.narrowed.what_if_total_usd || 0)} WHAT-IF`,
          costs.projection && `projected ~${fmtUSD(costs.projection.total)}`,
        ].filter(Boolean).join(' · ') || costs.narrowed.from : ''} />
      <${Kpi} label=${w('Spend today', 'Spent today')} href="/usage/costs" onOpen=${goCosts}
        value=${!costsLoaded ? '…' : today ? fmtUSD(today.cost) : '—'}
        meta=${!costsLoaded ? '' : today
          ? `${today.agents} agent${today.agents === 1 ? '' : 's'}${today.top ? ` · top: ${today.top.title} ${fmtUSD(today.top.cost)}` : ''}`
          : `not in ${spanLabel(costs)}`} />
      <${Kpi} label=${w('Next quota wall', 'Next dry spell')} href="/usage/quota" onOpen=${goQuota}
        tone=${wall ? 'danger' : 'good'}
        value=${!quotaLoaded ? '…' : wall ? (wall.status === 'limit' ? w('now', 'now') : formatUsageTime(wall.forecast.hits_limit_at, wall.now)) : w('none', 'none')}
        meta=${!quotaLoaded ? '' : wall
          ? `${usageProviderLabel(wall.series.provider)} ${usageWindowScopeLabel(wall.series, wizard)}${gapBeforeReset(wall.forecast)
            ? ` · ${gapBeforeReset(wall.forecast)} ${w('without quota', 'without mana')}` : ''}`
          : w('no window predicted to run out', 'every reserve holds')} />
      <${Kpi} label=${w('Windows at risk', 'Reserves at risk')} href="/usage/quota" onOpen=${goQuota}
        tone=${atRisk ? 'warn' : 'good'}
        value=${quotaLoaded ? `${atRisk} / ${summaries.length}` : '…'}
        meta=${quotaLoaded ? w('predicted to hit the limit before reset', 'foretold to run dry before replenishment') : ''} />
    </div>
    ${quotaLoaded && quota.payload?.coverage_warnings?.length > 0 && html`<a class="usage-ov-coverage" href="/usage/quota"
      onClick=${goQuota} role="status">⚠ ${w('Some quota graphs may be incomplete or stale (OpenCode activity has no native quota history) — see Quota',
        'Some reserves may be misread (OpenCode channels unseen) — see Reserves')}.</a>`}
    <h3 class="usage-ov-heading">${w('Quota', 'Reserves')}
      <a href="/usage/quota" onClick=${goQuota}>${w('full forecasts', 'full prophecies')} ›</a></h3>
    ${quota.request.phase === 'error' && !quotaLoaded
      ? html`<div class="empty">${w('Quota history failed to load', 'The reserves could not be read')}: ${quota.request.error}</div>`
      : !quotaLoaded ? html`<div class="empty">Loading…</div>`
        : summaries.length
          ? html`<div class="usage-ov-quota-grid">${summaries.map((item) =>
            html`<${QuotaCard} key=${item.key} item=${item} wizard=${wizard} onOpen=${goQuota} />`)}</div>`
          : html`<div class="empty">${w('No subscription usage samples yet.', 'No mana readings yet.')}</div>`}
    <h3 class="usage-ov-heading">${w('Spend', 'Coffers')} · ${spanLabel(costs)}
      <a href="/usage/costs" onClick=${goCosts}>${w('all costs', 'the full ledger')} ›</a></h3>
    ${costs.request.phase === 'error' && !costsLoaded
      ? html`<div class="empty">${w('Costs failed to load', 'The ledger could not be read')}: ${costs.request.error}</div>`
      : !costsLoaded ? html`<div class="empty">Loading…</div>`
        : html`<div class="usage-ov-spend">
          <${SpendChart} costs=${costs} label=${spanLabel(costs)} />
          <table class="usage-ov-agents">
            <thead><tr><th>${w('Top agents', 'Top spenders')}</th><th>${w('Cost', 'Gold')}</th></tr></thead>
            <tbody>${top.length
              ? top.map((agent) => html`<tr key=${agent.key}><td><span class="rowname">${agent.title}</span>
                <span class="id">${shortAgentId(agent.agentID, agent.convID)}</span></td>
                <td class="cost">${agent.whatIf ? '≈' : ''}${fmtUSD(agent.cost)}</td></tr>`)
              : html`<tr><td colspan="2" class="muted">No spend in this span.</td></tr>`}</tbody>
          </table>
        </div>`}
    ${(costs.filtered || costs.span !== 'month') && costsLoaded && html`<p class="usage-ov-note">
      ${w('Spend follows the span and filters chosen on the Costs view.', 'The coffers follow the span and filters chosen in the ledger.')}</p>`}
  </div>`;
}

// UsageLimits lists the live readings of the global and group limits
// (snapshot usage.usage_limits) and opens their manager.
function UsageLimits({ snapshot, wizard }) {
  const w = (plain, wiz) => (wizard ? wiz : plain);
  const limits = snapshot.value?.usage?.usage_limits || [];
  const manage = () => openUsageRulesManager();
  return html`<div class="usage-hub-limits">
    <h3 class="usage-ov-heading">${w('Limits', 'Wards on the coffers')}
      <button type="button" class="usage-limits-manage" id="usage-limits-manage" onClick=${manage}>${w('limits & budgets', 'wards & tithes')} ›</button></h3>
    ${limits.length ? html`<div class="usage-hub-limit-list">${limits.map((limit) => html`<div key=${limit.rule_key}
      class=${`usage-hub-limit ${limit.dry_run ? 'dry' : barTone(limit)}`} title=${limit.next ? `next: ${limit.next.action.replace('_', ' ')} at ${limit.next.at}%` : ''}>
      <span class="usage-hub-limit-name">${limit.name}<span class="muted"> · ${limit.scope === 'group' ? `group ${limit.group}` : 'global'}${limit.metric === 'window_pct' ? '' : ` · ${periodLabel(limit.period)}`}</span></span>
      <span class="usage-rule-bar"><span class="usage-rule-bar-fill" style=${`width:${Math.min(100, Math.max(0, limit.pct || 0))}%`}></span></span>
      <span class="usage-hub-limit-figure">${fmtAmount(limit.metric, limit.value)} / ${fmtAmount(limit.metric, limit.budget)}${limit.reached ? ` · ${limit.reached.replace('_', ' ')}` : ''}${limit.dry_run ? ' · dry run' : ''}</span>
    </div>`)}</div>`
      : html`<div class="empty usage-hub-limits-empty">${w('No spend or quota limits set.', 'The coffers are unwarded.')}
        <button type="button" class="tool" onClick=${manage}>${w('Set a limit…', 'Ward them…')}</button></div>`}
  </div>`;
}

function subtabMeta(name, quota, costs) {
  if (name === 'quota' && quota.request.hasLoaded) {
    const count = quota.series.length;
    return count ? `${count} window${count === 1 ? '' : 's'}` : '';
  }
  if (name === 'costs' && costs.request.hasLoaded && costs.narrowed) {
    // Name the span (and a narrowed provider/model selection) so the figure
    // never reads as month-to-date when it is not.
    const span = { month: 'mtd', '7d': '7d', '30d': '30d', '90d': '90d' }[costs.span] || spanLabel(costs);
    const narrowed = costs.selectedProviders?.size < (costs.providers?.length || 0)
      || costs.selectedModels?.size < (costs.models?.length || 0);
    return `${fmtUSD(costs.narrowed.total_usd)} ${span}${narrowed ? ' · filtered' : ''}`;
  }
  return '';
}

function Subnav({ current, wizard, open, quota, costs }) {
  return html`<nav class="usage-subnav" aria-label="Usage views">
    ${['overview', 'quota', 'costs'].map((name) => {
      const loc = usageLocation(name);
      const href = loc.subtab ? `/usage/${loc.subtab}` : '/usage';
      return html`<a key=${name} class=${`usage-subtab${current.subview === name ? ' active' : ''}`} href=${href}
        data-usage-subtab=${name} aria-current=${current.subview === name ? 'page' : undefined}
        onClick=${open(name)}>${SUBVIEW_LABELS[name][wizard ? 1 : 0]}${subtabMeta(name, quota, costs)
          && html`<span class="usage-subtab-meta">${subtabMeta(name, quota, costs)}</span>`}</a>`;
    })}
  </nav>`;
}

// FusedUsage is the fused view's per-node summary (usage-nodes.js), read
// while the Usage tab shows.
function FusedUsage({ fleet, snapshot, active, fetchImpl }) {
  const fleetValue = fleet.fleet.value;
  const fused = fleet.fused.value;
  const nodes = fleetValue && fused && !globalThis.__tclaudeRemoteNode ? tickedNodes(fleetValue, fused) : [];
  const peers = nodes.filter((n) => !n.local).map((n) => n.id);
  const entries = usePeerUsages({ ids: peers, active: active && nodes.length > 1, ...(fetchImpl ? { fetchImpl } : {}) });
  if (nodes.length < 2) return null;
  return html`<${FusedUsageSummary} nodes=${nodes} selfUsage=${snapshot.value?.usage} entries=${entries} fmt=${fmtUSD} />`;
}

export function UsageHubApp({ state, quotaState, costsState, fleet = fleetState, snapshot = dashboardState.snapshot, fetchImpl = undefined }) {
  const current = state.view.value;
  const wizard = useWizardTheme();
  const open = (name) => (event) => {
    if (event && isModifiedClick(event)) return;
    event?.preventDefault();
    if (!state.select(name)) return;
    document.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: withCostsPeriod(state.location.value) } }));
    // A quota card names its series; bring that graph into view.
    const series = event?.currentTarget?.dataset?.series;
    if (series) {
      requestAnimationFrame(() => document.querySelector(`#usage-root [data-series-key="${CSS.escape(series)}"]`)
        ?.scrollIntoView({ block: 'nearest' }));
    }
  };
  useEffect(() => {
    if (!current.halves.loaded) return;
    const body = document.body.classList;
    body.toggle('hide-usage-tab', !current.visible);
    body.toggle('hide-usage-quota', !current.halves.quota);
    body.toggle('hide-usage-costs', !current.halves.costs);
    if (!current.visible && current.activeTab === 'usage') document.querySelector('nav [data-tab="groups"]')?.click();
  }, [current.halves.loaded, current.halves.quota, current.halves.costs, current.visible, current.activeTab]);
  useEffect(() => {
    document.querySelector('#tab-usage')?.setAttribute('data-subview', current.subview);
  }, [current.subview]);
  return html`<div class="usage-hub">
    <${FusedUsage} fleet=${fleet} snapshot=${snapshot} active=${current.activeTab === 'usage'} fetchImpl=${fetchImpl} />
    ${current.showSubnav && html`<${Subnav} current=${current} wizard=${wizard} open=${open}
      quota=${quotaState.view.value} costs=${costsState.view.value} />`}
    ${current.subview === 'overview' && html`<${Overview} quota=${quotaState.view.value}
      costs=${costsState.view.value} wizard=${wizard} open=${open} />`}
    ${(current.subview === 'overview' || !current.showSubnav) && html`<${UsageLimits} snapshot=${snapshot} wizard=${wizard} />`}
  </div>`;
}

export function mountUsageHubIsland({ host, state, quotaState, costsState, registerCleanup }) {
  // Router restores (Back/Forward, a deep link) set the view without
  // announcing a navigation of their own.
  const restore = (event) => { state.applyLocation(event.detail?.location); };
  // The header's cost figures link here: land on the Costs view as one
  // history step (the nav click records the hub's location).
  const gotoCosts = (event) => {
    if (event.target.closest?.('[data-goto-tab="costs"]')) state.open('costs');
  };
  document.addEventListener('tclaude:restore-location', restore);
  document.addEventListener('click', gotoCosts);
  render(html`<${UsageHubApp} state=${state} quotaState=${quotaState} costsState=${costsState} />`, host);
  registerCleanup(() => {
    render(null, host);
    document.removeEventListener('tclaude:restore-location', restore);
    document.removeEventListener('click', gotoCosts);
  });
}
