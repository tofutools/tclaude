import { h } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { fmtAxisUSD, fmtCredits, fmtUSD, isWeekendKey } from './costs-model.js';

const html = htm.bind(h);

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function segmentName(segment, chart) {
  const parts = [];
  if (chart.stackByProvider !== false && segment.provider) parts.push(segment.provider);
  if (chart.stackByModel && segment.model) parts.push(segment.model);
  if (!parts.length) parts.push('cost');
  if (segment.kind === 'what_if') parts.push('WHAT-IF');
  return parts.join(' · ');
}

function appendTooltipSegment(fragment, segment, chart, child = false) {
  const row = element('div', `cost-tip-row${child ? ' child' : ''}`);
  row.append(element('span', `cost-tip-sw ${segment.className}`));
  row.append(element('span', 'cost-tip-name', segmentName(segment, chart)));
  const amount = segment.kind === 'what_if' && segment.credits > 0
    ? `${fmtCredits(segment.credits)} — ${fmtUSD(segment.cost)} subscription value`
    : `${segment.approximate || segment.kind === 'what_if' ? '≈' : ''}${fmtUSD(segment.cost)}`;
  row.append(element('span', 'cost-tip-amt', amount));
  fragment.append(row);
}

function tooltipRows(day, chart) {
  const fragment = document.createDocumentFragment();
  fragment.append(element('div', 'cost-tip-day', `${day.day}${day.projected ? ' · projection' : ''}`));
  if (chart.stackByProvider !== false && chart.stackByModel) {
    const providers = new Map();
    for (const segment of day.segments) {
      const group = providers.get(segment.provider) || [];
      group.push(segment);
      providers.set(segment.provider, group);
    }
    for (const [provider, segments] of providers) {
      const header = element('div', 'cost-tip-group');
      header.append(element('span', `cost-tip-sw ${segments[0].className}`),
        element('strong', 'cost-tip-name', provider),
        element('strong', 'cost-tip-amt', `${day.projected ? '≈' : ''}${fmtUSD(segments.reduce((sum, item) => sum + item.cost, 0))}`));
      fragment.append(header);
      for (const segment of segments) appendTooltipSegment(fragment, segment, chart, true);
    }
  } else {
    for (const segment of day.segments) appendTooltipSegment(fragment, segment, chart);
  }
  const total = element('div', 'cost-tip-total');
  const spacer = element('span', 'cost-tip-sw');
  spacer.style.visibility = 'hidden';
  total.append(spacer, element('span', 'cost-tip-name', 'total'), element('span', 'cost-tip-amt', fmtUSD(day.cost)));
  fragment.append(total);
  return fragment;
}

function daySummary(day, chart) {
  const breakdown = (day.segments || []).map((segment) =>
    `${segmentName(segment, chart)} ${segment.approximate ? 'approximately ' : ''}${fmtUSD(segment.cost)}`).join(', ');
  return `${day.day}, ${day.projected ? 'projection' : 'recorded'}, ${day.projected ? 'approximately ' : ''}${fmtUSD(day.cost)} total.${breakdown ? ` Breakdown: ${breakdown}.` : ''}`;
}

// This is the Costs island's sole imperative boundary. Preact owns the stable
// host; this adapter owns every chart descendant plus its body-level tooltip
// and listeners, returning one disposer that removes all of them together.
export function mountImperativeCostChart(host, chart) {
  host.replaceChildren();
  if (!chart?.days?.length) {
    host.append(element('div', 'empty', 'No days in span.'));
    return () => host.replaceChildren();
  }
  if (!(chart.scaleMax > 0)) {
    host.append(element('div', 'empty', 'No API cost recorded in this span. Cost is tracked only for agents on API/enterprise pricing (subscription sessions have no per-dollar cost).'));
    return () => host.replaceChildren();
  }

  const shell = element('div', 'cost-chart');
  const axis = element('div', 'cost-yaxis');
  const yArea = element('div', 'cost-yarea');
  const ticks = [
    { pct: 100, label: fmtAxisUSD(chart.scaleMax) },
    { pct: 50, label: fmtAxisUSD(chart.scaleMax / 2) },
    { pct: 0, label: '$0' },
  ];
  for (const tick of ticks) {
    const label = element('div', 'cost-ytick', tick.label);
    label.style.bottom = tick.pct + '%';
    yArea.append(label);
  }
  axis.append(yArea, element('div', 'cost-day'));
  const plot = element('div', 'cost-plot');
  const grid = element('div', 'cost-grid');
  for (const tick of ticks) {
    const line = element('div', 'cost-gridline');
    line.style.bottom = tick.pct + '%';
    grid.append(line);
  }
  const columns = element('div', 'cost-cols');
  const byDay = new Map();
  const spendColumns = [];
  const showBreakdown = chart.stackByProvider !== false || chart.stackByModel || chart.days.some((day) =>
    (day.segments || []).some((segment) => segment.kind === 'what_if'));
  const labelEvery = chart.days.length > 62 ? 7 : chart.days.length > 35 ? 2 : 1;
  chart.days.forEach((day, index) => {
    byDay.set(day.day, day);
    const column = element('div', `cost-col${isWeekendKey(day.day) ? ' weekend' : ''}${day.projected ? ' projected' : ''}`);
    if (day.cost > 0) {
      const hasWhatIf = day.segments?.some((segment) => segment.kind === 'what_if');
      column.dataset.tip = day.projected
        ? `${day.day} — projected ~${fmtUSD(day.cost)}${day.includesWhatIf ? ' · includes WHAT-IF estimates' : ''}`
        : `${day.day} — ${fmtUSD(day.cost)}${hasWhatIf ? ' · includes WHAT-IF estimates' : ''}`;
      column.dataset.day = day.day;
      column.setAttribute('tabindex', '0');
      column.setAttribute('role', 'img');
      column.setAttribute('aria-label', daySummary(day, chart));
      spendColumns.push(column);
    }
    const area = element('div', 'cost-bararea');
    if (day.projected && !day.segments?.length) {
      const bar = element('div', 'cost-bar');
      bar.style.height = Math.max(day.cost > 0 ? 2 : 0, Math.round(day.cost / chart.scaleMax * 100)) + '%';
      area.append(bar);
    } else {
      for (const segment of day.segments) {
        const bar = element('div', `cost-seg${day.projected ? ' cost-seg-projected' : ''} ${segment.className}`);
        bar.style.height = Math.max(segment.cost > 0 ? 1 : 0, segment.cost / chart.scaleMax * 100).toFixed(3) + '%';
        area.append(bar);
      }
    }
    const date = new Date(day.day + 'T12:00:00');
    column.append(area, element('div', 'cost-day', index % labelEvery === 0 ? String(date.getDate()) : ''));
    columns.append(column);
  });
  plot.append(grid, columns);
  shell.append(axis, plot);
  const status = element('div', 'cost-chart-status');
  status.setAttribute('role', 'status');
  status.setAttribute('aria-live', 'polite');
  status.setAttribute('aria-atomic', 'true');
  host.append(shell, status);

  let tooltip = null;
  const hide = () => { if (tooltip) tooltip.style.display = 'none'; };
  const show = (column, clientX, clientY, announce = false) => {
    if (!column) { hide(); return; }
    if (!tooltip) {
      tooltip = element('div', 'cost-tip');
      document.body.append(tooltip);
    }
    const day = byDay.get(column.dataset.day);
    tooltip.replaceChildren();
    if (showBreakdown && day?.segments?.length) tooltip.append(tooltipRows(day, chart));
    else tooltip.textContent = column.dataset.tip;
    if (announce) status.textContent = daySummary(day, chart);
    tooltip.style.display = 'block';
    const pad = 14;
    const rect = tooltip.getBoundingClientRect();
    let left = clientX + pad;
    let top = clientY + pad;
    if (left + rect.width > window.innerWidth - 4) left = clientX - pad - rect.width;
    if (top + rect.height > window.innerHeight - 4) top = clientY - pad - rect.height;
    tooltip.style.left = Math.max(4, left) + 'px';
    tooltip.style.top = Math.max(4, top) + 'px';
  };
  const move = (event) => show(event.target.closest?.('.cost-col[data-tip]'), event.clientX, event.clientY);
  const focus = (event) => {
    const column = event.target.closest?.('.cost-col[data-tip]');
    if (!column) return;
    const rect = column.getBoundingClientRect();
    show(column, rect.left + rect.width / 2, rect.top, true);
  };
  const navigate = (event) => {
    const moves = { ArrowLeft: -1, ArrowRight: 1 };
    if (!(event.key in moves) && event.key !== 'Home' && event.key !== 'End') return;
    const current = spendColumns.indexOf(event.target.closest?.('.cost-col[data-tip]'));
    if (current < 0) return;
    event.preventDefault();
    const index = event.key === 'Home' ? 0 : event.key === 'End' ? spendColumns.length - 1
      : Math.max(0, Math.min(spendColumns.length - 1, current + moves[event.key]));
    spendColumns[index].focus();
  };
  host.addEventListener('mousemove', move);
  host.addEventListener('mouseleave', hide);
  host.addEventListener('focusin', focus);
  host.addEventListener('focusout', hide);
  host.addEventListener('keydown', navigate);
  return () => {
    host.removeEventListener('mousemove', move);
    host.removeEventListener('mouseleave', hide);
    host.removeEventListener('focusin', focus);
    host.removeEventListener('focusout', hide);
    host.removeEventListener('keydown', navigate);
    tooltip?.remove();
    host.replaceChildren();
  };
}

export function CostsChart({ chart, enabled = true }) {
  const host = useRef(null);
  useEffect(() => {
    if (!enabled) {
      host.current.replaceChildren();
      return undefined;
    }
    return mountImperativeCostChart(host.current, chart);
  }, [chart, enabled]);
  return html`<div id="costs-chart" ref=${host}></div>`;
}
// dashboard-imperative-boundary: cost-chart
