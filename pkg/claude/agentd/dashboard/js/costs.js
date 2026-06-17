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
import { dashPrefs } from './prefs.js';

// Sticky toggle: when on, the month projection fills the empty weekdays
// before tclaude's first run this month at the per-weekday average, so
// the figure reads as a representative full month ("projected average
// month cost") instead of one dragged low by starting mid-month. Only
// the "This month" span has a projection, so it's the only span the
// toggle affects. Persisted server-side via dashPrefs (see prefs.js).
const FILL_WEEKDAYS_KEY = 'tclaude.dash.costs.fillEmptyWeekdays';
let fillEmptyWeekdays = false;

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
// Monotonic fetch counter: rapid span flips (or a tab click racing a
// span change) can land responses out of order, and a stale response
// must never repaint the UI for a span the user has already left.
let loadSeq = 0;

// While the tab sits open, refresh this often off the snapshot tick —
// slow enough to stay negligible, fast enough that a day boundary or
// fresh spend shows up without poking the span buttons.
const REPOLL_MS = 60_000;

// dayKey formats a Date as the API's local-calendar-day key.
function dayKey(d) {
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

// fmtLastActivity renders the breakdown's last-activity cell. The API
// sends a precise `last_activity` timestamp (RFC3339) plus the
// date-only `last_day`; prefer the timestamp, shown as local
// "YYYY-MM-DD HH:MM", and fall back to the bare day when no time is
// known (pre-v53 history whose session was already gone).
function fmtLastActivity(a) {
  if (a.last_activity) {
    const d = new Date(a.last_activity);
    if (!isNaN(d.getTime())) {
      const p = n => String(n).padStart(2, '0');
      return `${dayKey(d)} ${p(d.getHours())}:${p(d.getMinutes())}`;
    }
  }
  return a.last_day || '';
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

// niceCeil rounds v up to a 1/2/2.5/5 × 10^k "nice" number — the
// Y-axis top, so the scale reads "$5" rather than "$4.7312".
function niceCeil(v) {
  const base = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) {
    if (m * base >= v - 1e-12) return m * base;
  }
  return 10 * base;
}

// fmtAxisUSD formats a Y-axis tick value compactly: whole dollars
// without cents, fractional ticks with just enough decimals (sub-cent
// scales keep four — a $0.005 tick must not round up to "$0.01").
function fmtAxisUSD(v) {
  if (!(v > 0)) return '$0';
  if (v >= 1000) return '$' + +(v / 1000).toFixed(1) + 'k';
  if (v >= 1) return Number.isInteger(v) ? '$' + v : '$' + v.toFixed(2);
  return '$' + +v.toFixed(4);
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
//
// The weekday denominator starts at the later of the month's first day
// (data.from) and tclaude's first-ever costed day (data.first_day):
// when the very first use was this month, the empty days before it
// would drag the average toward zero and project far too low, so they
// are excluded; when earlier-month history exists those leading zeros
// are genuine idle weekdays and stay in the denominator (start = the
// 1st). The numerator (total_usd) is unaffected — there is by
// definition no spend before the first-ever costed day.
//
// When `fillEmpty` is on, those excluded leading weekdays (the ones in
// [data.from, startKey) — empty by definition, since nothing was spent
// before the first costed day) are projected at the per-weekday average
// too. The figure then represents a representative *full* month
// (perWeekday × every weekday in the month) — "projected average month
// cost" — rather than the current month skewed low by a mid-month start.
// Only the leading empties are filled: idle weekdays after the first run
// are already in the denominator, so projecting them again would double
// count and overshoot perWeekday × total weekdays. The returned `total`
// switches with the flag; `leadingFill` (day → projected usd) lets the
// chart render those columns as projected bars.
function monthProjection(data, fillEmpty) {
  const now = new Date();
  const todayKey = dayKey(now);
  const startKey = data.first_day && data.first_day > data.from
    ? data.first_day : data.from;
  let weekdaysElapsed = 0;
  for (const d of data.days) {
    if (d.day >= startKey && d.day <= todayKey && !isWeekendKey(d.day)) weekdaysElapsed++;
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

  // Leading empty weekdays: the calendar weekdays before the first
  // costed day this month (data.from .. startKey, exclusive). Only
  // populated when the first run was this month (startKey > data.from);
  // with earlier-month history startKey is the 1st and there is no
  // leading region. data.days is ascending from data.from.
  const leadingFill = {};
  let leadingTotal = 0;
  for (const d of data.days) {
    if (d.day >= startKey) break;
    if (!isWeekendKey(d.day)) {
      leadingFill[d.day] = perWeekday;
      leadingTotal += perWeekday;
    }
  }

  const totalNoFill = data.total_usd + projectedRemaining;
  return {
    perWeekday,
    weekdaysElapsed,
    future,
    fillEmpty: !!fillEmpty,
    leadingFill,
    total: fillEmpty ? totalNoFill + leadingTotal : totalNoFill,
  };
}

// barColHTML renders one chart column, with bar height scaled against
// the Y-axis top (scaleMax) so bars line up with the gridlines.
// Projected bars are hollow (CSS) so estimated spend never reads as
// recorded spend; weekend columns are dimmed. The hover tooltip is a
// data-tip attribute (instant CSS tooltip, not the native delayed
// title) and only exists on columns with actual value — hovering an
// empty day shows nothing.
function barColHTML(day, usd, scaleMax, projected, showLabel) {
  const pct = scaleMax > 0 ? Math.max(usd > 0 ? 2 : 0, Math.round(usd / scaleMax * 100)) : 0;
  const date = new Date(day + 'T12:00:00');
  const cls = ['cost-col'];
  if (isWeekendKey(day)) cls.push('weekend');
  if (projected) cls.push('projected');
  const tip = !(usd > 0) ? ''
    : projected
      ? `${day} — projected ~${fmtUSD(usd)}`
      : `${day} — ${fmtUSD(usd)}`;
  return `<div class="${cls.join(' ')}"${tip ? ` data-tip="${esc(tip)}"` : ''}>`
    + `<div class="cost-bararea"><div class="cost-bar" style="height:${pct}%"></div></div>`
    + `<div class="cost-day">${showLabel ? date.getDate() : ''}</div>`
    + `</div>`;
}

// yAxisHTML renders the Y-axis tick labels and the gridline overlay
// for a chart scaled to scaleMax. Both place ticks at the same
// bottom-percentages, so labels and lines stay aligned with the bars
// (whose heights are percentages of the same scale).
function yAxisHTML(scaleMax) {
  const ticks = [
    { pct: 100, label: fmtAxisUSD(scaleMax) },
    { pct: 50, label: fmtAxisUSD(scaleMax / 2) },
    { pct: 0, label: '$0' },
  ];
  const axis = `<div class="cost-yaxis"><div class="cost-yarea">`
    + ticks.map(t => `<div class="cost-ytick" style="bottom:${t.pct}%">${esc(t.label)}</div>`).join('')
    + `</div><div class="cost-day"></div></div>`;
  const grid = `<div class="cost-grid">`
    + ticks.map(t => `<div class="cost-gridline" style="bottom:${t.pct}%"></div>`).join('')
    + `</div>`;
  return { axis, grid };
}

function renderChart(data, proj) {
  // With the fill toggle on, the leading empty weekdays render as
  // projected (hollow) bars at the per-weekday average instead of empty
  // actual columns, so the chart matches the "average month" total.
  const fill = (proj && proj.fillEmpty) ? proj.leadingFill : null;
  const actual = data.days.map(d =>
    fill && fill[d.day] != null
      ? { day: d.day, cost_usd: fill[d.day], projected: true }
      : { ...d, projected: false });
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
  const scaleMax = niceCeil(maxUSD);
  // Thin the day-of-month labels on wide spans so they don't collide.
  const labelEvery = all.length > 62 ? 7 : (all.length > 35 ? 2 : 1);
  const cols = all.map((d, i) =>
    barColHTML(d.day, d.cost_usd, scaleMax, d.projected, i % labelEvery === 0));
  const { axis, grid } = yAxisHTML(scaleMax);
  $('#costs-chart').innerHTML =
    `<div class="cost-chart">${axis}`
    + `<div class="cost-plot">${grid}<div class="cost-cols">${cols.join('')}</div></div>`
    + `</div>`;
}

function renderSummary(data, proj) {
  const bits = [`<span class="cost-total">Total: <strong>${esc(fmtUSD(data.total_usd))}</strong></span>`,
    `<span class="muted">${esc(data.from)} → ${esc(data.to)}</span>`];
  if (proj) {
    const label = proj.fillEmpty ? 'Projected avg month total' : 'Projected month total';
    const tip = `Spend so far divided by elapsed weekdays (${proj.weekdaysElapsed}), extrapolated over the month's remaining weekdays — weekends excluded from the estimate.`
      + (proj.fillEmpty
        ? ' The empty weekdays before the first run this month are also filled at the per-weekday average, so this reflects a representative full month.'
        : '');
    bits.push(`<span class="cost-proj" title="${esc(tip)}">`
      + `${label}: <strong>~${esc(fmtUSD(proj.total))}</strong>`
      + ` <span class="muted">(${esc(fmtUSD(proj.perWeekday))}/weekday)</span></span>`);
  }
  $('#costs-summary').innerHTML = bits.join('<span class="cost-sep">·</span>');
}

// renderTable draws the per-agent breakdown. The API splits a
// conversation that spent across several days into one row per day, so a
// resume shows its true per-day spend (e.g. $16.44 the day it started,
// $3.64 the day it was continued) instead of one double-counted lump.
// The earlier-day slices carry `continued`, rendered with a ↩ marker so
// it's clear they belong to the same conversation as a newer row above.
// The footer counts distinct conversations, not rows, so a multi-day
// agent still reads as one agent.
function renderTable(data) {
  const agents = data.agents || [];
  if (!agents.length) {
    $('#costs-table').innerHTML = '';
    return;
  }
  const nAgents = new Set(agents.map(a => a.conv_id)).size;
  $('#costs-table').innerHTML = `
    <table>
      <thead><tr><th>Agent</th><th>Cost</th><th>Model</th><th>Last activity</th></tr></thead>
      <tbody>
        ${agents.map(a => `
          <tr${a.continued ? ' class="cost-continued"' : ''}>
            <td>${a.continued ? '<span class="cost-cont" title="Continued conversation — earlier day of an agent shown above">↩</span> ' : ''}<span class="rowname">${esc(a.title || '(unknown)')}</span> <span class="id">${esc(shortId(a.conv_id))}</span></td>
            <td><span class="cost-amt" title="$${(a.cost_usd || 0).toFixed(4)}">${esc(fmtUSD(a.cost_usd))}</span></td>
            <td><span class="muted">${esc(a.model || '')}</span></td>
            <td><span class="muted">${esc(fmtLastActivity(a))}</span></td>
          </tr>`).join('')}
        <tr class="cost-total-row">
          <td><span class="muted">total (${nAgents} agent${nAgents === 1 ? '' : 's'})</span></td>
          <td><span class="cost-amt">${esc(fmtUSD(data.total_usd))}</span></td>
          <td></td>
          <td></td>
        </tr>
      </tbody>
    </table>`;
}

async function loadCosts() {
  const seq = ++loadSeq;
  const span = currentSpan;
  // Stamped at request start — deliberately also throttling after a
  // failure, so a broken endpoint is retried at the slow re-poll
  // cadence rather than on every 2s snapshot tick.
  lastFetchedAt = Date.now();
  try {
    const from = dayKey(spanFromDate(span));
    const r = await fetch('/api/costs?from=' + encodeURIComponent(from),
      { credentials: 'same-origin' });
    if (!r.ok) throw new Error(await r.text() || r.status);
    const data = await r.json();
    if (seq !== loadSeq) return; // superseded by a newer load
    const proj = span === 'month' ? monthProjection(data, fillEmptyWeekdays) : null;
    renderSummary(data, proj);
    renderChart(data, proj);
    renderTable(data);
  } catch (e) {
    if (seq !== loadSeq) return;
    // Clear the sibling panes too — a stale summary/table next to the
    // error banner would read as current data for the failed span.
    $('#costs-summary').textContent = '';
    $('#costs-table').innerHTML = '';
    $('#costs-chart').innerHTML =
      `<div class="empty">Failed to load costs: ${esc(e.message || e)}</div>`;
  }
}

function costsTabActive() {
  return $('#tab-costs').classList.contains('active');
}

// syncFillToggle enables the "fill empty weekdays" checkbox only on the
// month span (the only span with a projection) and dims it otherwise, so
// toggling it on a trailing-window span — where it would do nothing —
// reads as inert rather than broken.
function syncFillToggle() {
  const cb = $('#costs-fill-weekdays');
  const label = $('#costs-fill-weekdays-label');
  if (!cb || !label) return;
  const active = currentSpan === 'month';
  cb.disabled = !active;
  label.classList.toggle('disabled', !active);
}

// bindCostsTab wires the tab: load on activation, reload on span
// change, slow re-poll off the snapshot tick while visible.
function bindCostsTab() {
  $('nav button[data-tab="costs"]').addEventListener('click', loadCosts);
  $$('#costs-spans button').forEach(b => {
    b.addEventListener('click', () => {
      currentSpan = b.dataset.span;
      $$('#costs-spans button').forEach(x => x.classList.toggle('active', x === b));
      syncFillToggle();
      loadCosts();
    });
  });
  // "Fill empty weekdays" toggle — restores its persisted state (off
  // when never touched), reloads on change. dashPrefs is loaded before
  // this binder runs (boot awaits initDashPrefs), so the read is warm.
  const fillToggle = $('#costs-fill-weekdays');
  if (fillToggle) {
    fillEmptyWeekdays = dashPrefs.getItem(FILL_WEEKDAYS_KEY) === '1';
    fillToggle.checked = fillEmptyWeekdays;
    fillToggle.addEventListener('change', () => {
      fillEmptyWeekdays = fillToggle.checked;
      dashPrefs.setItem(FILL_WEEKDAYS_KEY, fillEmptyWeekdays ? '1' : '0');
      loadCosts();
    });
  }
  syncFillToggle();
  document.addEventListener('tclaude:snapshot', () => {
    if (costsTabActive() && Date.now() - lastFetchedAt > REPOLL_MS) loadCosts();
  });
  // The top-bar "api" cost token (render.js) carries data-goto-tab=
  // "costs". Clicking it opens this tab exactly as the nav button does:
  // the synthetic .click() fires both handlers bound on that button —
  // the tab-switch (bindTabs) and the load-on-activation above.
  // Delegated so it survives the token's re-render on every snapshot.
  document.addEventListener('click', e => {
    if (e.target.closest('[data-goto-tab="costs"]')) {
      $('nav button[data-tab="costs"]').click();
    }
  });
}

export { bindCostsTab };
