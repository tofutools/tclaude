import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { fmtAxisUSD, fmtExactUSD } from './costs-model.js';

const html = htm.bind(h);
const DEFAULT_W = 1000;
const H = 190;
const PAD = { left: 48, right: 16, top: 15, bottom: 28 };

export function CostsAccumulatedChart({ chart }) {
  const host = useRef(null);
  const [width, setWidth] = useState(DEFAULT_W);
  const [tooltip, setTooltip] = useState(null);
  const [announcement, setAnnouncement] = useState('');
  useEffect(() => {
    const node = host.current;
    if (!node) return undefined;
    const update = () => setWidth(Math.max(320, Math.round(node.clientWidth || DEFAULT_W)));
    update();
    if (typeof ResizeObserver === 'undefined') {
      window.addEventListener?.('resize', update);
      return () => window.removeEventListener?.('resize', update);
    }
    const observer = new ResizeObserver(update);
    observer.observe(node);
    return () => observer.disconnect();
  }, []);
  const points = chart?.points || [];
  if (!points.length) return html`<div ref=${host} id="costs-accumulated-chart" class="cost-accumulated empty">No days in span.</div>`;
  if (!(chart.scaleMax > 0)) return html`<div ref=${host} id="costs-accumulated-chart" class="cost-accumulated empty">No cost recorded for the selected providers and models.</div>`;
  const x = (index) => PAD.left + (points.length === 1 ? 0 : index / (points.length - 1)) * (width - PAD.left - PAD.right);
  const y = (value) => PAD.top + (1 - value / chart.scaleMax) * (H - PAD.top - PAD.bottom);
  const line = (items) => items.map((point) => `${x(point.index)},${y(point.cost)}`).join(' ');
  const labelEvery = points.length > 62 ? 14 : points.length > 35 ? 7 : points.length > 14 ? 3 : 1;
  const lastRecorded = points.reduce((latest, point) => point.projected ? latest : point, points[0]);
  const lastPoint = points[points.length - 1];
  const describePoint = (point, projected = point.projected) => ({
    point, projected, x: x(point.index), y: y(point.cost),
  });
  const pointSummary = ({ point, projected }) => `${point.day}, ${projected ? 'projection' : 'recorded'}, ${fmtExactUSD(point.cost)} accumulated, ${projected ? 'approximately ' : ''}${fmtExactUSD(point.dailyCost)} that day.`;
  const inspectPoint = (description, announce = false) => {
    setTooltip(description);
    if (announce) setAnnouncement(pointSummary(description));
  };
  const showTooltip = (event, segment, announce = false) => {
    const svg = event.currentTarget.ownerSVGElement || event.currentTarget.closest('svg');
    const rect = svg.getBoundingClientRect();
    const cursorX = (event.clientX - rect.left) * width / Math.max(rect.width, 1);
    const point = segment.points.reduce((nearest, candidate) =>
      Math.abs(x(candidate.index) - cursorX) < Math.abs(x(nearest.index) - cursorX) ? candidate : nearest);
    inspectPoint(describePoint(point, segment.projected), announce);
  };
  const navigateTooltip = (event) => {
    const moves = { ArrowLeft: -1, ArrowRight: 1 };
    if (!(event.key in moves) && event.key !== 'Home' && event.key !== 'End') return;
    event.preventDefault();
    const current = tooltip?.point.index ?? lastRecorded.index;
    const index = event.key === 'Home' ? 0 : event.key === 'End' ? points.length - 1
      : Math.max(0, Math.min(points.length - 1, current + moves[event.key]));
    inspectPoint(describePoint(points[index]), true);
  };
  const accessibleSummary = lastPoint.projected
    ? `Accumulated cost. Recorded through ${lastRecorded.day}: ${fmtExactUSD(lastRecorded.cost)}. Projected through ${lastPoint.day}: ${fmtExactUSD(lastPoint.cost)}. Focus and use Left and Right Arrow keys to inspect daily values.`
    : `Accumulated cost recorded through ${lastPoint.day}: ${fmtExactUSD(lastPoint.cost)}. Focus and use Left and Right Arrow keys to inspect daily values.`;
  const tipWidth = 218;
  const tipX = tooltip ? Math.max(PAD.left, Math.min(width - PAD.right - tipWidth, tooltip.x + 10)) : 0;
  return html`<div ref=${host} id="costs-accumulated-chart" class="cost-accumulated">
    <div class="cost-chart-heading"><strong>Accumulated cost</strong><span><i class="cost-line-key recorded"></i> recorded <i class="cost-line-key projected"></i> projection</span></div>
    <svg class="cost-accumulated-svg" viewBox=${`0 0 ${width} ${H}`} role="img" tabIndex="0"
      aria-label=${accessibleSummary} onfocus=${() => inspectPoint(describePoint(lastRecorded), true)}
      onblur=${() => { setTooltip(null); setAnnouncement(''); }} onkeydown=${navigateTooltip}>
      ${(chart.segments || []).filter((segment) => !segment.projected && segment.points.length > 1).map((segment, index) => {
        const area = `${x(segment.points[0].index)},${H - PAD.bottom} ${line(segment.points)} ${x(segment.points[segment.points.length - 1].index)},${H - PAD.bottom}`;
        return html`<polygon key=${`area-${index}`} class="cost-accumulated-area" points=${area} />`;
      })}
      ${[0, .5, 1].map((ratio) => html`<g class="cost-accumulated-grid" key=${ratio}>
        <line x1=${PAD.left} x2=${width - PAD.right} y1=${y(chart.scaleMax * ratio)} y2=${y(chart.scaleMax * ratio)} />
        <text x=${PAD.left - 7} y=${y(chart.scaleMax * ratio) + 4} text-anchor="end">${fmtAxisUSD(chart.scaleMax * ratio)}</text>
      </g>`)}
      ${(chart.segments || []).map((segment, index) => html`<g key=${`line-${index}`}>
        <polyline class=${`cost-accumulated-line${segment.projected ? ' projected' : ''}`} points=${line(segment.points)} />
        <polyline class="cost-accumulated-hit" points=${line(segment.points)}
          onmousemove=${(event) => showTooltip(event, segment)} onpointerdown=${(event) => showTooltip(event, segment, true)}
          onmouseleave=${(event) => { if (document.activeElement !== event.currentTarget.closest('svg')) setTooltip(null); }} />
      </g>`)}
      ${points.map((point, index) => index % labelEvery === 0 || index === points.length - 1
        ? html`<text class="cost-accumulated-day" x=${x(index)} y=${H - 7}
          text-anchor=${index === 0 ? 'start' : index === points.length - 1 ? 'end' : 'middle'}>${point.day.slice(5)}</text>` : null)}
      ${tooltip && html`<g class=${`cost-accumulated-tooltip${tooltip.projected ? ' projected' : ''}`} pointer-events="none">
        <line x1=${tooltip.x} x2=${tooltip.x} y1=${PAD.top} y2=${H - PAD.bottom} />
        <circle cx=${tooltip.x} cy=${tooltip.y} r="4" />
        <rect x=${tipX} y=${Math.max(PAD.top + 3, tooltip.y - 56)} width=${tipWidth} height="50" rx="4" />
        <text x=${tipX + 9} y=${Math.max(PAD.top + 18, tooltip.y - 41)}>
          <tspan font-weight="600">${tooltip.point.day} · ${tooltip.projected ? 'projection' : 'recorded'}</tspan>
          <tspan x=${tipX + 9} dy="16">${fmtExactUSD(tooltip.point.cost)} accumulated · ${tooltip.projected ? '~' : '+'}${fmtExactUSD(tooltip.point.dailyCost)} day</tspan>
        </text>
      </g>`}
    </svg>
    <div class="cost-accumulated-status" role="status" aria-live="polite" aria-atomic="true">${announcement}</div>
  </div>`;
}
