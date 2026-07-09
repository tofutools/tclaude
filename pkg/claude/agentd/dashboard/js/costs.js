// costs.js — the Costs tab: a per-day API-cost bar chart with a
// month-total projection, followed by a per-agent breakdown table.
//
// Data comes from GET /api/costs?from=YYYY-MM-DD (see agentd/costs.go),
// which aggregates the session_cost_daily table — per-day spend deltas
// recovered from the statusline hook's cumulative cost snapshots.
// Unlike the snapshot-driven tabs this one fetches on demand: tab
// activation and span change, plus a slow re-poll while the tab is
// visible — cost history doesn't move on the 2s snapshot cadence.

import { $, $$, esc, shortAgentId, idTooltip } from './helpers.js';
import { morphInto } from './morph.js';
import { dashPrefs } from './prefs.js';
// lastSnapshot lives in dashboard.js — imported back for its cost_tab_whatif
// flag (whether the tab is in WHAT-IF mode). Same deliberate, benign cycle
// tabs.js documents: this is a read-only live binding, touched only inside
// loadCosts()/bindCostDisplayToggle() long after both modules finish
// evaluating, never at module top level.
import { lastSnapshot } from './dashboard.js';

// Sticky toggle: when on, the month projection fills the empty weekdays
// before tclaude's first run this month at the per-weekday average, so
// the figure reads as a representative full month ("projected average
// month cost") instead of one dragged low by starting mid-month. Only
// the "This month" span has a projection, so it's the only span the
// toggle affects. Persisted server-side via dashPrefs (see prefs.js).
const FILL_WEEKDAYS_KEY = 'tclaude.dash.costs.fillEmptyWeekdays';
let fillEmptyWeekdays = false;

// Sticky toggle: when on, weekends count toward the month projection
// instead of being projected at zero. The estimation basis switches
// from per-elapsed-weekday to per-elapsed-day — the denominator counts
// every elapsed calendar day, and the remaining (and leading, when
// filled) weekend days are projected at that per-day average rather
// than zero. Off by default (the historical weekday-only behaviour).
// Only the "This month" span has a projection, so it's the only span
// affected. Persisted server-side via dashPrefs (see prefs.js).
const INCLUDE_WEEKENDS_KEY = 'tclaude.dash.costs.includeWeekends';
let includeWeekends = false;

// Sticky harness subset for the breakdown table. Empty/missing means "all
// currently present harnesses", so newly-added harnesses appear by default.
const HARNESS_FILTER_KEY = 'tclaude.dash.costs.harnesses';

// Sticky toggle for the per-agent cost badge on the Groups/Agents rows
// (the 💲 button in the Groups filter bar). Default shown ('0' = not
// hidden) so pay-per-token behaviour is unchanged; the human can hide the
// badges — handy for the hypothetical WHAT-IF figures on a subscription.
// Drives body.agent-cost-hidden, which CSS uses to suppress .harness-cost.
const COST_HIDDEN_KEY = 'tclaude.dash.agentCost.hidden';

// The fixed date spans (the four buttons). "month" is the calendar month
// to date (the only span that gets a projection); the rest are trailing
// windows ending today. A fifth, dynamic span — 'calmonth' — browses one
// calendar month at a time via the ‹ › stepper (the current month at
// offset 0 folds back into 'month'); it isn't in this array because its
// range depends on monthOffset (below).
const SPANS = [
  { key: 'month', label: 'This month' },
  { key: '7d', label: 'Last 7d', days: 7 },
  { key: '30d', label: 'Last 30d', days: 30 },
  { key: '90d', label: 'Last 90d', days: 90 },
];

// currentSpan is a fixed key above OR 'calmonth' (a completed month
// browsed with the stepper). monthOffset counts calendar months back from
// the current one: 0 = this month, 1 = last month, 2 = two months ago, …
// Offset 0 is the current month — the same span as the "This month" button
// (currentSpan flips to 'month' there, keeping its projection), so the
// stepper is one continuous browser from this month back through history,
// and the current month stays in sync with (and highlighted alongside) the
// "This month" button. It persists across span switches so returning to the
// stepper resumes where you left off.
let currentSpan = 'month';
let monthOffset = 0;

// The furthest back the ‹ stepper may go. The server caps a span at
// maxCostSpanDays days measured from `to`, but a whole-month span is only
// ~31 days so that cap never bites here; this is just a sane floor on how
// many empty months you can page through. Refined down to the first month
// with recorded spend once a payload names data.first_day (see
// syncMonthNav) so ‹ disables at the start of history.
const MAX_MONTH_OFFSET = 24;

// Month names for the stepper label ("June 2026"). English to match the
// tab's other fixed labels ("This month", "Last 7d") rather than the
// browser locale.
const MONTH_NAMES = ['January', 'February', 'March', 'April', 'May', 'June',
  'July', 'August', 'September', 'October', 'November', 'December'];

// The breakdown table's sortable columns, in render order — mirrors the
// Audit tab's clickable-header pattern. Every column is client-side
// sortable (the per-agent rows arrive as one small array, so a header
// click re-renders from the data already in hand — no refetch). `sort` is
// the comparator key; `numeric`/`text` pick the default direction on a
// fresh column (text A→Z, cost/activity newest-or-highest first).
const COST_COLUMNS = [
  { label: 'Agent', sort: 'agent', text: true },
  { label: 'Cost', sort: 'cost', numeric: true },
  { label: 'Harness', sort: 'harness', text: true },
  { label: 'Model', sort: 'model', text: true },
  { label: 'Last activity', sort: 'activity' },
];
// Active sort. Default activity/desc reproduces the server's recency
// ordering (collectCosts already returns rows newest-first), so the
// initial render is unchanged. Held in memory like the Audit tab's view
// state — it survives re-polls and span changes but resets on reload.
let costSort = 'activity';
let costDir = 'desc';
// The last /api/costs payload, kept so a header click can re-sort and
// re-render the table without refetching (the chart/summary are unchanged
// by a sort, only the table re-renders).
let lastCostData = null;

let lastFetchedAt = 0;
// The WHAT-IF mode the last load fetched under. A flip (the opt-in toggled,
// or real spend appearing) must reload immediately so the chart/table/banner
// match the new mode rather than waiting out the slow re-poll.
let lastLoadedWhatIf = false;
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

// monthStart returns the first day (local) of the calendar month `offset`
// months before the current one — offset 0 = this month, 1 = last month.
// JS Date normalises a negative/underflowing month, so month -1 rolls
// back to December of the prior year.
function monthStart(offset) {
  const now = new Date();
  return new Date(now.getFullYear(), now.getMonth() - offset, 1);
}

// spanRange returns the span's [from, to] as local Dates. Trailing
// windows and "This month" end today; 'calmonth' is a completed calendar
// month (first … last day) monthOffset months back — its `to` is that
// month's last day, not today, which is what the /api/costs `to` param
// carries so the server bounds the upper edge instead of running to now.
function spanRange(span) {
  const now = new Date();
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  if (span === 'calmonth') {
    const from = monthStart(monthOffset);
    const to = new Date(from.getFullYear(), from.getMonth() + 1, 0);
    return { from, to };
  }
  const s = SPANS.find(x => x.key === span) || SPANS[0];
  if (!s.days) return { from: new Date(now.getFullYear(), now.getMonth(), 1), to: today };
  const from = new Date(today);
  from.setDate(from.getDate() - (s.days - 1));
  return { from, to: today };
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
// so far. The estimation unit is the elapsed WEEKDAY by default, or the
// elapsed calendar DAY when `weekendsIncl` is on — the `counts` predicate
// below is the single switch. With weekends excluded (the default) the
// average is taken per elapsed weekday and the remaining weekend days are
// projected at zero (weekend spend still counts in the numerator — money
// spent is money spent — it's only the *projected* weekend days that go to
// zero). With weekends included every elapsed day divides the total and
// every remaining day is projected at that per-day average. Returns null
// when no qualifying day has elapsed yet (e.g. weekends-excluded and the
// month started on a weekend) or nothing has been spent — no basis to
// extrapolate from.
//
// The denominator starts at the later of the month's first day
// (data.from) and tclaude's first-ever costed day (data.first_day):
// when the very first use was this month, the empty days before it
// would drag the average toward zero and project far too low, so they
// are excluded; when earlier-month history exists those leading zeros
// are genuine idle days and stay in the denominator (start = the
// 1st). The numerator (total_usd) is unaffected — there is by
// definition no spend before the first-ever costed day.
//
// When `fillEmpty` is on, those excluded leading days (the ones in
// [data.from, startKey) — empty by definition, since nothing was spent
// before the first costed day) are projected at the per-unit average
// too. The figure then represents a representative *full* month
// (perDay × every qualifying day in the month) — "projected average
// month cost" — rather than the current month skewed low by a mid-month
// start. Only the leading empties are filled: idle days after the first
// run are already in the denominator, so projecting them again would
// double count and overshoot perDay × total qualifying days. Which days
// qualify follows the same weekend switch. The returned `total` switches
// with the flag; `leadingFill` (day → projected usd) lets the chart
// render those columns as projected bars.
function monthProjection(data, fillEmpty, weekendsIncl) {
  const now = new Date();
  const todayKey = dayKey(now);
  const startKey = data.first_day && data.first_day > data.from
    ? data.first_day : data.from;
  // The estimation-unit predicate: every day when weekends are included,
  // weekdays only otherwise. Drives the denominator, the future
  // projection and the leading fill alike, so the basis stays consistent.
  const counts = key => weekendsIncl || !isWeekendKey(key);
  let daysElapsed = 0;
  for (const d of data.days) {
    if (d.day >= startKey && d.day <= todayKey && counts(d.day)) daysElapsed++;
  }
  if (!daysElapsed || !(data.total_usd > 0)) return null;
  const perDay = data.total_usd / daysElapsed;

  const lastOfMonth = new Date(now.getFullYear(), now.getMonth() + 1, 0);
  const future = [];
  let projectedRemaining = 0;
  const cursor = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  cursor.setDate(cursor.getDate() + 1);
  for (; cursor <= lastOfMonth; cursor.setDate(cursor.getDate() + 1)) {
    const key = dayKey(cursor);
    const usd = counts(key) ? perDay : 0;
    projectedRemaining += usd;
    future.push({ day: key, cost_usd: usd });
  }

  // Leading empty days: the calendar days before the first costed day
  // this month (data.from .. startKey, exclusive) that qualify under the
  // weekend switch. Only populated when the first run was this month
  // (startKey > data.from); with earlier-month history startKey is the
  // 1st and there is no leading region. data.days is ascending from
  // data.from.
  const leadingFill = {};
  let leadingTotal = 0;
  for (const d of data.days) {
    if (d.day >= startKey) break;
    if (counts(d.day)) {
      leadingFill[d.day] = perDay;
      leadingTotal += perDay;
    }
  }

  const totalNoFill = data.total_usd + projectedRemaining;
  return {
    perDay,
    daysElapsed,
    weekendsIncluded: !!weekendsIncl,
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
  // With the fill toggle on, the leading empty days render as projected
  // (hollow) bars at the per-unit average instead of empty actual
  // columns, so the chart matches the "average month" total. Which
  // leading days are filled (weekdays only, or weekends too) follows the
  // include-weekends switch via proj.leadingFill.
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

function renderSummary(data, proj, span) {
  // The current-month span fetches only through today so the projection can
  // fill the rest of the month (see monthProjection), but the header should
  // read as the whole selected span — 1st → month-end — matching the "This
  // month" button and the projected bars rather than stopping at today's
  // last data point. Every other span shows its real upper edge: trailing
  // windows genuinely end today, and 'calmonth' already ends on its month's
  // last day.
  const now = new Date();
  const to = span === 'month'
    ? dayKey(new Date(now.getFullYear(), now.getMonth() + 1, 0))
    : data.to;
  const bits = [`<span class="cost-total">Total: <strong>${esc(fmtUSD(data.total_usd))}</strong></span>`,
    `<span class="muted">${esc(data.from)} → ${esc(to)}</span>`];
  if (proj) {
    const label = proj.fillEmpty ? 'Projected avg month total' : 'Projected month total';
    const unit = proj.weekendsIncluded ? 'day' : 'weekday';
    const tip = `Spend so far divided by elapsed ${unit}s (${proj.daysElapsed}), extrapolated over the month's remaining ${unit}s — `
      + (proj.weekendsIncluded ? 'weekends included in the estimate.' : 'weekends excluded from the estimate.')
      + (proj.fillEmpty
        ? ` The empty ${unit}s before the first run this month are also filled at the per-${unit} average, so this reflects a representative full month.`
        : '');
    bits.push(`<span class="cost-proj" title="${esc(tip)}">`
      + `${label}: <strong>~${esc(fmtUSD(proj.total))}</strong>`
      + ` <span class="muted">(${esc(fmtUSD(proj.perDay))}/${unit})</span></span>`);
  }
  // Morph rather than swap so the copyable total / projection figures survive
  // the ~60s re-poll tick.
  morphInto($('#costs-summary'), bits.join('<span class="cost-sep">·</span>'));
}

// recencyKey mirrors the server's costRowRecencyKey: the precise
// last-activity timestamp when known, else the calendar day floored to
// midnight (which sorts just below any same-day timestamp). Lexical order
// is time order — the local offset is constant across rows.
function recencyKey(a) {
  if (a.last_activity) return a.last_activity;
  if (a.last_day) return a.last_day + 'T00:00:00';
  return '';
}

// sortCostAgents returns a sorted copy of the breakdown rows for the
// active column/direction. The activity column with dir 'desc' reproduces
// collectCosts's server order exactly (recency desc, then cost desc, then
// conv id asc), so the default view is unchanged. The cost/conv-id
// tiebreakers are applied un-negated regardless of direction, so equal
// primary-key rows keep a stable, sensible order either way.
function sortCostAgents(agents, key, dir) {
  const mul = dir === 'asc' ? 1 : -1;
  const cmpStr = (a, b) => (a < b ? -1 : a > b ? 1 : 0);
  return agents.slice().sort((x, y) => {
    let c = 0;
    switch (key) {
      case 'agent': c = (x.title || '').localeCompare(y.title || ''); break;
      case 'cost': c = (x.cost_usd || 0) - (y.cost_usd || 0); break;
      case 'harness': c = (x.harness || '').localeCompare(y.harness || ''); break;
      case 'model': c = (x.model || '').localeCompare(y.model || ''); break;
      default: c = cmpStr(recencyKey(x), recencyKey(y)); break; // 'activity'
    }
    if (c !== 0) return c < 0 ? -mul : mul;
    if ((x.cost_usd || 0) !== (y.cost_usd || 0)) return (y.cost_usd || 0) - (x.cost_usd || 0);
    return cmpStr(x.conv_id || '', y.conv_id || '');
  });
}

// costHeaderHTML renders the breakdown's clickable column headers with the
// active sort's direction arrow — the same affordance the Audit tab uses.
function costHeaderHTML() {
  return '<tr>' + COST_COLUMNS.map(c => {
    const active = costSort === c.sort;
    const arrow = active ? (costDir === 'asc' ? ' ▲' : ' ▼') : '';
    return `<th class="cost-sort${active ? ' active' : ''}" data-sort="${c.sort}" title="Sort by ${esc(c.label)}">${esc(c.label)}${arrow}</th>`;
  }).join('') + '</tr>';
}

function harnessLabel(h) {
  return h || 'unknown';
}

function costHarnesses(agents) {
  return [...new Set((agents || []).map(a => harnessLabel(a.harness)))].sort((a, b) => a.localeCompare(b));
}

function selectedCostHarnesses(harnesses) {
  let saved = [];
  try { saved = JSON.parse(dashPrefs.getItem(HARNESS_FILTER_KEY) || '[]') || []; } catch (_) { saved = []; }
  const known = new Set(harnesses);
  const selected = saved.filter(h => known.has(h));
  return new Set(selected.length ? selected : harnesses);
}

function saveSelectedCostHarnesses(selected, allHarnesses) {
  const all = selected.size === allHarnesses.length && allHarnesses.every(h => selected.has(h));
  if (all) dashPrefs.removeItem(HARNESS_FILTER_KEY);
  else dashPrefs.setItem(HARNESS_FILTER_KEY, JSON.stringify([...selected]));
}

function renderHarnessFilter(agents) {
  const wrap = $('#filter-costs-harnesses');
  if (!wrap) return { selected: new Set() };
  const harnesses = costHarnesses(agents);
  wrap.hidden = harnesses.length <= 1;
  if (!harnesses.length) {
    wrap.innerHTML = '';
    return { selected: new Set() };
  }
  const selected = selectedCostHarnesses(harnesses);
  wrap.innerHTML = harnesses.map(h =>
    `<label class="filter-toggle costs-harness-choice" title="Show ${esc(h)} cost rows">`
    + `<input type="checkbox" data-harness="${esc(h)}"${selected.has(h) ? ' checked' : ''} />`
    + `<span>${esc(h)}</span></label>`
  ).join('');
  return { selected };
}

// renderTable draws the per-agent breakdown. The API splits a
// conversation that spent across several days into one row per day, so a
// resume shows its true per-day spend (e.g. $16.44 the day it started,
// $3.64 the day it was continued) instead of one double-counted lump.
// The earlier-day slices carry `continued`, rendered with a ↩ marker so
// it's clear they belong to the same conversation as a newer day of that
// agent (which the sort/filter may place anywhere, not necessarily
// adjacent). The footer counts distinct conversations, not rows, so a
// multi-day agent still reads as one agent.
//
// A conversation with more than one slice is a multi-day "chain". Every
// row of such a chain gets data-conv (the shared conv id) and a subtle
// left accent (.cost-chain); its latest day — the one row that is not
// `continued` — is the chain head and gets a ↳ marker so the current
// generation reads as the live tip of a chain rather than as a one-off
// single-day agent (which carries no marker at all). The chain's slices
// can be non-contiguous in any sort, so hovering any one of them
// highlights the whole set (see bindCostsChainHover).
//
// The Agent cell leads with the row's stable agent_id (shortAgentId — the
// rotation-immune `agt_` handle the roster/audit/mail surfaces also lead
// with), falling back to the conv-id prefix when the spend belongs to a
// plain conversation with no agent_id. The full `<agent_id> / <conv-id>`
// pair is on the id span's hover title (idTooltip), so either identifier
// can be read/copied off the tooltip — the same idiom as those tabs.
//
// Columns are click-sortable (costHeaderHTML / costSort+costDir) and the
// rows are narrowed by the table's text filter (#filter-costs — matches
// title / agent id / conv id / model) so a specific agent can be isolated; the
// footer then totals just the visible rows, and the count chip reads
// matched/all. Sort and filter are display-only over the data already
// fetched — neither refetches; the chart and summary stay on the full set.
function renderTable(data) {
  const agents = data.agents || [];
  const bar = $('#costs-table-filter');
  const countEl = $('#filter-costs-count');
  if (bar) bar.hidden = !agents.length;
  if (!agents.length) {
    $('#costs-table').innerHTML = '';
    if (countEl) countEl.textContent = '';
    return;
  }
  const nAgents = new Set(agents.map(a => a.conv_id)).size;
  // Slices per conversation, counted over the FULL set so "active across N
  // days" and the chain accent stay truthful even when the filter hides
  // some of a conversation's days. >1 means a multi-day chain.
  const sliceCount = {};
  for (const a of agents) sliceCount[a.conv_id] = (sliceCount[a.conv_id] || 0) + 1;

  const { selected: harnessSelected } = renderHarnessFilter(agents);
  const q = ($('#filter-costs')?.value || '').trim().toLowerCase();
  const matches = a => !q
    || (a.title || '').toLowerCase().includes(q)
    || (a.agent_id || '').toLowerCase().includes(q)
    || (a.conv_id || '').toLowerCase().includes(q)
    || (a.harness || '').toLowerCase().includes(q)
    || (a.model || '').toLowerCase().includes(q);
  const visible = sortCostAgents(agents, costSort, costDir)
    .filter(a => harnessSelected.has(harnessLabel(a.harness)))
    .filter(matches);
  const filtered = visible.length !== agents.length;
  const shownConvs = new Set(visible.map(a => a.conv_id)).size;
  if (countEl) countEl.textContent = filtered ? `${shownConvs} / ${nAgents}` : '';

  if (!visible.length) {
    morphInto($('#costs-table'), '<div class="empty">No agents match the filter.</div>');
    return;
  }

  // Footer reflects what's shown: when a filter narrows the set it totals
  // just the visible rows (the subtotal for the isolated agent[s]);
  // unfiltered it uses the response total so it agrees with the chart to
  // the cent rather than drifting on floating-point summation.
  const footTotal = filtered ? visible.reduce((s, a) => s + (a.cost_usd || 0), 0) : data.total_usd;
  // Morph rather than swap so the copyable cost figures / agent ids survive the
  // ~60s re-poll (and a header-click re-sort). Each row is keyed by (conv_id,
  // day): conv_id ALONE is not row-unique — a multi-day chain splits one
  // conversation into one row per day (see the header comment) — so the row's
  // own `day` is combined in to make the key unique. The total footer row stays
  // unkeyed (positional, always last).
  morphInto($('#costs-table'), `
    <table>
      <thead>${costHeaderHTML()}</thead>
      <tbody>
        ${visible.map(a => {
          const chain = sliceCount[a.conv_id] > 1;
          const cls = [];
          if (a.continued) cls.push('cost-continued');
          if (chain) cls.push('cost-chain');
          const marker = a.continued
            ? '<span class="cost-cont" title="Continued conversation — an earlier day of this agent; hover to highlight all its days">↩</span> '
            : chain
              ? `<span class="cost-head" title="Latest day of an agent active across ${sliceCount[a.conv_id]} days — hover to highlight all of them">↳</span> `
              : '';
          return `
          <tr data-key="cost-${esc(a.conv_id)}-${esc(a.day)}"${cls.length ? ` class="${cls.join(' ')}"` : ''}${chain ? ` data-conv="${esc(a.conv_id)}"` : ''}>
            <td title="${esc(a.title || '(unknown)')}">${marker}<span class="rowname">${esc(a.title || '(unknown)')}</span> <span class="id" title="${esc(idTooltip(a.agent_id, a.conv_id))}">${esc(shortAgentId(a.agent_id, a.conv_id))}</span></td>
            <td><span class="cost-amt" title="$${(a.cost_usd || 0).toFixed(4)}">${esc(fmtUSD(a.cost_usd))}</span></td>
            <td><span class="muted">${esc(harnessLabel(a.harness))}</span></td>
            <td><span class="muted">${esc(a.model || '')}</span></td>
            <td><span class="muted">${esc(fmtLastActivity(a))}</span></td>
          </tr>`;
        }).join('')}
        <tr class="cost-total-row">
          <td><span class="muted">${filtered ? 'matched' : 'total'} (${shownConvs} agent${shownConvs === 1 ? '' : 's'})</span></td>
          <td><span class="cost-amt">${esc(fmtUSD(footTotal))}</span></td>
          <td></td>
          <td></td>
          <td></td>
        </tr>
      </tbody>
    </table>`);
}

// bindCostsChainHover ties together the rows of a multi-day chain:
// hovering any one highlights every row sharing its conv id. Delegated on
// the #costs-table container (which survives each renderTable re-render),
// so it is wired once. Only rows carrying data-conv (chains of >1 slice)
// participate; comparing the attribute value avoids needing CSS.escape on
// the conv id. mouseleave clears the highlight when the pointer leaves the
// table, and hovering a non-chain row (no data-conv) clears it too.
function bindCostsChainHover() {
  const tbl = $('#costs-table');
  if (!tbl) return;
  let current = null;
  const setHL = conv => {
    tbl.querySelectorAll('tr[data-conv]').forEach(tr =>
      tr.classList.toggle('cost-chain-hl', !!conv && tr.getAttribute('data-conv') === conv));
    current = conv;
  };
  tbl.addEventListener('mouseover', e => {
    const row = e.target.closest('tr[data-conv]');
    const conv = row ? row.getAttribute('data-conv') : null;
    if (conv !== current) setHL(conv);
  });
  tbl.addEventListener('mouseleave', () => { if (current) setHL(null); });
}

// bindCostsChartTip wires the cursor-following day tooltip for the chart.
// The chart's innerHTML is rebuilt on every re-poll, so we delegate on the
// stable #costs-chart container (which persists) and float a single tooltip
// element on document.body — position:fixed keyed off clientX/clientY, so it
// survives the re-render and needs no offset-parent math. Only columns
// carrying data-tip react (empty days have none); the tip is placed just
// off the cursor and flips to the other side near a viewport edge so it
// never spills off-screen.
function bindCostsChartTip() {
  const chart = $('#costs-chart');
  if (!chart) return;
  let tipEl = null;
  const hide = () => { if (tipEl) tipEl.style.display = 'none'; };
  const show = (e, text) => {
    if (!tipEl) {
      tipEl = document.createElement('div');
      tipEl.className = 'cost-tip';
      document.body.appendChild(tipEl);
    }
    tipEl.textContent = text;
    tipEl.style.display = 'block';
    const pad = 14;
    const r = tipEl.getBoundingClientRect();
    let x = e.clientX + pad;
    let y = e.clientY + pad;
    if (x + r.width > window.innerWidth - 4) x = e.clientX - pad - r.width;
    if (y + r.height > window.innerHeight - 4) y = e.clientY - pad - r.height;
    tipEl.style.left = Math.max(4, x) + 'px';
    tipEl.style.top = Math.max(4, y) + 'px';
  };
  chart.addEventListener('mousemove', e => {
    const col = e.target.closest('.cost-col[data-tip]');
    if (!col) { hide(); return; }
    show(e, col.getAttribute('data-tip'));
  });
  chart.addEventListener('mouseleave', hide);
}

// reRenderCostTable re-draws just the breakdown from the data already in
// hand — the shared path for a sort-header click or a filter keystroke,
// neither of which needs a refetch (the chart/summary are unaffected).
function reRenderCostTable() {
  if (lastCostData) renderTable(lastCostData);
}

// bindCostsSort wires the clickable column headers (delegated on the
// re-rendered #costs-table). Clicking the active column flips its
// direction; a fresh column takes its natural default (text A→Z,
// cost/activity highest-or-newest first), mirroring the Audit tab.
function bindCostsSort() {
  const tbl = $('#costs-table');
  if (!tbl) return;
  tbl.addEventListener('click', e => {
    const th = e.target.closest('th.cost-sort');
    if (!th) return;
    const key = th.dataset.sort;
    if (costSort === key) {
      costDir = costDir === 'asc' ? 'desc' : 'asc';
    } else {
      costSort = key;
      const col = COST_COLUMNS.find(c => c.sort === key);
      costDir = col && col.text ? 'asc' : 'desc';
    }
    reRenderCostTable();
  });
}

// bindCostsFilter wires the breakdown's text filter (#filter-costs) and
// its clear button — a client-side narrowing over the rows already
// fetched, so it re-renders live with no debounce. Escape clears too.
function bindCostsFilter() {
  const input = $('#filter-costs');
  if (input) {
    input.addEventListener('input', reRenderCostTable);
    input.addEventListener('keydown', e => {
      if (e.key === 'Escape') { input.value = ''; reRenderCostTable(); }
    });
  }
  const clear = $('#filter-costs-clear');
  if (clear) {
    clear.addEventListener('click', () => {
      if (input) input.value = '';
      reRenderCostTable();
      if (input) input.focus();
    });
  }
}

function bindCostsHarnessFilter() {
  const wrap = $('#filter-costs-harnesses');
  if (!wrap) return;
  wrap.addEventListener('change', e => {
    const cb = e.target.closest('input[type=checkbox][data-harness]');
    if (!cb) return;
    const checks = [...wrap.querySelectorAll('input[type=checkbox][data-harness]')];
    if (!checks.some(x => x.checked)) {
      cb.checked = true;
      return;
    }
    const all = checks.map(x => x.dataset.harness);
    const selected = new Set(checks.filter(x => x.checked).map(x => x.dataset.harness));
    saveSelectedCostHarnesses(selected, all);
    reRenderCostTable();
  });
}

async function loadCosts() {
  const seq = ++loadSeq;
  const span = currentSpan;
  // Stamped at request start — deliberately also throttling after a
  // failure, so a broken endpoint is retried at the slow re-poll
  // cadence rather than on every 2s snapshot tick.
  lastFetchedAt = Date.now();
  // WHAT-IF mode: a subscription account that opted in (cost.show_on_subscription)
  // — the server flags it on the snapshot. Source the hypothetical
  // pay-per-token-equivalent figures (virtual_cost_usd) and show the banner.
  const whatif = !!(lastSnapshot && lastSnapshot.cost_tab_whatif);
  lastLoadedWhatIf = whatif;
  const banner = $('#costs-whatif-banner');
  if (banner) banner.hidden = !whatif;
  try {
    const { from, to } = spanRange(span);
    const r = await fetch('/api/costs?from=' + encodeURIComponent(dayKey(from))
      + '&to=' + encodeURIComponent(dayKey(to))
      + (whatif ? '&whatif=1' : ''),
      { credentials: 'same-origin' });
    if (!r.ok) throw new Error(await r.text() || r.status);
    const data = await r.json();
    if (seq !== loadSeq) return; // superseded by a newer load
    // Kept for the sort/filter re-render path, which redraws the table
    // from this payload without refetching.
    lastCostData = data;
    // first_day (earliest recorded spend) may now bound how far ‹ can
    // step, so refresh the stepper's enabled state against the payload.
    syncMonthNav();
    const proj = span === 'month' ? monthProjection(data, fillEmptyWeekdays, includeWeekends) : null;
    renderSummary(data, proj, span);
    renderChart(data, proj);
    renderTable(data);
  } catch (e) {
    if (seq !== loadSeq) return;
    lastCostData = null;
    // Clear the sibling panes too — a stale summary/table next to the
    // error banner would read as current data for the failed span.
    $('#costs-summary').textContent = '';
    $('#costs-table').innerHTML = '';
    const bar = $('#costs-table-filter');
    if (bar) bar.hidden = true;
    $('#costs-chart').innerHTML =
      `<div class="empty">Failed to load costs: ${esc(e.message || e)}</div>`;
  }
}

function costsTabActive() {
  return $('#tab-costs').classList.contains('active');
}

// --- Cost display multiplier (live edit) -----------------------------
// The factor lives in config.json (cost.estimate_factor) and is applied
// server-side to every cost figure — this tab, the per-agent badges and
// the top bar. Editing it here is a small live twin of the Config tab:
// GET on tab activation, debounced POST on change, then a reload so the
// scaled numbers repaint immediately. Persisted, so it sticks and the
// Config tab shows the same value.
let costFactorSaveTimer = null;

function setCostFactorStatus(msg, isError) {
  const el = $('#costs-factor-status');
  if (!el) return;
  el.textContent = msg || '';
  el.classList.toggle('error', !!isError);
}

// loadCostFactor fetches the resolved factor and shows it in the input.
// Best-effort: a failure leaves the field blank rather than blocking the
// chart, which loads independently.
async function loadCostFactor() {
  const inp = $('#costs-factor');
  if (!inp) return;
  try {
    const r = await fetch('/api/cost-factor', { credentials: 'same-origin' });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    const data = await r.json();
    const f = Number(data.estimate_factor);
    // Show a non-default factor verbatim; 1 (no adjustment) reads cleaner
    // as a blank field with the placeholder "1.0".
    inp.value = (Number.isFinite(f) && f !== 1) ? +f.toFixed(4) : '';
    setCostFactorStatus('');
  } catch (e) {
    setCostFactorStatus('');
  }
}

// saveCostFactor persists the input's value, then reloads the costs so
// the scaled figures show at once. A blank field clears the override
// (null → server resets to 1). The per-agent badges and top bar pick the
// new factor up on the next snapshot tick.
async function saveCostFactor() {
  const inp = $('#costs-factor');
  if (!inp) return;
  const raw = inp.value.trim();
  let factor = null; // blank clears the override
  if (raw !== '') {
    const f = Number(raw);
    if (!Number.isFinite(f) || f <= 0 || f > 10) {
      setCostFactorStatus('must be 0–10', true);
      return;
    }
    factor = f;
  }
  setCostFactorStatus('saving…');
  try {
    const r = await fetch('/api/cost-factor', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ estimate_factor: factor }),
    });
    if (!r.ok) {
      const d = await r.json().catch(() => ({}));
      throw new Error(d.error || ('HTTP ' + r.status));
    }
    setCostFactorStatus('saved');
    loadCosts();
  } catch (e) {
    setCostFactorStatus(e.message || String(e), true);
  }
}

// syncFillToggle enables the projection-only checkboxes ("fill empty
// weekdays" and "include weekends") only on the month span (the only span
// with a projection) and dims them otherwise, so toggling one on a
// trailing-window span — where it would do nothing — reads as inert
// rather than broken.
function syncFillToggle() {
  const active = currentSpan === 'month';
  for (const id of ['fill-weekdays', 'include-weekends']) {
    const cb = $('#costs-' + id);
    const label = $('#costs-' + id + '-label');
    if (!cb || !label) continue;
    cb.disabled = !active;
    label.classList.toggle('disabled', !active);
  }
}

// clampMonthOffset keeps the stepper within [0, MAX_MONTH_OFFSET] — offset
// 0 is the current month (the head of the browse, shared with the "This
// month" button), and MAX_MONTH_OFFSET is the sane paging floor.
// syncMonthNav enforces the data-driven back bound (the first month with
// recorded spend).
function clampMonthOffset(o) {
  return Math.max(0, Math.min(MAX_MONTH_OFFSET, o));
}

// updateMonthLabel writes the stepper button's month name ("June 2026")
// for the current offset.
function updateMonthLabel() {
  const cur = $('#costs-month-cur');
  if (!cur) return;
  const d = monthStart(monthOffset);
  cur.textContent = `${MONTH_NAMES[d.getMonth()]} ${d.getFullYear()}`;
}

// syncSpanHighlight lights the control(s) for the active span. A trailing
// window lights its own fixed button. A browsed month lights the stepper's
// month button; when that month is the current one (offset 0) the dedicated
// "This month" button lights up alongside it — the same span shown in two
// places, so both read as active and stay in sync. All span controls share
// the .active style and live under #costs-spans, so a clear-then-set keeps
// exactly the intended one(s) lit.
function syncSpanHighlight() {
  $$('#costs-spans button.tool').forEach(x => x.classList.remove('active'));
  if (currentSpan === 'month' || currentSpan === 'calmonth') {
    const cur = $('#costs-month-cur');
    if (cur) cur.classList.add('active');
    // Offset 0 is the current month — also light "This month" (the same
    // span, surfaced as a dedicated button). currentSpan === 'month' already
    // implies offset 0, but the guard keeps the intent explicit.
    if (monthOffset === 0) {
      const thisMonth = $('#costs-spans > button.tool[data-span="month"]');
      if (thisMonth) thisMonth.classList.add('active');
    }
  } else {
    const btn = $(`#costs-spans > button.tool[data-span="${currentSpan}"]`);
    if (btn) btn.classList.add('active');
  }
}

// syncMonthNav updates the ‹ › stepper's enabled state. › (newer) stops at
// offset 0 — the current month is the newest, there's no future to page
// into. ‹ (older) stops at MAX_MONTH_OFFSET and, once a payload names the
// first-ever costed day (first_day, across all history), at that month — so
// you can only page back as far as recorded spend exists (the operator's
// "prev month only if data there exists"). When the first-ever spend is
// this month, off is 0 and ‹ disables right at the current month.
function syncMonthNav() {
  const prev = $('#costs-month-prev');
  const next = $('#costs-month-next');
  if (next) next.disabled = monthOffset <= 0;
  let oldest = MAX_MONTH_OFFSET;
  const first = lastCostData && lastCostData.first_day;
  if (first) {
    const fd = new Date(first + 'T12:00:00');
    const now = new Date();
    const off = (now.getFullYear() - fd.getFullYear()) * 12 + (now.getMonth() - fd.getMonth());
    oldest = Math.min(MAX_MONTH_OFFSET, Math.max(0, off));
  }
  if (prev) prev.disabled = monthOffset >= oldest;
}

// activateMonth switches to the browsed month at `offset` (0 = the current
// month, which keeps its projection; ≥1 = a completed month), updating the
// label, the active highlight and the stepper bounds, then reloads. At
// offset 0 currentSpan flips to 'month' so the projection and its toggles
// stay live and the "This month" button lights up with the stepper; a
// completed month is 'calmonth', where the projection toggles go inert
// (syncFillToggle).
function activateMonth(offset) {
  monthOffset = clampMonthOffset(offset);
  currentSpan = monthOffset === 0 ? 'month' : 'calmonth';
  updateMonthLabel();
  syncSpanHighlight();
  syncFillToggle();
  syncMonthNav();
  loadCosts();
}

// selectFixedSpan activates a fixed-span button. "This month" is the
// current month at the head of the month stepper, so it routes through
// activateMonth(0) — keeping the stepper label, bounds and the dual
// highlight in sync. The trailing windows (7d/30d/90d) are their own spans.
function selectFixedSpan(key) {
  if (key === 'month') { activateMonth(0); return; }
  currentSpan = key;
  syncSpanHighlight();
  syncFillToggle();
  syncMonthNav();
  loadCosts();
}

// goMonth handles a ‹/› arrow. While browsing a month — the current one
// ('month') or a completed one ('calmonth') — it steps the offset (‹ delta
// +1 older, › delta -1 newer); from a trailing-window span it instead just
// enters the stepper at the month already shown on the label, so the first
// press reveals that month rather than skipping it. clampMonthOffset + the
// disabled arrows keep it in range (› stops at the current month, ‹ at the
// first month with recorded data).
function goMonth(delta) {
  const inMonthView = currentSpan === 'month' || currentSpan === 'calmonth';
  activateMonth(inMonthView ? monthOffset + delta : monthOffset);
}

// bindCostsTab wires the tab: load on activation, reload on span
// change, slow re-poll off the snapshot tick while visible.
function bindCostsTab() {
  $('nav button[data-tab="costs"]').addEventListener('click', () => { loadCosts(); loadCostFactor(); });
  bindCostsChainHover();
  bindCostsChartTip();
  bindCostsSort();
  bindCostsFilter();
  bindCostsHarnessFilter();
  // Cost display multiplier: debounce typing so a few keystrokes settle
  // into one save+reload, but commit immediately on Enter / blur.
  const factorInput = $('#costs-factor');
  if (factorInput) {
    factorInput.addEventListener('input', () => {
      clearTimeout(costFactorSaveTimer);
      costFactorSaveTimer = setTimeout(saveCostFactor, 600);
    });
    factorInput.addEventListener('change', () => {
      clearTimeout(costFactorSaveTimer);
      saveCostFactor();
    });
    factorInput.addEventListener('keydown', e => {
      if (e.key === 'Enter') { clearTimeout(costFactorSaveTimer); saveCostFactor(); }
    });
  }
  // The four fixed-span buttons — direct children of #costs-spans, so this
  // selector excludes the stepper's buttons (nested in #costs-month-nav),
  // which get their own handlers below.
  $$('#costs-spans > button.tool').forEach(b => {
    b.addEventListener('click', () => selectFixedSpan(b.dataset.span));
  });
  // The ‹ › month stepper and its month-label button (browse a completed
  // calendar month). The label enters/re-enters the stepper at the shown
  // month; the arrows page older/newer (bounds enforced by syncMonthNav).
  const monthCur = $('#costs-month-cur');
  if (monthCur) monthCur.addEventListener('click', () => activateMonth(monthOffset));
  const monthPrev = $('#costs-month-prev');
  if (monthPrev) monthPrev.addEventListener('click', () => goMonth(1));
  const monthNext = $('#costs-month-next');
  if (monthNext) monthNext.addEventListener('click', () => goMonth(-1));
  updateMonthLabel();
  // Light the stepper's month button alongside the default "This month"
  // span (offset 0), so the current month reads as selected in the stepper
  // from the first paint — not just after a span click.
  syncSpanHighlight();
  syncMonthNav();
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
  // "Include weekends" toggle — same persisted-pref + reload-on-change
  // pattern. Switches the month projection's basis from per-weekday to
  // per-day (see monthProjection). dashPrefs is warm here (boot awaits
  // initDashPrefs before this binder runs).
  const weekendsToggle = $('#costs-include-weekends');
  if (weekendsToggle) {
    includeWeekends = dashPrefs.getItem(INCLUDE_WEEKENDS_KEY) === '1';
    weekendsToggle.checked = includeWeekends;
    weekendsToggle.addEventListener('change', () => {
      includeWeekends = weekendsToggle.checked;
      dashPrefs.setItem(INCLUDE_WEEKENDS_KEY, includeWeekends ? '1' : '0');
      loadCosts();
    });
  }
  syncFillToggle();
  document.addEventListener('tclaude:snapshot', () => {
    if (!costsTabActive()) return;
    // Reload immediately when the WHAT-IF mode flips (so the chart/table/banner
    // never lag the body.cost-whatif class applyCostTabVisibility just set);
    // otherwise honour the slow re-poll.
    const whatif = !!(lastSnapshot && lastSnapshot.cost_tab_whatif);
    if (whatif !== lastLoadedWhatIf || Date.now() - lastFetchedAt > REPOLL_MS) loadCosts();
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

// bindCostDisplayToggle wires the 💲 button in the Groups filter bar: a
// sticky show/hide for the per-agent cost badge. Restores the persisted
// state at boot (default shown), flips body.agent-cost-hidden on click, and
// keeps aria-pressed in sync. The badge itself (helpers.js harnessLine)
// renders unconditionally; this class is what CSS uses to hide it, so the
// toggle takes effect on the live DOM without a re-render.
function bindCostDisplayToggle() {
  const btn = $('#groups-cost-toggle');
  if (!btn) return;
  const apply = hidden => {
    document.body.classList.toggle('agent-cost-hidden', hidden);
    btn.setAttribute('aria-pressed', hidden ? 'false' : 'true');
    btn.classList.toggle('off', hidden);
  };
  apply(dashPrefs.getItem(COST_HIDDEN_KEY) === '1');
  btn.addEventListener('click', () => {
    const hidden = !document.body.classList.contains('agent-cost-hidden');
    apply(hidden);
    dashPrefs.setItem(COST_HIDDEN_KEY, hidden ? '1' : '0');
  });
}

export { bindCostsTab, bindCostDisplayToggle };
