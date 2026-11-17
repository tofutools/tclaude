// phone-usage.js -- the phone's Usage views. The Overview stays the hub's
// own (usage-hub-island.js); on a phone the subnav is a ChipRow, and Quota and
// Costs are summaries in place of the desktop islands, whose fine controls
// (history and look-ahead ranges, prediction choice, calendar months,
// multipliers, filters, charts, the agent table) are left to the desktop:
//
//   Quota: one card per window: % used with a bar, the reset countdown, a
//          sparkline and the prediction line.
//   Costs: the span chips, the total, the daily spend bars stacked by
//          provider (the Overview's, tap a day for its breakdown), the
//          totals per provider and per model, and the top agents.
//
// Both read the same states the desktop views do (usage-history-state.js,
// costs-state.js), so the figures match the desktop's.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { COST_SPANS, costModelLabel, costProviderLabel, fmtExactUSD, fmtUSD, providerSegmentClass } from './costs-model.js';
import { shortAgentId } from './helpers.js';
import { formatUsageTime, usageProviderLabel, usageWindowScopeLabel } from './usage-history-model.js';
import { Sparkline, spendDays, topCostAgents } from './usage-overview-charts.js';
import { Card, ChipRow, Section } from './phone-ui.js';

const html = htm.bind(h);

// ---- Pure derivations (exported for tests) --------------------------------

// quotaBarTone colours a window's bar: red at risk or nearly full, amber past
// two thirds, else the accent.
export function quotaBarTone(item) {
  const pct = item.latest?.pct ?? 0;
  if (item.atRisk || pct >= 90) return 'bad';
  if (pct >= 67) return 'warn';
  return 'ok';
}

// quotaCardMeta is a window card's second line: when it resets and how old
// its latest sample is.
export function quotaCardMeta(item, wizard = false) {
  const sampled = item.latest
    ? `${wizard ? 'scried' : 'sampled'} ${formatUsageTime(item.latest.at, item.now)}`
    : (wizard ? 'no reading' : 'no sample');
  return [item.reset, sampled].filter(Boolean).join(' · ');
}

// costTotals are the Costs view's figures as the phone lists them: the
// totals per provider and per model, and the top agents, all read off the
// narrowed rows (the desktop's Providers and Models choices still apply), so
// the lists add up to the total.
export function costTotals(costs, topLimit = 5) {
  const rows = costs.narrowed?.agents || [];
  const fold = (labelOf, subOf) => {
    const byKey = new Map();
    for (const row of rows) {
      if (!(row.cost_usd > 0)) continue;
      const key = labelOf(row);
      const entry = byKey.get(key) || { key, cost: 0, agents: new Set(), subs: new Set() };
      entry.cost += row.cost_usd;
      entry.agents.add(row.agent_id || row.conv_id);
      entry.subs.add(subOf(row));
      byKey.set(key, entry);
    }
    const total = [...byKey.values()].reduce((sum, entry) => sum + entry.cost, 0);
    return [...byKey.values()].map((entry) => ({
      key: entry.key, cost: entry.cost, agentCount: entry.agents.size, subs: [...entry.subs].sort(),
      share: total > 0 ? entry.cost / total : 0,
    })).sort((a, b) => b.cost - a.cost || a.key.localeCompare(b.key));
  };
  const narrowed = (costs.selectedProviders?.size ?? 0) < (costs.providers?.length ?? 0)
    || (costs.selectedModels?.size ?? 0) < (costs.models?.length ?? 0);
  return {
    total: costs.narrowed?.total_usd || 0,
    real: costs.narrowed?.real_total_usd || 0,
    whatIf: costs.narrowed?.what_if_total_usd || 0,
    providers: fold(costProviderLabel, costModelLabel),
    models: fold(costModelLabel, costProviderLabel),
    narrowed,
    top: topCostAgents(costs.rows, topLimit),
  };
}

// spanChips are the Costs span chips: the trailing spans, plus the browsed
// month while one is shown (stepping months is the desktop's).
export function spanChips(costs) {
  const chips = COST_SPANS.map((span) => ({ key: span.key, label: span.label }));
  if (costs.span === 'calmonth') chips.push({ key: 'calmonth', label: costs.monthLabel });
  return chips;
}

// spendBars are the Costs span's daily bars as the phone draws them: the
// Overview's days stacked by provider (spendDays), the providers that spent in
// the span in palette order (the legend, and every day's breakdown rows), and
// the tallest day, which sets the scale. Empty when the span has no spend.
export function spendBars(costs) {
  const days = spendDays(costs);
  const spent = new Set(days.flatMap((day) => day.segments.map((segment) => segment.provider)));
  const order = costs.providers || [];
  const providers = [...spent].sort((a, b) => order.indexOf(a) - order.indexOf(b) || a.localeCompare(b))
    .map((provider) => ({ provider, className: providerSegmentClass(provider, order) }));
  const top = Math.max(0, ...days.map((day) => day.cost));
  return { days: top > 0 ? days : [], providers, top };
}

// dayBreakdown is the area under the chart: the picked day (the latest when
// none is, or when the picked one left the span), its total, and one row per
// provider of the span, $0 where it spent nothing that day.
export function dayBreakdown(bars, pickedDay) {
  const picked = bars.days.find((day) => day.day === pickedDay);
  const day = picked || bars.days[bars.days.length - 1];
  if (!day) return null;
  const costOf = new Map(day.segments.map((segment) => [segment.provider, segment.cost]));
  return {
    day: day.day, picked: Boolean(picked), cost: day.cost, whatIf: day.whatIf,
    rows: bars.providers.map((entry) => ({ ...entry, cost: costOf.get(entry.provider) || 0 })),
  };
}

// dayLabel is a day key as the chart names it: "Nov 10"; dayOfMonth is the
// axis tick under its bar.
export function dayLabel(key) {
  const date = new Date(key + 'T12:00:00');
  return Number.isNaN(date.getTime()) ? key : date.toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
}
const dayOfMonth = (key) => String(Number(key.slice(8, 10)) || '');

// ---- Views -----------------------------------------------------------------

const SUBVIEW_LABELS = { overview: ['Overview', 'Overview'], quota: ['Quota', 'Reserves'], costs: ['Costs', 'Coffers'] };

// PhoneUsageChips is the hub's subnav on a phone: Overview · Quota · Costs,
// each with the same count the desktop subtab carries.
export function PhoneUsageChips({ current, wizard, open, meta }) {
  return html`<${ChipRow} class="phone-usage-chips" label="Usage views" value=${current.subview}
    onSelect=${(name, event) => open(name)(event)}
    items=${['overview', 'quota', 'costs'].map((name) => ({
      key: name, label: SUBVIEW_LABELS[name][wizard ? 1 : 0], count: meta(name) || null,
    }))} />`;
}

function QuotaWindowCard({ item, wizard }) {
  const { series, latest } = item;
  const pct = latest ? Math.max(0, Math.min(100, latest.pct)) : 0;
  const tone = quotaBarTone(item);
  const name = `${usageProviderLabel(series.provider)} ${usageWindowScopeLabel(series, wizard)}`;
  return html`<${Card} class=${`phone-quota-card${item.atRisk ? ' at-risk' : ''}`} role="group" aria-label=${name}
    data-series=${item.key}>
    <div class="phone-quota-head">
      <div class="phone-quota-name"><span class="ui-eyebrow">${usageProviderLabel(series.provider)}</span>
        <strong>${usageWindowScopeLabel(series, wizard)}</strong></div>
      <span class=${`phone-quota-pct ${tone}`}>${latest ? `${latest.pct.toFixed(0)}%` : '—'}</span>
    </div>
    <div class="phone-quota-bar" role="meter" aria-label=${`${name} used`} aria-valuemin="0" aria-valuemax="100"
      aria-valuenow=${latest ? Math.round(pct) : 0}><b class=${tone} style=${`width:${pct}%`}></b></div>
    <div class="phone-quota-meta">${quotaCardMeta(item, wizard)}</div>
    <${Sparkline} points=${item.points} forecast=${item.forecast} now=${item.now} wizard=${wizard} />
    <div class=${`phone-quota-status ${item.atRisk ? 'danger' : item.view.tone}`}>${item.statusLine}</div>
  <//>`;
}

// PhoneQuota lists the windows, the ones predicted to run out first.
export function PhoneQuota({ quota, summaries, wizard }) {
  const w = (plain, wizardly) => (wizard ? wizardly : plain);
  if (quota.request.phase === 'error' && !quota.request.hasLoaded) {
    return html`<div role="alert" class="island-error">${w('Quota history failed to load', 'The reserves could not be read')}: ${quota.request.error}</div>`;
  }
  if (!quota.request.hasLoaded) return html`<div class="phone-empty">Loading…</div>`;
  const sorted = [...summaries].sort((a, b) => Number(b.atRisk) - Number(a.atRisk));
  return html`<div class="phone-usage-quota">
    ${quota.payload?.coverage_warnings?.length > 0 && html`<p class="phone-usage-warn" role="status">⚠ ${w(
      'Some quota graphs may be incomplete or stale (OpenCode activity has no native quota history).',
      'Some reserves may be misread (OpenCode channels unseen).')}</p>`}
    ${sorted.length
      ? sorted.map((item) => html`<${QuotaWindowCard} key=${item.key} item=${item} wizard=${wizard} />`)
      : html`<div class="phone-empty">${w('No subscription usage samples yet.', 'No mana readings yet.')}</div>`}
    <p class="phone-usage-foot">${w('History ranges and prediction choices are on the desktop.', 'The finer prophecies are read at the desktop.')}</p>
  </div>`;
}

function CostRow({ name, sub, cost, share, whatIf }) {
  return html`<li class="phone-cost-row">
    <span class="phone-cost-name">${name}${sub && html`<span class="phone-cost-sub">${sub}</span>`}</span>
    <span class="phone-cost-fig">${whatIf ? '≈' : ''}${fmtUSD(cost)}${share != null && html`<span class="phone-cost-share">${Math.round(share * 100)}%</span>`}</span>
    ${share != null && html`<span class="phone-cost-bar" aria-hidden="true"><b style=${`width:${Math.max(0, Math.min(100, share * 100))}%`}></b></span>`}
  </li>`;
}

// PhoneSpendChart is the Overview's daily spend bars at the phone's width.
// A phone has no hover, so each day is a button: tapping it picks that day
// (outlined) and the breakdown under the chart shows it; tapping it again, or
// nothing tapped, shows the latest day.
export function PhoneSpendChart({ costs, wizard }) {
  const w = (plain, wizardly) => (wizard ? wizardly : plain);
  const [pickedDay, setPickedDay] = useState(null);
  // What a screen reader hears on a tap: the picked day in one line. The
  // breakdown itself is not live, so the minute's reload of today's figures
  // is not read out.
  const [announcement, setAnnouncement] = useState('');
  const bars = spendBars(costs);
  const title = w('Daily spend', 'Daily tithes');
  if (!bars.days.length) {
    return html`<${Card} class="phone-spend" role="group" aria-label=${title}>
      <div class="phone-spend-head"><strong>${title}</strong></div>
      <div class="phone-empty">${w('No spend in this span.', 'The coffers are untouched.')}</div>
    <//>`;
  }
  const shown = dayBreakdown(bars, pickedDay);
  const pick = (day) => {
    setPickedDay(day?.day || null);
    const next = dayBreakdown(bars, day?.day || null);
    setAnnouncement(`${dayLabel(next.day)}${day ? '' : ', latest'}: ${fmtExactUSD(next.cost)}; ${next.rows.map((row) => `${row.provider} ${fmtExactUSD(row.cost)}`).join(', ')}`);
  };
  // A tick under every bar up to a week, else about seven along the span.
  const tickEvery = Math.max(1, Math.ceil(bars.days.length / 7));
  const tick = (index) => (bars.days.length - 1 - index) % tickEvery === 0;
  return html`<${Card} class="phone-spend" role="group" aria-label=${title}>
    <div class="phone-spend-head"><strong>${title}</strong><span>${w('by provider', 'by house')}</span></div>
    <div class="phone-spend-plot" style=${`--phone-spend-days:${bars.days.length}`}>
      ${bars.days.map((day, index) => {
        const picked = shown.picked && shown.day === day.day;
        return html`<button key=${day.day} type="button" data-ui-exempt data-day=${day.day}
          class=${`phone-spend-day${picked ? ' picked' : ''}${day.whatIf ? ' what-if' : ''}`}
          aria-pressed=${picked ? 'true' : 'false'} aria-label=${`${dayLabel(day.day)}, ${fmtExactUSD(day.cost)}`}
          onClick=${() => pick(picked ? null : day)}>
          <span class="phone-spend-stack" style=${`height:${(day.cost / bars.top) * 100}%`}>
            ${day.segments.map((segment) => html`<i key=${segment.provider} class=${segment.className}
              style=${`flex-grow:${segment.cost}`}></i>`)}
          </span>
          <span class="phone-spend-tick" aria-hidden="true">${tick(index) ? dayOfMonth(day.day) : ''}</span>
        </button>`;
      })}
    </div>
    <div class="phone-spend-legend">
      ${bars.providers.map((entry) => html`<span key=${entry.provider}><i class=${entry.className}></i>${entry.provider}</span>`)}
    </div>
    <div class="phone-spend-status" role="status" aria-live="polite" aria-atomic="true">${announcement}</div>
    <div class="phone-spend-detail">
      <div class="phone-spend-detail-head"><strong>${dayLabel(shown.day)}${shown.picked ? '' : html` <span class="phone-spend-latest">· latest</span>`}</strong>
        <span>${shown.whatIf ? '≈' : ''}${fmtExactUSD(shown.cost)}</span></div>
      ${shown.rows.map((row) => html`<div key=${row.provider} class="phone-spend-row">
        <span><i class=${row.className}></i>${row.provider}</span><span>${fmtExactUSD(row.cost)}</span></div>`)}
    </div>
  <//>`;
}

// PhoneCosts is the Costs summary. pickSpan(key) changes the span the way
// the desktop's span buttons do.
export function PhoneCosts({ costs, wizard, pickSpan, spanLabel }) {
  const w = (plain, wizardly) => (wizard ? wizardly : plain);
  const loaded = costs.request.hasLoaded && costs.narrowed;
  const chips = html`<${ChipRow} class="phone-cost-spans" label="Costs span" value=${costs.span}
    onSelect=${(key) => { if (key !== costs.span && key !== 'calmonth') pickSpan(key); }} items=${spanChips(costs)} />`;
  if (costs.request.phase === 'error' && !loaded) {
    return html`<div class="phone-usage-costs">${chips}
      <div role="alert" class="island-error">${w('Costs failed to load', 'The ledger could not be read')}: ${costs.request.error}</div></div>`;
  }
  if (!loaded) return html`<div class="phone-usage-costs">${chips}<div class="phone-empty">Loading…</div></div>`;
  const t = costTotals(costs);
  return html`<div class="phone-usage-costs">
    ${chips}
    <${Card} class="phone-cost-total" role="group" aria-label=${`Total ${spanLabel}`}>
      <span class="ui-eyebrow">${w('Total', 'Coffers')} · ${spanLabel}${t.narrowed ? ` · ${w('filtered', 'filtered')}` : ''}</span>
      <span class="phone-cost-big">${fmtUSD(t.total)}</span>
      <span class="phone-cost-meta">${[
        costs.hasWhatIf && `${fmtUSD(t.real)} real + ≈${fmtUSD(t.whatIf)} WHAT-IF`,
        costs.projection && `projected ~${fmtUSD(costs.projection.total)}`,
      ].filter(Boolean).join(' · ') || `${costs.narrowed.from || ''}`}</span>
      ${costs.hasWhatIf && html`<span class="phone-cost-caveat">≈ WHAT-IF is the pay-per-token equivalent of subscription use, not a real charge.</span>`}
    <//>
    <${PhoneSpendChart} costs=${costs} wizard=${wizard} />
    <${Section} title=${w('By provider', 'By house')} count=${t.providers.length}>
      ${t.providers.length
        ? html`<${Card} as="ul" class="phone-cost-list">${t.providers.map((entry) => html`<${CostRow} key=${entry.key} name=${entry.key}
          sub=${`${entry.agentCount} agent${entry.agentCount === 1 ? '' : 's'}`} cost=${entry.cost} share=${entry.share} />`)}<//>`
        : html`<div class="phone-empty">${w('No spend in this span.', 'The coffers are untouched.')}</div>`}
    <//>
    <${Section} title=${w('By model', 'By familiar')} count=${t.models.length}>
      ${t.models.length
        ? html`<${Card} as="ul" class="phone-cost-list">${t.models.map((entry) => html`<${CostRow} key=${entry.key} name=${entry.key}
          sub=${entry.subs.join(', ')} cost=${entry.cost} share=${entry.share} />`)}<//>`
        : html`<div class="phone-empty">${w('No spend in this span.', 'The coffers are untouched.')}</div>`}
    <//>
    <${Section} title=${w('Top agents', 'Top spenders')} count=${t.top.length}>
      ${t.top.length
        ? html`<${Card} as="ul" class="phone-cost-list">${t.top.map((agent) => html`<${CostRow} key=${agent.key} name=${agent.title}
          sub=${shortAgentId(agent.agentID, agent.convID)} cost=${agent.cost} whatIf=${agent.whatIf} />`)}<//>`
        : html`<div class="phone-empty">${w('No spend in this span.', 'The coffers are untouched.')}</div>`}
    <//>
    <p class="phone-usage-foot">${w('Running totals, months, filters and multipliers are on the desktop.', 'The full ledger is kept at the desktop.')}</p>
  </div>`;
}
