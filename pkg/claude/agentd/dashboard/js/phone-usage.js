// phone-usage.js -- the phone's Usage views. The Overview stays the hub's
// own (usage-hub-island.js); on a phone the subnav is a ChipRow, and Quota and
// Costs are summaries in place of the desktop islands, whose fine controls
// (history and look-ahead ranges, prediction choice, calendar months,
// multipliers, filters, charts, the agent table) are left to the desktop:
//
//   Quota: one card per window: % used with a bar, the reset countdown, a
//          sparkline and the prediction line.
//   Costs: the span chips, the total, the totals per provider and per model,
//          and the top agents.
//
// Both read the same states the desktop views do (usage-history-state.js,
// costs-state.js), so the figures match the desktop's.

import { h } from 'preact';
import htm from 'htm';
import { COST_SPANS, fmtUSD } from './costs-model.js';
import { shortAgentId } from './helpers.js';
import { formatUsageTime, usageProviderLabel, usageWindowScopeLabel } from './usage-history-model.js';
import { Sparkline, topCostAgents } from './usage-overview-charts.js';
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
// providers and models the current selection covers (the desktop's
// Providers and Models choices still apply), the top agents, and the total.
export function costTotals(costs, topLimit = 5) {
  const providers = (costs.providerStats || [])
    .filter((entry) => !costs.selectedProviders || costs.selectedProviders.has(entry.provider))
    .filter((entry) => entry.cost > 0);
  const models = (costs.modelStats || [])
    .filter((entry) => entry.available && (!costs.selectedModels || costs.selectedModels.has(entry.model)))
    .filter((entry) => entry.cost > 0);
  const narrowed = (costs.selectedProviders?.size ?? 0) < (costs.providers?.length ?? 0)
    || (costs.selectedModels?.size ?? 0) < (costs.models?.length ?? 0);
  return {
    total: costs.narrowed?.total_usd || 0,
    real: costs.narrowed?.real_total_usd || 0,
    whatIf: costs.narrowed?.what_if_total_usd || 0,
    providers, models, narrowed,
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
  const sum = (list) => list.reduce((acc, entry) => acc + entry.cost, 0) || 1;
  const providerSum = sum(t.providers);
  const modelSum = sum(t.models);
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
    <${Section} title=${w('By provider', 'By house')} count=${t.providers.length}>
      ${t.providers.length
        ? html`<ul class="phone-cost-list">${t.providers.map((entry) => html`<${CostRow} key=${entry.provider} name=${entry.provider}
          sub=${`${entry.agentCount} agent${entry.agentCount === 1 ? '' : 's'}`} cost=${entry.cost} share=${entry.cost / providerSum} />`)}</ul>`
        : html`<div class="phone-empty">${w('No spend in this span.', 'The coffers are untouched.')}</div>`}
    <//>
    <${Section} title=${w('By model', 'By familiar')} count=${t.models.length}>
      ${t.models.length
        ? html`<ul class="phone-cost-list">${t.models.map((entry) => html`<${CostRow} key=${entry.model} name=${entry.model}
          sub=${entry.availableProviders.join(', ')} cost=${entry.cost} share=${entry.cost / modelSum} />`)}</ul>`
        : html`<div class="phone-empty">${w('No spend in this span.', 'The coffers are untouched.')}</div>`}
    <//>
    <${Section} title=${w('Top agents', 'Top spenders')} count=${t.top.length}>
      ${t.top.length
        ? html`<ul class="phone-cost-list">${t.top.map((agent) => html`<${CostRow} key=${agent.key} name=${agent.title}
          sub=${shortAgentId(agent.agentID, agent.convID)} cost=${agent.cost} whatIf=${agent.whatIf} />`)}</ul>`
        : html`<div class="phone-empty">${w('No spend in this span.', 'The coffers are untouched.')}</div>`}
    <//>
    <p class="phone-usage-foot">${w('Charts, months, filters and multipliers are on the desktop.', 'The full ledger is kept at the desktop.')}</p>
  </div>`;
}
