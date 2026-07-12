// debug.js — the Debug tab: the daemon's poll-timing distributions
// (TCL-376), read from GET /api/perf (the in-memory recorder behind
// every polled dashboard endpoint — agentd/perf.go, TCL-374).
//
// One card per endpoint: a latency sparkline over the recent samples
// plus p50/p90/p99/max chips. An endpoint with a phase breakdown
// (/api/snapshot) also gets a "median poll composition" stacked bar and
// a per-phase aggregate table, so the dominant phase of a slow poll is
// visible at a glance.
//
// Fetched on tab activation, then re-fetched on each snapshot tick
// while the tab is showing (the tick event fires every ~2s, and
// /api/perf is a cheap in-memory read) — never on other tabs.

import { $, esc } from './helpers.js';
import { morphInto } from './morph.js';

// Monotonic guard: a slow response must never repaint over a newer one.
let loadSeq = 0;

// Fixed categorical slots for phase fills, assigned by each phase's
// first-seen (execution) order — never re-derived from rank, so a phase
// keeps its color as distributions shift. The set is the dataviz
// reference palette's dark column, validated against this dashboard's
// card surface (#161b22): all slots ≥3:1 contrast; adjacent-pair CVD
// separation sits in the 8–12 floor band, which is why every colored
// mark here also carries a text label, a tooltip, and the aggregate
// table (identity is never color-alone). The trailing gray is the
// overflow slot for a hypothetical 8th+ phase.
const PHASE_COLORS = [
  '#3987e5', '#199e70', '#c98500', '#008300',
  '#9085e9', '#e66767', '#d55181', '#7d8590',
];

function phaseColor(i) {
  return PHASE_COLORS[Math.min(i, PHASE_COLORS.length - 1)];
}

function fmtMs(v) {
  if (typeof v !== 'number' || !isFinite(v)) return '—';
  if (v >= 100) return Math.round(v) + ' ms';
  if (v >= 10) return v.toFixed(1) + ' ms';
  return v.toFixed(2) + ' ms';
}

// sparklineSVG draws the endpoint's total_ms series as a thin line with
// a soft area fill. The viewBox spans the sample count and the series
// max; the element scales to the card width with vector-effect keeping
// the stroke at 2px under the non-uniform scale (which is also why no
// text lives inside the SVG — it would distort; labels render as HTML
// beside it).
function sparklineSVG(samples) {
  const n = samples.length;
  if (n === 0) return '<div class="empty">No samples yet.</div>';
  const H = 100;
  const W = Math.max(n, 2);
  const maxMs = Math.max(...samples.map(s => s.total_ms), 1e-6);
  const x = i => n === 1 ? W / 2 : (i * W) / (n - 1);
  const y = v => H - (v / maxMs) * (H - 4) - 2;
  const pts = samples.map((s, i) => `${x(i).toFixed(2)},${y(s.total_ms).toFixed(2)}`);
  const area = `M0,${H} L${pts.join(' L')} L${W},${H} Z`;
  const latest = samples[n - 1].total_ms;
  return `
    <svg class="debug-spark" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" role="img"
         aria-label="latency sparkline: ${n} samples, latest ${fmtMs(latest)}, max ${fmtMs(maxMs)}">
      <title>${n} samples — latest ${fmtMs(latest)}, max ${fmtMs(maxMs)}</title>
      <path d="${area}" fill="rgba(57,135,229,0.15)" stroke="none"></path>
      <polyline points="${pts.join(' ')}" fill="none" stroke="#3987e5"
                stroke-width="2" vector-effect="non-scaling-stroke"
                stroke-linejoin="round" stroke-linecap="round"></polyline>
    </svg>`;
}

// phaseBarHTML renders the "median poll composition" bar: one segment
// per phase, sized by its p50 share. Identity rides the legend + each
// segment's tooltip, not color alone; the 2px gaps keep adjacent fills
// separable. Zero-width phases (p50 rounds to nothing) are dropped from
// the bar but stay in the table below.
function phaseBarHTML(phases) {
  const total = phases.reduce((acc, p) => acc + (p.p50_ms || 0), 0);
  if (total <= 0) return '';
  const segs = phases.map((p, i) => {
    const share = (p.p50_ms || 0) / total;
    if (share <= 0.001) return '';
    return `<div class="debug-phase-seg" style="flex:${share.toFixed(4)} 1 0;background:${phaseColor(i)}"
      title="${esc(p.name)} — median ${fmtMs(p.p50_ms)} (${Math.round(share * 100)}% of the median poll)"></div>`;
  }).join('');
  const legend = phases.map((p, i) =>
    `<span class="debug-legend-item"><span class="debug-legend-chip" style="background:${phaseColor(i)}"></span>${esc(p.name)} <span class="muted">${esc(fmtMs(p.p50_ms))}</span></span>`).join('');
  return `
    <div class="debug-subhead">median poll composition <span class="muted">(phase p50s, ${esc(fmtMs(total))} together)</span></div>
    <div class="debug-phasebar">${segs}</div>
    <div class="debug-legend">${legend}</div>`;
}

// phaseTableHTML is the table view of the phase aggregates — the
// always-available non-graphical encoding of the same numbers.
function phaseTableHTML(phases) {
  if (!phases.length) return '';
  return `
    <table class="debug-table">
      <thead><tr><th>Phase</th><th>p50</th><th>p90</th><th>p99</th><th>max</th></tr></thead>
      <tbody>
        ${phases.map((p, i) => `
          <tr data-key="phase-${esc(p.name)}">
            <td><span class="debug-legend-chip" style="background:${phaseColor(i)}"></span>${esc(p.name)}</td>
            <td>${esc(fmtMs(p.p50_ms))}</td>
            <td>${esc(fmtMs(p.p90_ms))}</td>
            <td>${esc(fmtMs(p.p99_ms))}</td>
            <td>${esc(fmtMs(p.max_ms))}</td>
          </tr>`).join('')}
      </tbody>
    </table>`;
}

function endpointCardHTML(ep) {
  const samples = ep.samples || [];
  const phases = ep.phases || [];
  const latest = samples.length ? samples[samples.length - 1].total_ms : NaN;
  return `
    <div class="debug-card" data-key="debug-${esc(ep.endpoint)}">
      <div class="debug-card-head">
        <span class="rowname">${esc(ep.endpoint)}</span>
        <span class="muted">${ep.count} sample${ep.count === 1 ? '' : 's'}</span>
        <span class="spacer"></span>
        <span class="debug-stat">latest <strong>${esc(fmtMs(latest))}</strong></span>
        <span class="debug-stat">p50 <strong>${esc(fmtMs(ep.p50_ms))}</strong></span>
        <span class="debug-stat">p90 <strong>${esc(fmtMs(ep.p90_ms))}</strong></span>
        <span class="debug-stat">p99 <strong>${esc(fmtMs(ep.p99_ms))}</strong></span>
        <span class="debug-stat">max <strong>${esc(fmtMs(ep.max_ms))}</strong></span>
      </div>
      ${sparklineSVG(samples)}
      ${phaseBarHTML(phases)}
      ${phaseTableHTML(phases)}
    </div>`;
}

function renderDebug(data) {
  const eps = (data.endpoints || []).slice();
  if (!eps.length) {
    morphInto($('#debug-list'),
      '<div class="empty">No poll samples recorded yet — the recorder fills as the dashboard polls (open the Groups tab for a few seconds, then return here).</div>');
    return;
  }
  // The snapshot poll leads (it is the endpoint under investigation);
  // the rest keep the server's alphabetical order.
  eps.sort((a, b) =>
    (b.endpoint === '/api/snapshot') - (a.endpoint === '/api/snapshot')
    || a.endpoint.localeCompare(b.endpoint));
  morphInto($('#debug-list'), eps.map(endpointCardHTML).join(''));
}

async function loadDebug() {
  const seq = ++loadSeq;
  try {
    const r = await fetch('/api/perf?limit=240', { credentials: 'same-origin' });
    if (!r.ok) throw new Error(await r.text() || r.status);
    const data = await r.json();
    if (seq !== loadSeq) return; // superseded
    renderDebug(data);
    const at = new Date(data.generated_at);
    $('#debug-updated').textContent = isNaN(at.getTime()) ? '' :
      `updated ${at.toLocaleTimeString()}`;
  } catch (e) {
    if (seq !== loadSeq) return;
    $('#debug-updated').textContent = '';
    morphInto($('#debug-list'),
      `<div class="empty">Failed to load poll timings: ${esc(e.message || e)}</div>`);
  }
}

function debugTabActive() {
  return $('#tab-debug').classList.contains('active');
}

// resetDebug clears the daemon's timing rings, then re-fetches so the
// tab immediately shows the empty (fresh-start) state. Used after
// changing what's being measured — agent count, a config knob — so the
// aggregates don't blend the before and after. No confirm: the data is
// a self-refilling diagnostic buffer, not state anyone can lose.
async function resetDebug() {
  try {
    const r = await fetch('/api/perf/reset', { method: 'POST', credentials: 'same-origin' });
    if (!r.ok) throw new Error(await r.text() || r.status);
  } catch (e) {
    morphInto($('#debug-list'),
      `<div class="empty">Failed to reset poll timings: ${esc(e.message || e)}</div>`);
    return;
  }
  loadDebug();
}

// bindDebugTab wires the tab: load on activation, then ride the
// snapshot tick while visible.
function bindDebugTab() {
  $('nav [data-tab="debug"]').addEventListener('click', loadDebug);
  $('#debug-reset').addEventListener('click', resetDebug);
  document.addEventListener('tclaude:snapshot', () => {
    if (debugTabActive()) loadDebug();
  });
}

export { bindDebugTab };
