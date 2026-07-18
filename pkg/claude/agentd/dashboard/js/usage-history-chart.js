import { h } from 'preact';
import htm from 'htm';
import { formatUsageAxisTick, usageAxisStart, usageAxisTicks } from './usage-history-axis.js';
import { usageProviderLabel, usageWindowLabel } from './usage-history-model.js';

const html = htm.bind(h);
const W = 720;
const H = 210;
const PAD = { left: 42, right: 18, top: 14, bottom: 28 };

function finiteDate(value) {
  if (value === null || value === undefined || value === '') return null;
  const result = new Date(value).getTime();
  return Number.isFinite(result) ? result : null;
}

function sampledPoints(points, maxPoints) {
  if (points.length <= maxPoints) return points;
  const stride = Math.ceil((points.length - 1) / (maxPoints - 1));
  const sampled = points.filter((_, index) => index === 0 || index === points.length - 1 || index % stride === 0);
  return sampled.length <= maxPoints ? sampled : [...sampled.slice(0, maxPoints - 1), points[points.length - 1]];
}

export function UsageHistoryChart({ series, from, generatedAt }) {
  const points = (series.points || []).map((point) => ({ ...point, time: finiteDate(point.at) })).filter((point) => point.time !== null);
  if (!points.length) return html`<div class="usage-chart-empty">No samples in this range.</div>`;
  const now = finiteDate(generatedAt) ?? points[points.length - 1].time;
  const start = usageAxisStart(finiteDate(from), points[0].time, now);
  const forecast = series.forecast || {};
  const rate = Number(forecast.rate_pct_per_hour) || 0;
  const hitAt = finiteDate(forecast.hits_limit_at);
  const resetAt = finiteDate(forecast.reset_at);
  const projecting = rate > 0 && ['before_reset', 'after_reset', 'projected'].includes(forecast.status);
  const eventAt = forecast.status === 'after_reset'
    ? resetAt
    : ['before_reset', 'projected'].includes(forecast.status) ? (hitAt ?? resetAt) : null;
  const horizon = projecting && eventAt && eventAt > now ? eventAt : now;
  const x = (time) => PAD.left + Math.max(0, Math.min(1, (time - start) / (horizon - start))) * (W - PAD.left - PAD.right);
  const y = (pct) => PAD.top + (1 - Math.max(0, Math.min(100, pct)) / 100) * (H - PAD.top - PAD.bottom);
  const resetTimes = new Set((series.resets || []).map((reset) => finiteDate(reset.at)));
  const segments = [];
  let current = [];
  for (const point of points) {
    if (resetTimes.has(point.time) && current.length) {
      segments.push(current);
      current = [];
    }
    current.push(point);
  }
  if (current.length) segments.push(current);
  const latest = points[points.length - 1];
  const forecastAt = Math.min(horizon, hitAt ?? horizon, resetAt ?? horizon);
  const forecastPct = Math.min(100, latest.pct + rate * Math.max(0, forecastAt - latest.time) / 3600000);
  const pointMarkers = sampledPoints(points, 240);
  const xTicks = usageAxisTicks(start, horizon);
  const scope = `${usageProviderLabel(series.provider)} · ${usageWindowLabel(series.window_name, series.duration_seconds)} window`;
  return html`<svg class="usage-line-chart" viewBox=${`0 0 ${W} ${H}`} role="img"
    aria-label=${`${series.provider} ${series.window_name} subscription usage history`}>
    ${[0, 50, 100].map((tick) => html`<g class="usage-grid" key=${tick}>
      <line x1=${PAD.left} x2=${W - PAD.right} y1=${y(tick)} y2=${y(tick)} />
      <text x=${PAD.left - 8} y=${y(tick) + 4} text-anchor="end">${tick}%</text>
    </g>`)}
    ${xTicks.map((tick, index) => html`<g class="usage-x-tick" key=${tick.time}>
      <line x1=${x(tick.time)} x2=${x(tick.time)} y1=${PAD.top} y2=${H - PAD.bottom} />
      <text x=${x(tick.time)} y=${H - 6}
        text-anchor=${index === 0 ? 'start' : index === xTicks.length - 1 ? 'end' : 'middle'}>
        ${formatUsageAxisTick(tick.time, start, horizon)}
      </text>
    </g>`)}
    ${segments.map((segment, index) => html`<polyline key=${index} class="usage-observed-line"
      points=${segment.map((point) => `${x(point.time)},${y(point.pct)}`).join(' ')} />`)}
    ${(series.resets || []).map((reset) => {
      const at = finiteDate(reset.at);
      if (at === null) return null;
      return html`<g class="usage-reset-mark" key=${reset.at}>
        <line x1=${x(at)} x2=${x(at)} y1=${PAD.top} y2=${H - PAD.bottom} />
        <circle cx=${x(at)} cy=${y(reset.pct)} r="3"><title>${`${scope} · reset detected · ${reset.pct.toFixed(1)}% · ${new Date(at).toLocaleString()}`}</title></circle>
      </g>`;
    })}
    ${rate > 0 && forecastAt > latest.time && html`<line class="usage-forecast-line"
      x1=${x(latest.time)} y1=${y(latest.pct)} x2=${x(forecastAt)} y2=${y(forecastPct)}>
      <title>${`${scope} · forecast ${forecastPct.toFixed(1)}% · ${new Date(forecastAt).toLocaleString()}`}</title>
    </line>`}
    ${pointMarkers.map((point) => html`<circle class="usage-point" key=${point.at} cx=${x(point.time)} cy=${y(point.pct)} r="2.5">
      <title>${`${scope} · ${point.pct.toFixed(1)}% · ${new Date(point.time).toLocaleString()}${point.source ? ` · source: ${point.source}` : ''}`}</title>
    </circle>`)}
    ${projecting && now > start && now < horizon && html`<g class="usage-now-mark">
      <line x1=${x(now)} x2=${x(now)} y1=${PAD.top} y2=${H - PAD.bottom} />
      <text x=${x(now)} y=${PAD.top + 10} text-anchor="middle">now</text>
    </g>`}
  </svg>`;
}
