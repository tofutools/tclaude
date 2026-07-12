export const COST_SPANS = [
  { key: 'month', label: 'This month' },
  { key: '7d', label: 'Last 7d', days: 7 },
  { key: '30d', label: 'Last 30d', days: 30 },
  { key: '90d', label: 'Last 90d', days: 90 },
];

export const COST_COLUMNS = [
  { label: 'Agent', sort: 'agent', text: true },
  { label: 'Cost', sort: 'cost', numeric: true },
  { label: 'Harness', sort: 'harness', text: true },
  { label: 'Model', sort: 'model', text: true },
  { label: 'Last activity', sort: 'activity' },
];

export const MAX_MONTH_OFFSET = 24;
export const MONTH_NAMES = ['January', 'February', 'March', 'April', 'May', 'June',
  'July', 'August', 'September', 'October', 'November', 'December'];
export const HARNESS_PALETTE_N = 6;

export function dayKey(date) {
  const pad = (value) => String(value).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

export function monthStart(offset, now = new Date()) {
  return new Date(now.getFullYear(), now.getMonth() - offset, 1);
}

export function spanRange(span, monthOffset, now = new Date()) {
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  if (span === 'calmonth') {
    const from = monthStart(monthOffset, now);
    return { from, to: new Date(from.getFullYear(), from.getMonth() + 1, 0) };
  }
  const selected = COST_SPANS.find((item) => item.key === span) || COST_SPANS[0];
  if (!selected.days) return { from: new Date(now.getFullYear(), now.getMonth(), 1), to: today };
  const from = new Date(today);
  from.setDate(from.getDate() - (selected.days - 1));
  return { from, to: today };
}

export function fmtUSD(value) {
  if (!(value > 0)) return '$0.00';
  return value >= 0.005 ? '$' + value.toFixed(2) : '<1¢';
}

export function fmtAxisUSD(value) {
  if (!(value > 0)) return '$0';
  if (value >= 1000) return '$' + +(value / 1000).toFixed(1) + 'k';
  if (value >= 1) return Number.isInteger(value) ? '$' + value : '$' + value.toFixed(2);
  return '$' + +value.toFixed(4);
}

export function niceCeil(value) {
  const base = Math.pow(10, Math.floor(Math.log10(value)));
  for (const multiple of [1, 2, 2.5, 5, 10]) {
    if (multiple * base >= value - 1e-12) return multiple * base;
  }
  return 10 * base;
}

export function isWeekendKey(key) {
  const day = new Date(key + 'T12:00:00').getDay();
  return day === 0 || day === 6;
}

export function monthProjection(data, fillEmpty, weekendsIncluded, now = new Date()) {
  const today = dayKey(now);
  const start = data.first_day && data.first_day > data.from ? data.first_day : data.from;
  const counts = (key) => weekendsIncluded || !isWeekendKey(key);
  let elapsed = 0;
  for (const day of data.days || []) {
    if (day.day >= start && day.day <= today && counts(day.day)) elapsed += 1;
  }
  if (!elapsed || !(data.total_usd > 0)) return null;
  const perDay = data.total_usd / elapsed;
  const future = [];
  let remaining = 0;
  const last = new Date(now.getFullYear(), now.getMonth() + 1, 0);
  const cursor = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1);
  for (; cursor <= last; cursor.setDate(cursor.getDate() + 1)) {
    const day = dayKey(cursor);
    const cost_usd = counts(day) ? perDay : 0;
    remaining += cost_usd;
    future.push({ day, cost_usd });
  }
  const leadingFill = {};
  let leadingTotal = 0;
  for (const day of data.days || []) {
    if (day.day >= start) break;
    if (counts(day.day)) {
      leadingFill[day.day] = perDay;
      leadingTotal += perDay;
    }
  }
  const withoutFill = data.total_usd + remaining;
  return {
    perDay, daysElapsed: elapsed, weekendsIncluded: !!weekendsIncluded,
    future, fillEmpty: !!fillEmpty, leadingFill,
    total: fillEmpty ? withoutFill + leadingTotal : withoutFill,
  };
}

export const harnessLabel = (harness) => harness || 'unknown';

export function costHarnesses(agents) {
  return [...new Set((agents || []).map((agent) => harnessLabel(agent.harness)))]
    .sort((a, b) => a.localeCompare(b));
}

export function resolveHarnessSelection(harnesses, saved) {
  const known = new Set(harnesses);
  const selected = (saved || []).filter((harness) => known.has(harness));
  return new Set(selected.length ? selected : harnesses);
}

export function filterCostData(payload, selected) {
  const agents = payload?.agents || [];
  const harnesses = costHarnesses(agents);
  if (harnesses.length <= 1 || selected.size === harnesses.length) return payload;
  const totals = {};
  let total = 0;
  for (const agent of agents) {
    if (!selected.has(harnessLabel(agent.harness))) continue;
    totals[agent.day] = (totals[agent.day] || 0) + (agent.cost_usd || 0);
    total += agent.cost_usd || 0;
  }
  return {
    ...payload,
    days: (payload.days || []).map((day) => ({ day: day.day, cost_usd: totals[day.day] || 0 })),
    total_usd: total,
  };
}

export function dailyBreakdown(agents, selected) {
  const result = {};
  for (const agent of agents || []) {
    const harness = harnessLabel(agent.harness);
    if (!selected.has(harness)) continue;
    const day = result[agent.day] || (result[agent.day] = {});
    day[harness] = (day[harness] || 0) + (agent.cost_usd || 0);
  }
  return result;
}

export function harnessSegmentClass(harness, harnesses) {
  const index = harnesses.indexOf(harnessLabel(harness));
  return 'cost-seg-h' + (index >= 0 ? index % HARNESS_PALETTE_N : 0);
}

function recencyKey(agent) {
  if (agent.last_activity) return agent.last_activity;
  if (agent.last_day) return agent.last_day + 'T00:00:00';
  return '';
}

export function sortCostAgents(agents, sort) {
  const key = sort?.key || 'activity';
  const direction = sort?.dir || 'desc';
  const mul = direction === 'asc' ? 1 : -1;
  const compare = (a, b) => (a < b ? -1 : a > b ? 1 : 0);
  return (agents || []).slice().sort((left, right) => {
    let result = 0;
    switch (key) {
      case 'agent': result = (left.title || '').localeCompare(right.title || ''); break;
      case 'cost': result = (left.cost_usd || 0) - (right.cost_usd || 0); break;
      case 'harness': result = harnessLabel(left.harness).localeCompare(harnessLabel(right.harness)); break;
      case 'model': result = (left.model || '').localeCompare(right.model || ''); break;
      default: result = compare(recencyKey(left), recencyKey(right));
    }
    if (result) return result < 0 ? -mul : mul;
    if ((left.cost_usd || 0) !== (right.cost_usd || 0)) return (right.cost_usd || 0) - (left.cost_usd || 0);
    return compare(left.conv_id || '', right.conv_id || '');
  });
}

export function fmtLastActivity(agent) {
  if (agent.last_activity) {
    const date = new Date(agent.last_activity);
    if (!Number.isNaN(date.getTime())) {
      const pad = (value) => String(value).padStart(2, '0');
      return `${dayKey(date)} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
    }
  }
  return agent.last_day || '';
}

export function matchesCostAgent(agent, query) {
  const needle = query.trim().toLowerCase();
  return !needle || [agent.title, agent.agent_id, agent.conv_id, agent.harness, agent.model]
    .some((value) => (value || '').toLowerCase().includes(needle));
}

export function oldestMonthOffset(firstDay, now = new Date()) {
  if (!firstDay) return MAX_MONTH_OFFSET;
  const first = new Date(firstDay + 'T12:00:00');
  const offset = (now.getFullYear() - first.getFullYear()) * 12 + now.getMonth() - first.getMonth();
  return Math.min(MAX_MONTH_OFFSET, Math.max(0, offset));
}

export function monthLabel(offset, now = new Date()) {
  const date = monthStart(offset, now);
  return `${MONTH_NAMES[date.getMonth()]} ${date.getFullYear()}`;
}

export function buildCostChart(data, projection, agents, selected, harnesses) {
  const breakdown = dailyBreakdown(agents, selected);
  const fill = projection?.fillEmpty ? projection.leadingFill : null;
  const actual = (data?.days || []).map((day) => {
    if (fill && fill[day.day] != null) {
      return { day: day.day, cost: fill[day.day], projected: true, segments: [] };
    }
    const parts = breakdown[day.day] || {};
    const segments = harnesses.filter((harness) => (parts[harness] || 0) > 0).map((harness) => ({
      harness, cost: parts[harness], className: harnessSegmentClass(harness, harnesses),
    }));
    return { day: day.day, cost: segments.reduce((sum, segment) => sum + segment.cost, 0), projected: false, segments };
  });
  const future = (projection?.future || []).map((day) => ({
    day: day.day, cost: day.cost_usd, projected: true, segments: [],
  }));
  const days = actual.concat(future);
  const maximum = days.length ? Math.max(...days.map((day) => day.cost)) : 0;
  return { days, scaleMax: maximum > 0 ? niceCeil(maximum) : 0 };
}
