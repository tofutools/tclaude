// The Usage Overview's two compact charts — the span's daily spend bars and
// each quota window's sparkline — and their hover inspection. They read like
// the Costs and Quota views' full charts (same palette, same tooltip chrome)
// at a fraction of the size, so an operator can analyse spend and quota from
// the Overview without switching views.
import { h } from 'preact';
import { useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  costProviderLabel, fmtExactUSD, fmtUSD, isWeekendKey, niceCeil, providerSegmentClass,
} from './costs-model.js';
import { formatUsageDuration, formatUsageTime } from './usage-history-model.js';

const html = htm.bind(h);
const TIP_PAD = 14;
const SPEND_W = 720;
const SPEND_H = 150;
const SPARK_W = 240;
const SPARK_H = 40;

// topCostAgents folds the per agent-day slices of the shown rows (the Costs
// view's provider/model filters apply) into one total per agent.
export function topCostAgents(rows, limit = 5) {
  const byAgent = new Map();
  for (const row of rows || []) {
    const key = row.agent_id || row.conv_id;
    const entry = byAgent.get(key) || {
      key, title: row.title || '(unknown)', agentID: row.agent_id, convID: row.conv_id, cost: 0, whatIf: false,
    };
    entry.cost += row.cost_usd || 0;
    entry.whatIf ||= row.cost_kind === 'what_if' || row.cost_kind === 'mixed';
    byAgent.set(key, entry);
  }
  return [...byAgent.values()].filter((entry) => entry.cost > 0)
    .sort((a, b) => b.cost - a.cost || a.title.localeCompare(b.title)).slice(0, limit);
}

// spendDays splits each day of the shown span by provider, in the Costs
// view's provider palette, so the Overview's bars read like its stacks.
export function spendDays(costs) {
  const providers = costs.providers || [];
  const agents = costs.narrowed?.agents || [];
  return (costs.narrowed?.days || []).map((day) => {
    const byProvider = new Map();
    for (const agent of agents) {
      if (agent.day !== day.day || !(agent.cost_usd > 0)) continue;
      const provider = costProviderLabel(agent);
      byProvider.set(provider, (byProvider.get(provider) || 0) + agent.cost_usd);
    }
    const segments = [...byProvider].map(([provider, cost]) => ({
      provider, cost, className: providerSegmentClass(provider, providers),
    })).sort((a, b) => providers.indexOf(a.provider) - providers.indexOf(b.provider));
    return { day: day.day, cost: day.cost_usd || 0, whatIf: day.cost_kind === 'what_if', segments };
  });
}

// spendSlots is one entry per chart slot: the span's recorded days, then the
// projection's remaining days. Each carries what its hover shows — the running
// total, its share of the span, and the day's agents. A projected slot's
// running total spreads any leading-day fill evenly over the remaining days,
// so the last slot lands on the projection total the legend quotes.
export function spendSlots(costs) {
  const days = spendDays(costs);
  const agents = costs.narrowed?.agents || [];
  let running = 0;
  const slots = days.map((day) => {
    running += day.cost;
    const ranked = topCostAgents(agents.filter((agent) => agent.day === day.day), Infinity);
    return { ...day, projected: false, accumulated: running, agentCount: ranked.length, top: ranked.slice(0, 3) };
  });
  const recorded = running;
  for (const slot of slots) slot.share = recorded > 0 ? slot.cost / recorded : 0;
  const future = costs.projection?.future || [];
  const futureSum = future.reduce((sum, day) => sum + (day.cost_usd || 0), 0);
  const gap = costs.projection ? Math.max(0, costs.projection.total - recorded - futureSum) : 0;
  future.forEach((day, index) => {
    running += day.cost_usd || 0;
    slots.push({
      day: day.day, cost: day.cost_usd || 0, projected: true, segments: [], whatIf: false,
      accumulated: running + gap * (index + 1) / future.length, agentCount: 0, top: [], share: 0,
    });
  });
  return { slots, recorded };
}

function weekday(key) {
  const date = new Date(key + 'T12:00:00');
  return Number.isNaN(date.getTime()) ? '' : date.toLocaleDateString('en-US', { weekday: 'short' });
}

// FloatingTip is the overview's viewport-fixed tooltip. It measures itself
// after each render and flips to the cursor's other side near a viewport edge,
// the same placement the Costs charts use.
function FloatingTip({ anchor, className = '', children }) {
  const panel = useRef(null);
  const [position, setPosition] = useState(null);
  useLayoutEffect(() => {
    const node = panel.current;
    if (!node) return;
    const rect = node.getBoundingClientRect();
    let left = anchor.x + TIP_PAD;
    let top = anchor.y + TIP_PAD;
    if (left + rect.width > window.innerWidth - 4) left = anchor.x - TIP_PAD - rect.width;
    if (top + rect.height > window.innerHeight - 4) top = anchor.y - TIP_PAD - rect.height;
    const next = { left: Math.max(4, left), top: Math.max(4, top) };
    setPosition((current) => current?.left === next.left && current?.top === next.top ? current : next);
  });
  const style = position
    ? `display:block;left:${position.left}px;top:${position.top}px`
    : `display:block;left:${anchor.x}px;top:${anchor.y}px;visibility:hidden`;
  return html`<div ref=${panel} class=${`cost-tip usage-ov-tip ${className}`} style=${style}>${children}</div>`;
}

function TipRow({ swatch = '', name, amount, className = '' }) {
  return html`<div class=${`cost-tip-row ${className}`}>
    <span class=${`cost-tip-sw ${swatch}`} style=${swatch ? '' : 'visibility:hidden'}></span>
    <span class="cost-tip-name">${name}</span><span class="cost-tip-amt">${amount}</span>
  </div>`;
}

// The breakdown lists providers top-down in the order the bar stacks them on
// screen (the first segment sits at the bottom).
function SpendTip({ slot, focus, costs, anchor }) {
  const projection = costs.projection;
  if (slot.projected) {
    const skipped = !(slot.cost > 0) && projection && !projection.weekendsIncluded && isWeekendKey(slot.day);
    return html`<${FloatingTip} anchor=${anchor} className="projected">
      <div class="cost-tip-day">${slot.day} · ${weekday(slot.day)} · projection</div>
      <${TipRow} name="projected day" amount=${skipped ? 'weekend — $0' : `≈${fmtExactUSD(slot.cost)}`} />
      <div class="cost-tip-total"><span class="cost-tip-sw" style="visibility:hidden"></span>
        <span class="cost-tip-name">projected accumulated</span>
        <span class="cost-tip-amt">≈${fmtExactUSD(slot.accumulated)}</span></div>
      ${projection?.daysElapsed > 0 && html`<div class="usage-ov-tip-note">
        ${fmtUSD(projection.perDay)}/day average over ${projection.daysElapsed} recorded day${projection.daysElapsed === 1 ? '' : 's'}${projection.weekendsIncluded ? '' : ' (weekdays)'}</div>`}
    </${FloatingTip}>`;
  }
  return html`<${FloatingTip} anchor=${anchor}>
    <div class="cost-tip-day">${slot.day} · ${weekday(slot.day)}</div>
    ${[...slot.segments].reverse().map((segment) => html`<${TipRow} key=${segment.provider} swatch=${segment.className}
      name=${segment.provider} amount=${fmtExactUSD(segment.cost)}
      className=${focus && focus === segment.provider ? 'usage-ov-tip-focused' : ''} />`)}
    <div class="cost-tip-total"><span class="cost-tip-sw" style="visibility:hidden"></span>
      <span class="cost-tip-name">day total</span><span class="cost-tip-amt">${fmtExactUSD(slot.cost)}</span></div>
    <${TipRow} name="accumulated" amount=${fmtExactUSD(slot.accumulated)} />
    ${slot.share > 0 && html`<${TipRow} name="share of span" amount=${`${(slot.share * 100).toFixed(1)}%`} />`}
    ${slot.top.length > 0 && html`<div class="usage-ov-tip-agents">
      <div class="usage-ov-tip-sub">${slot.agentCount} agent${slot.agentCount === 1 ? '' : 's'}${slot.agentCount > slot.top.length ? ` · top ${slot.top.length}` : ''}</div>
      ${slot.top.map((agent) => html`<${TipRow} key=${agent.key} name=${agent.title}
        amount=${`${agent.whatIf ? '≈' : ''}${fmtExactUSD(agent.cost)}`} className="usage-ov-tip-agent" />`)}
    </div>`}
    ${slot.whatIf && html`<div class="usage-ov-tip-note">includes WHAT-IF estimates</div>`}
  </${FloatingTip}>`;
}

function slotSummary(slot) {
  if (slot.projected) return `${slot.day}, projection, approximately ${fmtExactUSD(slot.cost)} that day, approximately ${fmtExactUSD(slot.accumulated)} accumulated.`;
  const breakdown = slot.segments.map((segment) => `${segment.provider} ${fmtExactUSD(segment.cost)}`).join(', ');
  return `${slot.day}, ${fmtExactUSD(slot.cost)} that day${breakdown ? ` (${breakdown})` : ''}, ${fmtExactUSD(slot.accumulated)} accumulated.`;
}

// SpendChart draws the span's daily bars (stacked by provider) on one scale
// and the running total plus its projection on another. Hovering a day — or a
// provider's slice of it — opens its breakdown; hovering a legend entry
// singles out that provider across the span; the plot is keyboard-steppable.
export function SpendChart({ costs, label }) {
  const plot = useRef(null);
  const [hover, setHover] = useState(null);
  const [legendFocus, setLegendFocus] = useState(null);
  const [announcement, setAnnouncement] = useState('');
  // A click focuses the plot; that focus must not jump the readout away from
  // the day under the pointer.
  const pointerDown = useRef(false);
  const { slots, recorded } = useMemo(() => spendSlots(costs), [costs]);
  const recordedCount = slots.filter((slot) => !slot.projected).length;
  if (!recordedCount) return html`<div class="usage-ov-chart empty">No days in span.</div>`;
  const W = SPEND_W;
  const H = SPEND_H;
  // Two scales, as in the mock: the bars read the day-to-day pace, the line
  // the running total and where the projection lands. The legend names both.
  const dailyTop = niceCeil(Math.max(...slots.filter((slot) => !slot.projected).map((slot) => slot.cost))) || 1;
  const totalTop = Math.max(recorded, costs.projection?.total || 0) || 1;
  const bw = W / slots.length;
  const yDaily = (value) => (value / dailyTop) * (H - 4);
  const yTotal = (value) => H - (value / totalTop) * (H - 6);
  const cx = (index) => (index + 0.5) * bw;
  const recordedSlots = slots.slice(0, recordedCount);
  const line = recordedSlots.map((slot, index) => `${cx(index)},${yTotal(slot.accumulated)}`).join(' ');
  const projectedLine = costs.projection && slots.length > recordedCount
    ? slots.slice(recordedCount - 1).map((slot, offset) => `${cx(recordedCount - 1 + offset)},${yTotal(slot.accumulated)}`).join(' ')
    : '';
  const providers = [...new Set(recordedSlots.flatMap((slot) => slot.segments.map((segment) => segment.provider)))];
  const active = hover ? slots[hover.index] : null;
  const focusProvider = hover?.provider || legendFocus;

  const inspect = (index, provider, anchor, announce = false) => {
    setHover({ index, provider: provider || null, anchor });
    if (announce) setAnnouncement(slotSummary(slots[index]));
  };
  const slotAnchor = (index) => {
    const rect = plot.current?.getBoundingClientRect();
    const slot = slots[index];
    return {
      x: (rect?.left || 0) + cx(index) / W * (rect?.width || W),
      y: (rect?.top || 0) + yTotal(slot.accumulated) / H * (rect?.height || H),
    };
  };
  const move = (event) => {
    const rect = plot.current.getBoundingClientRect();
    const ratio = (event.clientX - rect.left) / Math.max(rect.width, 1);
    const index = Math.max(0, Math.min(slots.length - 1, Math.floor(ratio * slots.length)));
    const provider = event.target?.getAttribute?.('data-provider');
    inspect(index, provider, { x: event.clientX, y: event.clientY });
  };
  const keydown = (event) => {
    const moves = { ArrowLeft: -1, ArrowRight: 1 };
    if (!(event.key in moves) && event.key !== 'Home' && event.key !== 'End' && event.key !== 'Escape') return;
    event.preventDefault();
    if (event.key === 'Escape') { setHover(null); return; }
    const current = hover?.index ?? recordedCount - 1;
    const index = event.key === 'Home' ? 0 : event.key === 'End' ? slots.length - 1
      : Math.max(0, Math.min(slots.length - 1, current + moves[event.key]));
    inspect(index, null, slotAnchor(index), true);
  };
  const leave = () => setHover(null);
  const focus = () => {
    if (pointerDown.current) { pointerDown.current = false; return; }
    inspect(recordedCount - 1, null, slotAnchor(recordedCount - 1), true);
  };
  const summary = `${label} spend ${fmtExactUSD(recorded)}${costs.projection ? `, projected ${fmtExactUSD(costs.projection.total)}` : ''}. Focus and use Left and Right Arrow keys to inspect each day.`;

  return html`<div class="usage-ov-chart-wrap">
    <div class="usage-ov-chart">
      <div ref=${plot} class=${`usage-ov-plot${hover ? ' hovering' : ''}${focusProvider ? ' has-focus' : ''}`}
        role="img" tabIndex="0" aria-label=${summary}
        onmousemove=${move} onmouseleave=${leave} onmousedown=${() => { pointerDown.current = true; }}
        onfocus=${focus}
        onblur=${() => { setHover(null); setAnnouncement(''); }} onkeydown=${keydown}>
        <svg viewBox=${`0 0 ${W} ${H}`} preserveAspectRatio="none" aria-hidden="true">
          ${active && html`<rect class=${`hover-col${active.projected ? ' projected' : ''}`}
            x=${hover.index * bw} y="0" width=${bw} height=${H} />`}
          ${recordedSlots.map((slot, index) => {
            let top = H;
            const classes = [slot.whatIf && 'what-if', hover?.index === index && 'hovered'].filter(Boolean).join(' ');
            return html`<g key=${slot.day} class=${classes}>
              ${slot.segments.map((segment) => {
                const height = yDaily(segment.cost);
                top -= height;
                const focused = focusProvider === segment.provider && (legendFocus || hover?.index === index);
                return html`<rect key=${segment.provider} data-provider=${segment.provider}
                  class=${`bar ${segment.className}${focused ? ' focused' : ''}`}
                  x=${index * bw + bw * 0.15} y=${top} width=${bw * 0.7} height=${height} />`;
              })}
            </g>`;
          })}
          <polyline class="cumulative" points=${line} />
          ${projectedLine && html`<polyline class="projection" points=${projectedLine} />`}
        </svg>
        ${active && html`<i class=${`usage-ov-dot${active.projected ? ' projected' : ''}`} aria-hidden="true"
          style=${`left:${cx(hover.index) / W * 100}%;top:${yTotal(active.accumulated) / H * 100}%`}></i>`}
      </div>
    </div>
    <div class="usage-ov-legend">
      ${providers.map((provider) => html`<span key=${provider} class=${`legend-provider${legendFocus === provider ? ' focused' : ''}`}
        onmouseenter=${() => setLegendFocus(provider)} onmouseleave=${() => setLegendFocus(null)}>
        <i class=${`swatch ${providerSegmentClass(provider, costs.providers || [])}`}></i>${provider}</span>`)}
      <span>bars: daily, up to ${fmtUSD(dailyTop)}</span>
      <span><i class="line"></i>accumulated ${fmtUSD(recorded)}</span>
      ${costs.projection && html`<span><i class="line dashed"></i>projected ~${fmtUSD(costs.projection.total)}</span>`}
    </div>
    ${active && html`<${SpendTip} slot=${active} focus=${hover.provider} costs=${costs} anchor=${hover.anchor} />`}
    <div class="usage-ov-chart-status" role="status" aria-live="polite" aria-atomic="true">${announcement}</div>
  </div>`;
}

// sparkGeometry lays a quota window's samples, its reset and its forecast on
// one time axis capped a week past now, so a far-off reset cannot squash the
// observed line flat.
export function sparkGeometry(points, forecast, now) {
  const from = new Date(points[0].at).getTime();
  const hit = new Date(forecast?.hits_limit_at).getTime();
  const reset = new Date(forecast?.reset_at).getTime();
  const ends = [now, Number.isFinite(hit) ? hit : now, Number.isFinite(reset) ? reset : now];
  const to = Math.max(...ends.filter((value) => value - now < 7 * 86_400_000));
  const last = points[points.length - 1];
  const lastAt = new Date(last.at).getTime();
  const rate = Number(forecast?.rate_pct_per_hour || 0);
  const projectTo = Number.isFinite(reset) && reset > lastAt ? reset : to;
  const projectedAt = (at) => Math.min(100, last.pct + rate * (at - lastAt) / 3_600_000);
  return {
    from, to, span: Math.max(1, to - from), last, lastAt, rate, reset, projectTo,
    projected: rate > 0 ? projectedAt(projectTo) : null, projectedAt,
  };
}

// sparkInspect reads the sparkline at a time: the nearest observed sample up
// to the last one, the forecast past it, and the reset when the cursor sits on
// its marker.
export function sparkInspect(points, geometry, at, tolerance) {
  const { lastAt, reset, rate, projectTo } = geometry;
  const nearReset = Number.isFinite(reset) && Math.abs(at - reset) <= tolerance;
  if (at <= lastAt + tolerance) {
    const point = points.reduce((best, candidate) =>
      Math.abs(new Date(candidate.at).getTime() - at) < Math.abs(new Date(best.at).getTime() - at) ? candidate : best);
    return { kind: 'observed', at: new Date(point.at).getTime(), pct: point.pct, nearReset };
  }
  if (rate > 0 && at <= projectTo + tolerance) {
    const clamped = Math.min(at, projectTo);
    return { kind: 'forecast', at: clamped, pct: geometry.projectedAt(clamped), nearReset };
  }
  return nearReset ? { kind: 'reset', at: reset } : null;
}

function SparkTip({ hit, geometry, now, wizard, anchor }) {
  const w = (plain, wizardly) => (wizard ? wizardly : plain);
  const time = (at) => `${new Date(at).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })} · ${formatUsageTime(at, now)}`;
  const resetLine = Number.isFinite(geometry.reset)
    ? `${w('resets', 'replenishes')} ${formatUsageTime(geometry.reset, now)}` : '';
  if (hit.kind === 'reset') {
    return html`<${FloatingTip} anchor=${anchor} className="usage-ov-spark-tip reset">
      <div class="cost-tip-day">${w('Window reset', 'Replenishment')}</div>
      <div class="usage-ov-tip-line">${time(hit.at)}</div>
    </${FloatingTip}>`;
  }
  const forecast = hit.kind === 'forecast';
  return html`<${FloatingTip} anchor=${anchor} className=${`usage-ov-spark-tip${forecast ? ' forecast' : ''}`}>
    <div class="cost-tip-day">${forecast ? w('Prediction', 'Prophecy') : w('Observed', 'Measured')}
      <b class="usage-ov-tip-pct">${forecast ? '≈' : ''}${hit.pct.toFixed(forecast ? 0 : 1)}%</b></div>
    <div class="usage-ov-tip-line">${time(hit.at)}</div>
    ${forecast && hit.pct >= 100 && html`<div class="usage-ov-tip-line warn">${w('limit reached', 'reserves run dry')}</div>`}
    ${forecast && html`<div class="usage-ov-tip-line">${geometry.rate.toFixed(geometry.rate < 1 ? 2 : 1)}%/h ${w('recent pace', 'channeling pace')}</div>`}
    ${resetLine && html`<div class="usage-ov-tip-line muted">${hit.nearReset ? '↺ ' : ''}${resetLine}${Number.isFinite(geometry.reset) && geometry.reset > hit.at
      ? ` (${formatUsageDuration(geometry.reset - hit.at)} after this)` : ''}</div>`}
  </${FloatingTip}>`;
}

// Sparkline is a quota card's compact usage history. Hovering it reads the
// sample (or the forecast) under the cursor; the card itself stays a link to
// the full Quota forecast.
export function Sparkline({ points, forecast, now, wizard = false }) {
  const wrap = useRef(null);
  const [hover, setHover] = useState(null);
  if (points.length < 2) return html`<div class="usage-ov-spark empty" aria-hidden="true"></div>`;
  const W = SPARK_W;
  const H = SPARK_H;
  const geometry = sparkGeometry(points, forecast, now);
  const { from, span, last, lastAt, reset, to, projectTo, projected } = geometry;
  const x = (at) => ((at - from) / span) * W;
  const y = (pct) => H - Math.max(0, Math.min(100, pct)) / 100 * H;
  const line = points.map((point) => `${x(new Date(point.at).getTime()).toFixed(1)},${y(point.pct).toFixed(1)}`).join(' ');
  const move = (event) => {
    const rect = wrap.current.getBoundingClientRect();
    const ratio = Math.max(0, Math.min(1, (event.clientX - rect.left) / Math.max(rect.width, 1)));
    const tolerance = span * 6 / Math.max(rect.width, 1);
    const hit = sparkInspect(points, geometry, from + ratio * span, tolerance);
    setHover(hit ? { hit, anchor: { x: event.clientX, y: event.clientY } } : null);
  };
  const hit = hover?.hit;
  return html`<div ref=${wrap} class="usage-ov-spark-wrap" onmousemove=${move} onmouseleave=${() => setHover(null)}>
    <svg class="usage-ov-spark" viewBox=${`0 0 ${W} ${H}`} preserveAspectRatio="none" aria-hidden="true">
      <line class="limit" x1="0" y1="0.5" x2=${W} y2="0.5" />
      ${Number.isFinite(reset) && reset <= to && html`<line class=${`reset${hit?.kind === 'reset' || hit?.nearReset ? ' hovered' : ''}`}
        x1=${x(reset)} y1="0" x2=${x(reset)} y2=${H} />`}
      <polyline class="observed" points=${line} />
      ${projected !== null && html`<line class="forecast" x1=${x(lastAt)} y1=${y(last.pct)}
        x2=${x(projectTo)} y2=${y(projected)} />`}
      ${hit && html`<line class="guide" x1=${x(hit.at)} y1="0" x2=${x(hit.at)} y2=${H} />`}
    </svg>
    ${hit && hit.kind !== 'reset' && html`<i class=${`usage-ov-dot${hit.kind === 'forecast' ? ' forecast' : ' observed'}`} aria-hidden="true"
      style=${`left:${x(hit.at) / W * 100}%;top:${y(hit.pct) / H * 100}%`}></i>`}
    ${hit && html`<${SparkTip} hit=${hit} geometry=${geometry} now=${now} wizard=${wizard} anchor=${hover.anchor} />`}
  </div>`;
}
