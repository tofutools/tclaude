import { h, render } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { isModifiedClick } from './helpers.js';
import { isWizardActive } from './slop.js';
import { COST_SPANS, dayKey, fmtExactUSD, fmtUSD } from './costs-model.js';
import {
  formatUsageResetCountdown, formatUsageTime, usageForecastOf, usageForecastView,
  usageProviderLabel, usageSeriesKeyOf, usageWindowScopeLabel,
} from './usage-history-model.js';
import { usageLocation } from './usage-hub-state.js';

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

// topCostAgents folds the per agent-day slices of the shown rows (the Costs
// view's provider/model filters apply) into one total per agent.
export function topCostAgents(rows, limit = 5) {
  const byAgent = new Map();
  for (const row of rows || []) {
    const key = row.agent_id || row.conv_id;
    const entry = byAgent.get(key) || { key, title: row.title || '(unknown)', cost: 0, whatIf: false };
    entry.cost += row.cost_usd || 0;
    entry.whatIf ||= row.cost_kind === 'what_if' || row.cost_kind === 'mixed';
    byAgent.set(key, entry);
  }
  return [...byAgent.values()].filter((entry) => entry.cost > 0)
    .sort((a, b) => b.cost - a.cost || a.title.localeCompare(b.title)).slice(0, limit);
}

// todaySpend reads today's slice out of the shown span; null when the span
// does not include today (an earlier calendar month).
export function todaySpend(costs, now = new Date()) {
  const key = dayKey(now);
  const day = (costs.narrowed?.days || []).find((item) => item.day === key);
  if (!day) return null;
  const agents = new Set((costs.rows || []).filter((row) => row.day === key)
    .map((row) => row.agent_id || row.conv_id));
  return { cost: day.cost_usd || 0, agents: agents.size };
}

// ---- Views -----------------------------------------------------------------

function Sparkline({ points, forecast, now }) {
  const W = 240;
  const H = 40;
  if (points.length < 2) return html`<div class="usage-ov-spark empty" aria-hidden="true"></div>`;
  const from = new Date(points[0].at).getTime();
  const hit = new Date(forecast?.hits_limit_at).getTime();
  const reset = new Date(forecast?.reset_at).getTime();
  const ends = [now, Number.isFinite(hit) ? hit : now, Number.isFinite(reset) ? reset : now];
  const to = Math.max(...ends.filter((value) => value - now < 7 * 86_400_000));
  const span = Math.max(1, to - from);
  const x = (at) => ((at - from) / span) * W;
  const y = (pct) => H - Math.max(0, Math.min(100, pct)) / 100 * H;
  const line = points.map((point) => `${x(new Date(point.at).getTime()).toFixed(1)},${y(point.pct).toFixed(1)}`).join(' ');
  const last = points[points.length - 1];
  const lastX = x(new Date(last.at).getTime());
  const rate = Number(forecast?.rate_pct_per_hour || 0);
  const projectTo = Number.isFinite(reset) && reset > new Date(last.at).getTime() ? reset : to;
  const projected = rate > 0
    ? Math.min(100, last.pct + rate * (projectTo - new Date(last.at).getTime()) / 3_600_000)
    : null;
  return html`<svg class="usage-ov-spark" viewBox=${`0 0 ${W} ${H}`} preserveAspectRatio="none" aria-hidden="true">
    <line class="limit" x1="0" y1="0.5" x2=${W} y2="0.5" />
    ${Number.isFinite(reset) && reset <= to && html`<line class="reset" x1=${x(reset)} y1="0" x2=${x(reset)} y2=${H} />`}
    <polyline class="observed" points=${line} />
    ${projected !== null && html`<line class="forecast" x1=${lastX} y1=${y(last.pct)}
      x2=${x(projectTo)} y2=${y(projected)} />`}
  </svg>`;
}

function QuotaCard({ item, wizard, onOpen }) {
  const { series, latest } = item;
  const tone = item.atRisk ? 'danger' : item.view.tone;
  return html`<a class=${`usage-ov-card usage-ov-quota${item.atRisk ? ' at-risk' : ''}`} href="/usage/quota"
    data-series=${item.key} onClick=${onOpen}
    title=${`${usageProviderLabel(series.provider)} ${usageWindowScopeLabel(series, wizard)} — open the full forecast`}>
    <div class="usage-ov-card-head">
      <div><span class="usage-provider">${usageProviderLabel(series.provider)}</span>
        <strong class="usage-ov-window">${usageWindowScopeLabel(series, wizard)}</strong></div>
      <span class="usage-ov-pct">${latest ? `${latest.pct.toFixed(0)}%` : '—'}</span>
    </div>
    <span class="usage-ov-meta">${item.reset || (latest ? formatUsageTime(latest.at, item.now) : '')}</span>
    <${Sparkline} points=${item.points} forecast=${item.forecast} now=${item.now} />
    <span class=${`usage-ov-status ${tone}`}>${item.view.headline.replace(/^(Prediction|Prophecy): /, '')}</span>
  </a>`;
}

function Kpi({ label, value, meta, tone = '', onOpen, href }) {
  return html`<a class=${`usage-ov-card usage-ov-kpi${tone ? ` ${tone}` : ''}`} href=${href} onClick=${onOpen}>
    <span class="usage-provider">${label}</span>
    <span class="usage-ov-value">${value}</span>
    <span class="usage-ov-meta">${meta}</span>
  </a>`;
}

function SpendChart({ costs }) {
  const days = costs.narrowed?.days || [];
  const W = 720;
  const H = 150;
  if (!days.length) return html`<div class="usage-ov-chart empty">No days in span.</div>`;
  const future = costs.projection?.future || [];
  const slots = days.length + future.length;
  let running = 0;
  const cumulative = days.map((day) => (running += day.cost_usd || 0));
  const top = Math.max(running, costs.projection?.total || 0, ...days.map((day) => day.cost_usd || 0)) || 1;
  const bw = W / slots;
  const y = (value) => H - (value / top) * (H - 6);
  const line = cumulative.map((value, index) => `${(index + 0.5) * bw},${y(value)}`).join(' ');
  return html`<div class="usage-ov-chart" role="img"
    aria-label=${`${spanLabel(costs)} spend ${fmtExactUSD(running)}${costs.projection ? `, projected ${fmtExactUSD(costs.projection.total)}` : ''}`}>
    <svg viewBox=${`0 0 ${W} ${H}`} preserveAspectRatio="none" aria-hidden="true">
      ${days.map((day, index) => {
        const height = H - y(day.cost_usd || 0);
        return height > 0 && html`<rect key=${day.day} class=${`bar ${day.cost_kind === 'what_if' ? 'what-if' : ''}`}
          x=${index * bw + bw * 0.15} y=${H - height} width=${bw * 0.7} height=${height}>
          <title>${day.day}: ${fmtExactUSD(day.cost_usd || 0)}</title></rect>`;
      })}
      <polyline class="cumulative" points=${line} />
      ${costs.projection && html`<line class="projection" x1=${(days.length - 0.5) * bw} y1=${y(running)}
        x2=${W - bw / 2} y2=${y(costs.projection.total)} />`}
    </svg>
  </div>`;
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
          ? `${today.agents} agent${today.agents === 1 ? '' : 's'} active`
          : `not in ${spanLabel(costs)}`} />
      <${Kpi} label=${w('Next quota wall', 'Next dry spell')} href="/usage/quota" onOpen=${goQuota}
        tone=${wall ? 'danger' : 'good'}
        value=${!quotaLoaded ? '…' : wall ? (wall.status === 'limit' ? w('now', 'now') : formatUsageTime(wall.forecast.hits_limit_at, wall.now)) : w('none', 'none')}
        meta=${!quotaLoaded ? '' : wall
          ? `${usageProviderLabel(wall.series.provider)} ${usageWindowScopeLabel(wall.series, wizard)}`
          : w('no window predicted to run out', 'every reserve holds')} />
      <${Kpi} label=${w('Windows at risk', 'Reserves at risk')} href="/usage/quota" onOpen=${goQuota}
        tone=${atRisk ? 'warn' : 'good'}
        value=${quotaLoaded ? `${atRisk} / ${summaries.length}` : '…'}
        meta=${quotaLoaded ? w('predicted to hit the limit before reset', 'foretold to run dry before replenishment') : ''} />
    </div>
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
          <${SpendChart} costs=${costs} />
          <table class="usage-ov-agents">
            <thead><tr><th>${w('Top agents', 'Top spenders')}</th><th>${w('Cost', 'Gold')}</th></tr></thead>
            <tbody>${top.length
              ? top.map((agent) => html`<tr key=${agent.key}><td><span class="rowname">${agent.title}</span></td>
                <td class="cost">${agent.whatIf ? '≈' : ''}${fmtUSD(agent.cost)}</td></tr>`)
              : html`<tr><td colspan="2" class="muted">No spend in this span.</td></tr>`}</tbody>
          </table>
        </div>`}
    ${(costs.filtered || costs.span !== 'month') && costsLoaded && html`<p class="usage-ov-note">
      ${w('Spend follows the span and filters chosen on the Costs view.', 'The coffers follow the span and filters chosen in the ledger.')}</p>`}
  </div>`;
}

function Subnav({ current, wizard, open }) {
  return html`<nav class="usage-subnav" aria-label="Usage views">
    ${['overview', 'quota', 'costs'].map((name) => {
      const loc = usageLocation(name);
      const href = loc.subtab ? `/usage/${loc.subtab}` : '/usage';
      return html`<a key=${name} class=${`usage-subtab${current.subview === name ? ' active' : ''}`} href=${href}
        data-usage-subtab=${name} aria-current=${current.subview === name ? 'page' : undefined}
        onClick=${open(name)}>${SUBVIEW_LABELS[name][wizard ? 1 : 0]}</a>`;
    })}
  </nav>`;
}

export function UsageHubApp({ state, quotaState, costsState }) {
  const current = state.view.value;
  const wizard = useWizardTheme();
  const open = (name) => (event) => {
    if (event && isModifiedClick(event)) return;
    event?.preventDefault();
    if (!state.select(name)) return;
    document.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: state.location.value } }));
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
    ${current.showSubnav && html`<${Subnav} current=${current} wizard=${wizard} open=${open} />`}
    ${current.subview === 'overview' && html`<${Overview} quota=${quotaState.view.value}
      costs=${costsState.view.value} wizard=${wizard} open=${open} />`}
  </div>`;
}

export function mountUsageHubIsland({ host, state, quotaState, costsState, registerCleanup }) {
  // Router restores (Back/Forward, a deep link) set the view without
  // announcing a navigation of their own.
  const restore = (event) => { state.applyLocation(event.detail?.location); };
  // The header's cost figures link here: land on the Costs view as one
  // history step (the nav click records the hub's location).
  const gotoCosts = (event) => {
    if (!event.target.closest?.('[data-goto-tab="costs"]')) return;
    state.select('costs');
    const nav = document.querySelector('nav [data-tab="usage"]');
    if (nav?.classList.contains('active')) {
      document.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: state.location.value } }));
    } else nav?.click();
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
