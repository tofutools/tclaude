import { h } from 'preact';
import htm from 'htm';
import { fmtAxisUSD, fmtExactUSD } from './costs-model.js';

const html = htm.bind(h);
const W = 1000;
const H = 190;
const PAD = { left: 48, right: 16, top: 15, bottom: 28 };

export function CostsAccumulatedChart({ chart }) {
  const points = chart?.points || [];
  if (!points.length) return html`<div id="costs-accumulated-chart" class="cost-accumulated empty">No days in span.</div>`;
  if (!(chart.scaleMax > 0)) return html`<div id="costs-accumulated-chart" class="cost-accumulated empty">No cost recorded for the selected providers and models.</div>`;
  const x = (index) => PAD.left + (points.length === 1 ? 0 : index / (points.length - 1)) * (W - PAD.left - PAD.right);
  const y = (value) => PAD.top + (1 - value / chart.scaleMax) * (H - PAD.top - PAD.bottom);
  const line = points.map((point, index) => `${x(index)},${y(point.cost)}`).join(' ');
  const area = `${PAD.left},${H - PAD.bottom} ${line} ${x(points.length - 1)},${H - PAD.bottom}`;
  const labelEvery = points.length > 62 ? 14 : points.length > 35 ? 7 : points.length > 14 ? 3 : 1;
  return html`<div id="costs-accumulated-chart" class="cost-accumulated">
    <div class="cost-chart-heading"><strong>Accumulated cost</strong><span>selected providers + models</span></div>
    <svg class="cost-accumulated-svg" viewBox=${`0 0 ${W} ${H}`} role="img"
      aria-label=${`Accumulated cost from ${points[0].day} through ${points[points.length - 1].day}: ${fmtExactUSD(points[points.length - 1].cost)}`}>
      <polygon class="cost-accumulated-area" points=${area} />
      ${[0, .5, 1].map((ratio) => html`<g class="cost-accumulated-grid" key=${ratio}>
        <line x1=${PAD.left} x2=${W - PAD.right} y1=${y(chart.scaleMax * ratio)} y2=${y(chart.scaleMax * ratio)} />
        <text x=${PAD.left - 7} y=${y(chart.scaleMax * ratio) + 4} text-anchor="end">${fmtAxisUSD(chart.scaleMax * ratio)}</text>
      </g>`)}
      <polyline class="cost-accumulated-line" points=${line} />
      ${points.map((point, index) => index % labelEvery === 0 || index === points.length - 1
        ? html`<text class="cost-accumulated-day" x=${x(index)} y=${H - 7}
          text-anchor=${index === 0 ? 'start' : index === points.length - 1 ? 'end' : 'middle'}>${point.day.slice(5)}</text>` : null)}
      ${points.map((point, index) => html`<circle class="cost-accumulated-point" cx=${x(index)} cy=${y(point.cost)} r="2">
        <title>${point.day} — ${fmtExactUSD(point.cost)} accumulated</title>
      </circle>`)}
    </svg>
  </div>`;
}
