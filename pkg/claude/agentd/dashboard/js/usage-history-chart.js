import { Fragment, h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import {
  formatUsageAxisTick, usageAxisStart, usageAxisTicks, usageForecastPoint,
} from './usage-history-axis.js';
import {
  formatUsageDuration, formatUsageTime, usageProviderLabel, usageWindowLabel,
} from './usage-history-model.js';

const html = htm.bind(h);
const W = 720;
const H = 210;
const PAD = { left: 42, right: 18, top: 14, bottom: 28 };
const TOOLTIP_WIDTH = 270;
const TOOLTIP_LINE_HEIGHT = 13;

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

function beforeResetLabel(resetAt, time) {
  if (resetAt === null) return 'reset time unknown';
  const delta = resetAt - time;
  if (Math.abs(delta) <= 60_000) return 'at reset';
  return delta > 0
    ? `${formatUsageDuration(delta)} before reset`
    : `${formatUsageDuration(delta)} after reset`;
}

function relativeMarkerTime(at, now) {
  const delta = at - now;
  if (Math.abs(delta) <= 60_000) return 'now';
  return delta > 0 ? `${formatUsageDuration(delta)} remaining` : `${formatUsageDuration(delta)} ago`;
}

function resetTimingLabel(resetAt, now) {
  if (resetAt === null) return 'Reset time unknown';
  const delta = resetAt - now;
  if (Math.abs(delta) <= 60_000) return 'Quota resets now';
  return delta > 0
    ? `Quota resets in ${formatUsageDuration(delta)}`
    : `Reported quota reset ${formatUsageDuration(delta)} ago`;
}

export function UsageHistoryChart({ series, from, generatedAt, lookaheadHours = 168 }) {
  const [tooltip, setTooltip] = useState(null);
  const [keyboardPointAt, setKeyboardPointAt] = useState(null);
  const [keyboardResetAt, setKeyboardResetAt] = useState(null);
  const points = (series.points || []).map((point) => ({ ...point, time: finiteDate(point.at) })).filter((point) => point.time !== null);
  if (!points.length) return html`<div class="usage-chart-empty">No samples in this range.</div>`;
  const now = finiteDate(generatedAt) ?? points[points.length - 1].time;
  const start = usageAxisStart(finiteDate(from), points[0].time, now);
  const forecast = series.forecast || {};
  const rate = Number(forecast.rate_pct_per_hour) || 0;
  const hitAt = finiteDate(forecast.hits_limit_at);
  const resetAt = finiteDate(forecast.reset_at);
  const projecting = rate > 0 && ['before_reset', 'after_reset', 'projected'].includes(forecast.status);
  const lookahead = [5, 24, 168, 720].includes(Number(lookaheadHours)) ? Number(lookaheadHours) : 168;
  const horizon = now + lookahead * 3600000;
  const x = (time) => PAD.left + Math.max(0, Math.min(1, (time - start) / (horizon - start))) * (W - PAD.left - PAD.right);
  const y = (pct) => PAD.top + (1 - Math.max(0, Math.min(100, pct)) / 100) * (H - PAD.top - PAD.bottom);
  const resetMarkers = (series.resets || [])
    .map((reset) => ({ ...reset, time: finiteDate(reset.at) }))
    .filter((reset) => reset.time !== null);
  const resetTimes = new Set(resetMarkers.map((reset) => reset.time));
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
  const hasForecastLine = projecting && forecastAt > latest.time;
  const scheduledResetVisible = resetAt !== null && resetAt > now && resetAt <= horizon;
  const pointMarkers = sampledPoints(points, 240);
  const keyboardPointIndex = Number.isInteger(keyboardPointAt) && keyboardPointAt < pointMarkers.length
    ? keyboardPointAt
    : pointMarkers.length - 1;
  const keyboardResetIndex = Number.isInteger(keyboardResetAt) && keyboardResetAt < resetMarkers.length
    ? keyboardResetAt
    : resetMarkers.length - 1;
  const xTicks = usageAxisTicks(start, horizon);
  const scope = `${usageProviderLabel(series.provider)} · ${usageWindowLabel(series.window_name, series.duration_seconds)} window`;
  const updateForecastHover = (event) => {
    const svg = event.currentTarget.ownerSVGElement || event.currentTarget.closest('svg');
    const rect = svg?.getBoundingClientRect();
    if (!rect?.width) return;
    const svgX = (event.clientX - rect.left) * W / rect.width;
    const firstX = x(latest.time);
    const lastX = x(forecastAt);
    const hoverPoint = usageForecastPoint(latest.time, latest.pct, rate, forecastAt,
      (svgX - firstX) / Math.max(1, lastX - firstX));
    setTooltip({
      x: x(hoverPoint.time), y: y(hoverPoint.pct), tone: 'forecast',
      lines: [
        scope,
        `Forecast · ${hoverPoint.pct.toFixed(1)}% · ${new Date(hoverPoint.time).toLocaleString()}`,
        beforeResetLabel(resetAt, hoverPoint.time),
      ],
    });
  };
  const showTooltip = (anchorX, anchorY, tone, lines) => setTooltip({ x: anchorX, y: anchorY, tone, lines });
  const hideTooltip = () => setTooltip(null);
  const focusChartItemByKey = (event, index, selector, length) => {
    let target = index;
    if (event.key === 'ArrowLeft') target = Math.max(0, index - 1);
    else if (event.key === 'ArrowRight') target = Math.min(length - 1, index + 1);
    else if (event.key === 'Home') target = 0;
    else if (event.key === 'End') target = length - 1;
    else return;
    event.preventDefault();
    const svg = event.currentTarget.ownerSVGElement || event.currentTarget.closest('svg');
    svg?.querySelectorAll(selector)[target]?.focus();
  };
  const tooltipHeight = tooltip ? 8 + tooltip.lines.length * TOOLTIP_LINE_HEIGHT : 0;
  const tooltipX = tooltip?.x > W / 2 ? tooltip.x - TOOLTIP_WIDTH - 10 : (tooltip?.x || 0) + 10;
  const tooltipY = tooltip
    ? Math.max(4, Math.min(H - tooltipHeight - 4, tooltip.y < tooltipHeight + 8 ? tooltip.y + 10 : tooltip.y - tooltipHeight - 8))
    : 0;
  return html`<svg class="usage-line-chart" viewBox=${`0 0 ${W} ${H}`} role="group"
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
    ${resetMarkers.map((reset, index) => {
      const at = reset.time;
      return html`<g class="usage-reset-mark" key=${reset.at}>
        <line x1=${x(at)} x2=${x(at)} y1=${PAD.top} y2=${H - PAD.bottom} />
        <circle cx=${x(at)} cy=${y(reset.pct)} r="3" />
        <line class="usage-marker-hit-target" x1=${x(at)} x2=${x(at)} y1=${PAD.top} y2=${H - PAD.bottom}
          tabIndex=${index === keyboardResetIndex ? '0' : '-1'} role="img"
          aria-label=${`${scope}; quota reset detected at ${new Date(at).toLocaleString()}; new post-reset baseline ${reset.pct.toFixed(1)}%; ${relativeMarkerTime(at, now)}${index === keyboardResetIndex ? '; use left and right arrow keys to explore detected resets' : ''}`}
          onmouseenter=${() => showTooltip(x(at), y(reset.pct), 'reset', [
            scope, `Quota reset detected · ${new Date(at).toLocaleString()}`,
            `New post-reset baseline: ${reset.pct.toFixed(1)}% · ${relativeMarkerTime(at, now)}`,
          ])} onmouseleave=${hideTooltip}
          onfocus=${() => {
            setKeyboardResetAt(index);
            showTooltip(x(at), y(reset.pct), 'reset', [
              scope, `Quota reset detected · ${new Date(at).toLocaleString()}`,
              `New post-reset baseline: ${reset.pct.toFixed(1)}% · ${relativeMarkerTime(at, now)}`,
            ]);
          }} onblur=${hideTooltip}
          onkeydown=${(event) => focusChartItemByKey(event, index, '.usage-reset-mark .usage-marker-hit-target', resetMarkers.length)} />
      </g>`;
    })}
    ${scheduledResetVisible && html`<g class="usage-scheduled-reset">
      <line x1=${x(resetAt)} x2=${x(resetAt)} y1=${PAD.top} y2=${H - PAD.bottom} />
      <line class="usage-marker-hit-target" x1=${x(resetAt)} x2=${x(resetAt)} y1=${PAD.top} y2=${H - PAD.bottom}
        tabIndex="0" role="img"
        aria-label=${`${scope}; scheduled quota reset at ${new Date(resetAt).toLocaleString()}; ${relativeMarkerTime(resetAt, now)}`}
        onmouseenter=${() => showTooltip(x(resetAt), PAD.top + 18, 'reset', [
          scope, `Scheduled quota reset · ${new Date(resetAt).toLocaleString()}`, relativeMarkerTime(resetAt, now),
        ])} onmouseleave=${hideTooltip}
        onfocus=${() => showTooltip(x(resetAt), PAD.top + 18, 'reset', [
          scope, `Scheduled quota reset · ${new Date(resetAt).toLocaleString()}`, relativeMarkerTime(resetAt, now),
        ])} onblur=${hideTooltip} />
    </g>`}
    ${hasForecastLine && html`<${Fragment}>
      <line class="usage-forecast-line" x1=${x(latest.time)} y1=${y(latest.pct)}
        x2=${x(forecastAt)} y2=${y(forecastPct)} />
      <line class="usage-forecast-hit-target" x1=${x(latest.time)} y1=${y(latest.pct)}
        x2=${x(forecastAt)} y2=${y(forecastPct)} tabIndex="0" role="img"
        aria-label=${`${scope} forecast; ${forecastPct.toFixed(1)}% at ${new Date(forecastAt).toLocaleString()}; ${beforeResetLabel(resetAt, forecastAt)}`}
        onmousemove=${updateForecastHover} onmouseleave=${hideTooltip}
        onfocus=${() => showTooltip(x(forecastAt), y(forecastPct), 'forecast', [
          scope, `Forecast · ${forecastPct.toFixed(1)}% · ${new Date(forecastAt).toLocaleString()}`,
          beforeResetLabel(resetAt, forecastAt),
        ])} onblur=${hideTooltip} />
    </${Fragment}>`}
    ${pointMarkers.map((point, index) => {
      const pointResetLabel = beforeResetLabel(finiteDate(point.resets_at), point.time);
      return html`<circle class="usage-point" key=${point.at}
        cx=${x(point.time)} cy=${y(point.pct)} r="2.5"
        tabIndex=${index === keyboardPointIndex ? '0' : '-1'} role="img"
        aria-label=${`${scope}; ${point.pct.toFixed(1)}% at ${new Date(point.time).toLocaleString()}; ${pointResetLabel}${index === keyboardPointIndex ? '; use left and right arrow keys to explore samples' : ''}`}
        onmouseenter=${() => showTooltip(x(point.time), y(point.pct), 'observed', [
          scope, `${point.pct.toFixed(1)}% · ${new Date(point.time).toLocaleString()}`,
          pointResetLabel,
        ])} onmouseleave=${hideTooltip}
        onfocus=${() => {
          setKeyboardPointAt(index);
          showTooltip(x(point.time), y(point.pct), 'observed', [
            scope, `${point.pct.toFixed(1)}% · ${new Date(point.time).toLocaleString()}`,
            pointResetLabel,
          ]);
        }} onblur=${hideTooltip}
        onkeydown=${(event) => focusChartItemByKey(event, index, '.usage-point', pointMarkers.length)} />`;
    })}
    ${now > start && now < horizon && html`<g class="usage-now-mark">
      <line x1=${x(now)} x2=${x(now)} y1=${PAD.top} y2=${H - PAD.bottom} />
      <text x=${x(now)} y=${PAD.top + 10} text-anchor="middle">now</text>
      <line class="usage-marker-hit-target" x1=${x(now)} x2=${x(now)} y1=${PAD.top} y2=${H - PAD.bottom}
        tabIndex="0" role="img" aria-label=${`${scope}; now is ${new Date(now).toLocaleString()}; ${beforeResetLabel(resetAt, now)}`}
        onmouseenter=${() => showTooltip(x(now), PAD.top + 18, 'now', [
          scope, `Now · ${new Date(now).toLocaleString()}`,
          resetTimingLabel(resetAt, now),
        ])} onmouseleave=${hideTooltip}
        onfocus=${() => showTooltip(x(now), PAD.top + 18, 'now', [
          scope, `Now · ${new Date(now).toLocaleString()}`,
          resetTimingLabel(resetAt, now),
        ])} onblur=${hideTooltip} />
    </g>`}
    ${tooltip && html`<g class=${`usage-chart-tooltip ${tooltip.tone}`} transform=${`translate(${tooltipX} ${tooltipY})`}>
      <rect width=${TOOLTIP_WIDTH} height=${tooltipHeight} rx="4" />
      <text x="7" y="12">${tooltip.lines.map((line, index) => html`<tspan x="7" dy=${index ? TOOLTIP_LINE_HEIGHT : 0}>${line}</tspan>`)}
      </text>
    </g>`}
  </svg>`;
}
