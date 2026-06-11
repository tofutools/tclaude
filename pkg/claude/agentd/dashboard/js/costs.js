// costs.js — the Costs tab: a per-day API-cost bar chart with a
// month-total projection, followed by a per-agent breakdown table.
//
// Data comes from GET /api/costs?from=YYYY-MM-DD (see agentd/costs.go),
// which aggregates the session_cost_daily table — per-day spend deltas
// recovered from the statusline hook's cumulative cost snapshots.
// Unlike the snapshot-driven tabs this one fetches on demand: tab
// activation and span change, plus a slow re-poll while the tab is
// visible — cost history doesn't move on the 2s snapshot cadence.

import { $, $$, esc, shortId } from './helpers.js';

// The selectable date spans. "month" is the calendar month to date
// (the only span that gets a projection); the rest are trailing
// windows ending today.
const SPANS = [
  { key: 'month', label: 'This month' },
  { key: '7d', label: 'Last 7d', days: 7 },
  { key: '30d', label: 'Last 30d', days: 30 },
  { key: '90d', label: 'Last 90d', days: 90 },
];

let currentSpan = 'month';
let lastFetchedAt = 0;

// While the tab sits open, refresh this often off the snapshot tick —
// slow enough to stay negligible, fast enough that a day boundary or
// fresh spend shows up without poking the span buttons.
const REPOLL_MS = 60_000;

// dayKey formats a Date as the API's local-calendar-day key.
function dayKey(d) {
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

// spanFromDate computes the span's starting Date (local).
function spanFromDate(span) {
  const now = new Date();
  const s = SPANS.find(x => x.key === span) || SPANS[0];
  if (!s.days) return new Date(now.getFullYear(), now.getMonth(), 1);
  const d = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  d.setDate(d.getDate() - (s.days - 1));
  return d;
}

// fmtUSD mirrors the harness-line cost token: two decimals, sub-cent
// totals as "<1¢" rather than a lying "$0.00".
function fmtUSD(v) {
  if (!(v > 0)) return '$0.00';
  return v >= 0.005 ? '$' + v.toFixed(2) : '<1¢';
}

// isWeekendKey reports whether a "YYYY-MM-DD" key falls on a
// Saturday/Sunday (local — keys are local days by construction).
function isWeekendKey(key) {
  const dow = new Date(key + 'T12:00:00').getDay();
  return dow === 0 || dow === 6;
}

// monthProjection estimates the calendar month's total from the spend
// so far, excluding weekends from the estimation: the average is
// taken per elapsed WEEKDAY (weekend spend still counts in the
// numerator — money spent is money spent — it's the remaining
// weekend days that are projected at zero). Returns null when the
// month has no elapsed weekdays yet (a month starting on a weekend)
// or nothing has been spent — no basis to extrapolate from.
function monthProjection(data) {
  const now = new Date();
  const todayKey = dayKey(now);
  let weekdaysElapsed = 0;
  for (const d of data.days) {
    if (d.day <= todayKey && !isWeekendKey(d.day)) weekdaysElapsed++;
  }
  if (!weekdaysElapsed || !(data.total_usd > 0)) return null;
  const perWeekday = data.total_usd / weekdaysElapsed;

  const lastOfMonth = new Date(now.getFullYear(), now.getMonth() + 1, 0);
  const future = [];
  let projectedRemaining = 0;
  const cursor = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  cursor.setDate(cursor.getDate() + 1);
  for (; cursor <= lastOfMonth; cursor.setDate(cursor.getDate() + 1)) {
    const key = dayKey(cursor);
    const usd = isWeekendKey(key) ? 0 : perWeekday;
    projectedRemaining += usd;
    future.push({ day: key, cost_usd: usd });
  }
  return {
    perWeekday,
    weekdaysElapsed,
    future,
    total: data.total_usd + projectedRemaining,
  };
}

// barColHTML renders one chart column. Projected bars are hollow
// (CSS) so estimated spend never reads as recorded spend; weekend
// columns are dimmed.
function barColHTML(day, usd, maxUSD, projected, showLabel) {
  const pct = maxUSD > 0 ? Math.max(usd > 0 ? 2 : 0, Math.round(usd / maxUSD * 100)) : 0;
  const date = new Date(day + 'T12:00:00');
  const cls = ['cost-col'];
  if (isWeekendKey(day)) cls.push('weekend');
  if (projected) cls.push('projected');
  const tip = projected
    ? `${day} — projected ~$${usd.toFixed(2)}`
    : `${day} — $${usd.toFixed(4)}`;
  return `<div class="${cls.join(' ')}" title="${esc(tip)}">`
    + `<div class="cost-bar" style="height:${pct}%"></div>`
    + `<div class="cost-day">${showLabel ? date.getDate() : ''}</div>`
    + `</div>`;
}

function renderChart(data, proj) {
  const actual = data.days.map(d => ({ ...d, projected: false }));
  const future = (proj ? proj.future : []).map(d => ({ ...d, projected: true }));
  const all = actual.concat(future);
  if (!all.length) {
    $('#costs-chart').innerHTML = '<div class="empty">No days in span.</div>';
    return;
  }
  const maxUSD = Math.max(...all.map(d => d.cost_usd));
  if (!(maxUSD > 0)) {
    $('#costs-chart').innerHTML =
      '<div class="empty">No API cost recorded in this span. Cost is tracked only for agents on API/enterprise pricing (subscription sessions have no per-dollar cost).</div>';
    return;
  }
  // Thin the day-of-month labels on wide spans so they don't collide.
  const labelEvery = all.length > 62 ? 7 : (all.length > 35 ? 2 : 1);
  const cols = all.map((d, i) =>
    barColHTML(d.day, d.cost_usd, maxUSD, d.projected, i % labelEvery === 0));
  $('#costs-chart').innerHTML =
    `<div class="cost-chart" style="--cols:${all.length}">${cols.join('')}</div>`;
}

function renderSummary(data, proj) {
  const bits = [`<span class="cost-total">Total: <strong>${esc(fmtUSD(data.total_usd))}</strong></span>`,
    `<span class="muted">${esc(data.from)} → ${esc(data.to)}</span>`];
  if (proj) {
    bits.push(`<span class="cost-proj" title="Spend so far divided by elapsed weekdays (${proj.weekdaysElapsed}), extrapolated over the month's remaining weekdays — weekends excluded from the estimate.">`
      + `Projected month total: <strong>~${esc(fmtUSD(proj.total))}</strong>`
      + ` <span class="muted">(${esc(fmtUSD(proj.perWeekday))}/weekday)</span></span>`);
  }
  $('#costs-summary').innerHTML = bits.join('<span class="cost-sep">·</span>');
}

function renderTable(data) {
  const agents = data.agents || [];
  if (!agents.length) {
    $('#costs-table').innerHTML = '';
    return;
  }
  $('#costs-table').innerHTML = `
    <table>
      <thead><tr><th>Agent</th><th>Cost</th><th>Last activity</th></tr></thead>
      <tbody>
        ${agents.map(a => `
          <tr>
            <td><span class="rowname">${esc(a.title || '(unknown)')}</span> <span class="id">${esc(shortId(a.conv_id))}</span></td>
            <td><span class="cost-amt" title="$${(a.cost_usd || 0).toFixed(4)}">${esc(fmtUSD(a.cost_usd))}</span></td>
            <td><span class="muted">${esc(a.last_day || '')}</span></td>
          </tr>`).join('')}
        <tr class="cost-total-row">
          <td><span class="muted">total (${agents.length} agent${agents.length === 1 ? '' : 's'})</span></td>
          <td><span class="cost-amt">${esc(fmtUSD(data.total_usd))}</span></td>
          <td></td>
        </tr>
      </tbody>
    </table>`;
}

async function loadCosts() {
  lastFetchedAt = Date.now();
  try {
    const from = dayKey(spanFromDate(currentSpan));
    const r = await fetch('/api/costs?from=' + encodeURIComponent(from),
      { credentials: 'same-origin' });
    if (!r.ok) throw new Error(await r.text() || r.status);
    const data = await r.json();
    const proj = currentSpan === 'month' ? monthProjection(data) : null;
    renderSummary(data, proj);
    renderChart(data, proj);
    renderTable(data);
  } catch (e) {
    $('#costs-chart').innerHTML =
      `<div class="empty">Failed to load costs: ${esc(e.message || e)}</div>`;
  }
}

function costsTabActive() {
  return $('#tab-costs').classList.contains('active');
}

// bindCostsTab wires the tab: load on activation, reload on span
// change, slow re-poll off the snapshot tick while visible.
function bindCostsTab() {
  $('nav button[data-tab="costs"]').addEventListener('click', loadCosts);
  $$('#costs-spans button').forEach(b => {
    b.addEventListener('click', () => {
      currentSpan = b.dataset.span;
      $$('#costs-spans button').forEach(x => x.classList.toggle('active', x === b));
      loadCosts();
    });
  });
  document.addEventListener('tclaude:snapshot', () => {
    if (costsTabActive() && Date.now() - lastFetchedAt > REPOLL_MS) loadCosts();
  });
}

export { bindCostsTab };
