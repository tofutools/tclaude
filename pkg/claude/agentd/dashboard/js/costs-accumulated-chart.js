import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { fmtAxisUSD, fmtExactUSD } from './costs-model.js';

const html = htm.bind(h);
const DEFAULT_W = 1000;
const H = 190;
const PAD = { left: 48, right: 16, top: 15, bottom: 28 };
const TIP_WIDTH = 288;
const TIP_PAD = 14;

function stackParts(points) {
  const result = [];
  let current = [];
  for (const point of points) {
    if (current.length && current[current.length - 1].projected !== point.projected) {
      const previous = current[current.length - 1];
      result.push({ projected: previous.projected, points: current });
      current = [previous];
    }
    current.push(point);
  }
  if (current.length) result.push({ projected: current[current.length - 1].projected, points: current });
  return result;
}

function visibleBoundary(points) {
  const first = points.findIndex((point) => point.upper > point.lower);
  return first < 0 ? [] : points.slice(Math.max(0, first - 1));
}

function breakdownLabel(item, chart) {
  const parts = [];
  if (chart.stackByProvider && item.provider) parts.push(item.provider);
  if (chart.stackByModel && item.model) parts.push(item.model);
  if (!parts.length) parts.push('cost');
  if (item.kind === 'what_if') parts.push('WHAT-IF');
  return parts.join(' · ');
}

function AccumulatedTip({ description, chart }) {
  const panel = useRef(null);
  const [position, setPosition] = useState(null);
  const { point, projected } = description;
  const rows = point.breakdown || [];
  useLayoutEffect(() => {
    const node = panel.current;
    if (!node) return;
    const rect = node.getBoundingClientRect();
    const panelWidth = rect.width || TIP_WIDTH;
    const panelHeight = rect.height || node.offsetHeight || 0;
    const anchor = description.anchor;
    let left = anchor.x + TIP_PAD;
    let top = anchor.y + TIP_PAD;
    if (left + panelWidth > window.innerWidth - 4) left = anchor.x - TIP_PAD - panelWidth;
    if (top + panelHeight > window.innerHeight - 4) top = anchor.y - TIP_PAD - panelHeight;
    const next = { left: Math.max(4, left), top: Math.max(4, top) };
    setPosition((current) => current?.left === next.left && current?.top === next.top ? current : next);
  }, [description]);
  const style = position
    ? `left:${position.left}px;top:${position.top}px`
    : `left:${description.anchor.x}px;top:${description.anchor.y}px;visibility:hidden`;
  return html`<div ref=${panel} class=${`cost-accumulated-tip-panel${projected ? ' projected' : ''}`} style=${style}>
    <strong>${point.day} · ${projected ? 'projection' : 'recorded'}</strong>
    ${chart.stackByProvider && chart.stackByModel
      ? [...new Set(rows.map((row) => row.provider))].map((provider) => {
        const children = rows.filter((row) => row.provider === provider);
        return html`<div class="cost-accumulated-tip-group" key=${provider}>
          <div class="cost-accumulated-tip-group-head"><span></span><b>${provider}</b><b>${projected ? '≈' : ''}${fmtExactUSD(children.reduce((sum, row) => sum + row.cost, 0))}</b></div>
          ${children.map((row) => html`<div class=${`cost-accumulated-tip-row child ${row.className}`} key=${row.key}>
            <i class="cost-accumulated-tip-sw"></i>
            <span>${breakdownLabel({ ...row, provider: '' }, chart)}</span>
            <span>${projected ? '≈' : ''}${fmtExactUSD(row.cost)}</span>
          </div>`)}
        </div>`;
      })
      : rows.map((row) => html`<div class=${`cost-accumulated-tip-row ${row.className}`} key=${row.key}>
        <i class="cost-accumulated-tip-sw"></i>
        <span>${breakdownLabel(row, chart)}</span><span>${projected ? '≈' : ''}${fmtExactUSD(row.cost)}</span>
      </div>`)}
    <div class="cost-accumulated-tip-total"><span></span><b>Accumulated total</b><b>${projected ? '≈' : ''}${fmtExactUSD(point.cost)}</b></div>
    <small>${projected ? `Daily projection ~${fmtExactUSD(point.dailyCost)} · split from recorded mix` : `Daily spend +${fmtExactUSD(point.dailyCost)}`}</small>
  </div>`;
}

export function CostsAccumulatedChart({ chart }) {
  const host = useRef(null);
  const [width, setWidth] = useState(DEFAULT_W);
  const [tooltip, setTooltip] = useState(null);
  const [announcement, setAnnouncement] = useState('');
  useEffect(() => {
    setTooltip(null);
    setAnnouncement('');
  }, [chart]);
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
  const describePoint = (point, projected = point.projected, anchor = null) => ({
    point, projected, x: x(point.index), y: y(point.cost), anchor,
  });
  const pointSummary = ({ point, projected }) => {
    const breakdown = (point.breakdown || []).map((item) =>
      `${breakdownLabel(item, chart)} ${projected ? 'approximately ' : ''}${fmtExactUSD(item.cost)}`).join(', ');
    return `${point.day}, ${projected ? 'projection' : 'recorded'}, ${fmtExactUSD(point.cost)} accumulated, ${projected ? 'approximately ' : ''}${fmtExactUSD(point.dailyCost)} that day.${breakdown ? ` Breakdown: ${breakdown}.` : ''}`;
  };
  const inspectPoint = (description, announce = false) => {
    let anchored = description;
    if (!description.anchor) {
      const svg = host.current?.querySelector('.cost-accumulated-svg');
      const rect = svg?.getBoundingClientRect();
      anchored = { ...description, anchor: {
        x: (rect?.left || 0) + description.x * (rect?.width || width) / width,
        y: (rect?.top || 0) + description.y * (rect?.height || H) / H,
      } };
    }
    setTooltip(anchored);
    if (announce) setAnnouncement(pointSummary(description));
  };
  const showTooltip = (event, announce = false) => {
    const svg = event.currentTarget.ownerSVGElement || event.currentTarget.closest('svg');
    const rect = svg.getBoundingClientRect();
    const cursorX = (event.clientX - rect.left) * width / Math.max(rect.width, 1);
    const point = points.reduce((nearest, candidate) =>
      Math.abs(x(candidate.index) - cursorX) < Math.abs(x(nearest.index) - cursorX) ? candidate : nearest);
    inspectPoint(describePoint(point, point.projected, { x: event.clientX, y: event.clientY }), announce);
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
  return html`<div ref=${host} id="costs-accumulated-chart" class="cost-accumulated">
    <div class="cost-chart-heading"><strong>Accumulated cost</strong><span><i class="cost-line-key recorded"></i> recorded <i class="cost-line-key projected"></i> projection</span></div>
    <svg class="cost-accumulated-svg" viewBox=${`0 0 ${width} ${H}`} role="img" tabIndex="0"
      aria-label=${accessibleSummary} onfocus=${() => inspectPoint(describePoint(lastRecorded), true)}
      onblur=${() => { setTooltip(null); setAnnouncement(''); }} onkeydown=${navigateTooltip}>
      ${!(chart.stacks || []).length && (chart.segments || []).filter((segment) => !segment.projected && segment.points.length > 1).map((segment, index) => {
        const area = `${x(segment.points[0].index)},${H - PAD.bottom} ${line(segment.points)} ${x(segment.points[segment.points.length - 1].index)},${H - PAD.bottom}`;
        return html`<polygon key=${`area-${index}`} class="cost-accumulated-area" points=${area} />`;
      })}
      ${(chart.stacks || []).flatMap((stack) => stackParts(stack.points).map((part, index) => {
        if (part.points.length < 2) return null;
        const upper = part.points.map((point) => `${x(point.index)},${y(point.upper)}`).join(' ');
        const lower = [...part.points].reverse().map((point) => `${x(point.index)},${y(point.lower)}`).join(' ');
        const boundary = visibleBoundary(part.points);
        return html`<g key=${`stack-${stack.key}-${index}`}>
          <polygon class=${`cost-accumulated-stack ${stack.className}${part.projected ? ' projected' : ''}`}
            points=${`${upper} ${lower}`} />
          ${boundary.length > 1 && html`<polyline
            class=${`cost-accumulated-stack-line ${stack.className}${part.projected ? ' projected' : ''}`}
            points=${boundary.map((point) => `${x(point.index)},${y(point.upper)}`).join(' ')} />`}
        </g>`;
      }))}
      ${[0, .5, 1].map((ratio) => html`<g class="cost-accumulated-grid" key=${ratio}>
        <line x1=${PAD.left} x2=${width - PAD.right} y1=${y(chart.scaleMax * ratio)} y2=${y(chart.scaleMax * ratio)} />
        <text x=${PAD.left - 7} y=${y(chart.scaleMax * ratio) + 4} text-anchor="end">${fmtAxisUSD(chart.scaleMax * ratio)}</text>
      </g>`)}
      ${(chart.segments || []).map((segment, index) => html`<g key=${`line-${index}`}>
        ${!(chart.stacks || []).length && html`<polyline class=${`cost-accumulated-line${segment.projected ? ' projected' : ''}`} points=${line(segment.points)} />`}
      </g>`)}
      ${points.map((point, index) => index % labelEvery === 0 || index === points.length - 1
        ? html`<text class="cost-accumulated-day" x=${x(index)} y=${H - 7}
          text-anchor=${index === 0 ? 'start' : index === points.length - 1 ? 'end' : 'middle'}>${point.day.slice(5)}</text>` : null)}
      <rect class="cost-accumulated-hover-target" x=${PAD.left} y=${PAD.top}
        width=${width - PAD.left - PAD.right} height=${H - PAD.top - PAD.bottom}
        onmousemove=${showTooltip} onpointerdown=${(event) => showTooltip(event, true)}
        onmouseleave=${(event) => { if (document.activeElement !== event.currentTarget.closest('svg')) setTooltip(null); }} />
      ${tooltip && html`<g class=${`cost-accumulated-tooltip${tooltip.projected ? ' projected' : ''}`} pointer-events="none">
        <line x1=${tooltip.x} x2=${tooltip.x} y1=${PAD.top} y2=${H - PAD.bottom} />
        <circle cx=${tooltip.x} cy=${tooltip.y} r="4" />
        <text aria-hidden="true" opacity="0">${tooltip.point.day} · ${tooltip.projected ? 'projection' : 'recorded'}</text>
      </g>`}
    </svg>
    ${tooltip && html`<${AccumulatedTip} description=${tooltip} chart=${chart} />`}
    <div class="cost-accumulated-status" role="status" aria-live="polite" aria-atomic="true">${announcement}</div>
  </div>`;
}
