import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);
const W = 720;
const H = 210;
const PAD = { left: 42, right: 18, top: 14, bottom: 28 };

function finiteDate(value) {
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
  const requestedStart = finiteDate(from) ?? points[0].time;
  const now = finiteDate(generatedAt) ?? points[points.length - 1].time;
  const observedWidth = Math.max(15 * 60_000, points[points.length - 1].time - points[0].time);
  // Do not compress a new install's first hour into the final pixel of a 7d
  // selection. The range remains the requested upper bound, but the plot starts
  // where observations actually begin until enough history accumulates.
  const start = Math.max(requestedStart, points[0].time - observedWidth * 0.05);
  const historyWidth = Math.max(60_000, now - start);
  const forecast = series.forecast || {};
  const rate = Number(forecast.rate_pct_per_hour) || 0;
  const hitAt = finiteDate(forecast.hits_limit_at);
  const resetAt = finiteDate(forecast.reset_at);
  const projecting = rate > 0 && ['before_reset', 'after_reset', 'projected'].includes(forecast.status);
  const eventAt = forecast.status === 'after_reset'
    ? resetAt
    : ['before_reset', 'projected'].includes(forecast.status) ? (hitAt ?? resetAt) : null;
  const horizon = projecting ? Math.max(now + historyWidth * 0.18, eventAt && eventAt > now ? eventAt : 0) : now;
  const x = (time) => PAD.left + Math.max(0, Math.min(1, (time - start) / (horizon - start))) * (W - PAD.left - PAD.right);
  const y = (pct) => PAD.top + (1 - Math.max(0, Math.min(100, pct)) / 100) * (H - PAD.top - PAD.bottom);
  const resetTimes = new Set((series.resets || []).map((reset) => finiteDate(reset.at)));
  const segments = [];
  let current = [];
  for (const point of points) {
    const previous = current[current.length - 1];
    if ((resetTimes.has(point.time) || (previous && previous.pct - point.pct >= 2)) && current.length) {
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
  return html`<svg class="usage-line-chart" viewBox=${`0 0 ${W} ${H}`} role="img"
    aria-label=${`${series.provider} ${series.window_name} subscription usage history`}>
    ${[0, 50, 100].map((tick) => html`<g class="usage-grid" key=${tick}>
      <line x1=${PAD.left} x2=${W - PAD.right} y1=${y(tick)} y2=${y(tick)} />
      <text x=${PAD.left - 8} y=${y(tick) + 4} text-anchor="end">${tick}%</text>
    </g>`)}
    ${segments.map((segment, index) => html`<polyline key=${index} class="usage-observed-line"
      points=${segment.map((point) => `${x(point.time)},${y(point.pct)}`).join(' ')} />`)}
    ${(series.resets || []).map((reset) => {
      const at = finiteDate(reset.at);
      if (at === null) return null;
      return html`<g class="usage-reset-mark" key=${reset.at}>
        <line x1=${x(at)} x2=${x(at)} y1=${PAD.top} y2=${H - PAD.bottom} />
        <circle cx=${x(at)} cy=${y(reset.pct)} r="3"><title>${`Reset detected · ${reset.pct.toFixed(1)}% · ${new Date(at).toLocaleString()}`}</title></circle>
      </g>`;
    })}
    ${rate > 0 && forecastAt > latest.time && html`<line class="usage-forecast-line"
      x1=${x(latest.time)} y1=${y(latest.pct)} x2=${x(forecastAt)} y2=${y(forecastPct)} />`}
    ${pointMarkers.map((point) => html`<circle class="usage-point" key=${point.at} cx=${x(point.time)} cy=${y(point.pct)} r="2.5">
      <title>${`${point.pct.toFixed(1)}% · ${new Date(point.time).toLocaleString()}${point.source ? ` · ${point.source}` : ''}`}</title>
    </circle>`)}
    <text class="usage-x-label" x=${PAD.left} y=${H - 6}>${new Date(start).toLocaleDateString()}</text>
    <text class="usage-x-label" x=${x(now)} y=${H - 6} text-anchor="end">now</text>
    ${projecting && html`<text class="usage-x-label forecast" x=${W - PAD.right} y=${H - 6} text-anchor="end">forecast</text>`}
  </svg>`;
}
