import { Fragment, h, render } from 'preact';
import { useEffect } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);
export const DEBUG_POLL_MS = 10_000;

const PHASE_COLORS = [
  '#3987e5', '#199e70', '#c98500', '#008300',
  '#9085e9', '#e66767', '#d55181', '#7d8590',
];

function phaseColor(index) {
  return PHASE_COLORS[Math.min(index, PHASE_COLORS.length - 1)];
}

function fmtMs(value) {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '—';
  if (value >= 100) return `${Math.round(value)} ms`;
  if (value >= 10) return `${value.toFixed(1)} ms`;
  return `${value.toFixed(2)} ms`;
}

function Sparkline({ samples }) {
  if (!samples.length) return html`<div class="empty">No samples yet.</div>`;
  const height = 100;
  const width = Math.max(samples.length, 2);
  const values = samples.map((sample) =>
    typeof sample.total_ms === 'number' && Number.isFinite(sample.total_ms)
      ? sample.total_ms
      : 0);
  const maxMs = Math.max(...values, 1e-6);
  const x = (index) => samples.length === 1 ? width / 2 : (index * width) / (samples.length - 1);
  const y = (value) => height - (value / maxMs) * (height - 4) - 2;
  const points = values.map((value, index) => `${x(index).toFixed(2)},${y(value).toFixed(2)}`);
  const area = `M0,${height} L${points.join(' L')} L${width},${height} Z`;
  const latest = samples[samples.length - 1].total_ms;
  const label = `latency sparkline: ${samples.length} samples, latest ${fmtMs(latest)}, max ${fmtMs(maxMs)}`;
  return html`
    <svg class="debug-spark" viewBox=${`0 0 ${width} ${height}`} preserveAspectRatio="none"
      role="img" aria-label=${label}>
      <title>${samples.length} samples — latest ${fmtMs(latest)}, max ${fmtMs(maxMs)}</title>
      <path class="debug-spark-area" d=${area}></path>
      <polyline class="debug-spark-line" points=${points.join(' ')}
        strokeWidth="2" vectorEffect="non-scaling-stroke"
        strokeLinejoin="round" strokeLinecap="round"></polyline>
    </svg>
  `;
}

function PhaseBreakdown({ phases }) {
  if (!phases.length) return null;
  const total = phases.reduce((sum, phase) => sum + (phase.p50_ms || 0), 0);
  return html`<${Fragment}>
    ${total > 0 && html`<${Fragment}>
      <div class="debug-subhead">median poll composition <span class="muted">(phase p50s, ${fmtMs(total)} together)</span></div>
      <div class="debug-phasebar">
        ${phases.map((phase, index) => {
          const share = (phase.p50_ms || 0) / total;
          if (share <= 0.001) return null;
          return html`<div key=${phase.name} class="debug-phase-seg"
            style=${{ flex: `${share.toFixed(4)} 1 0`, background: phaseColor(index) }}
            title=${`${phase.name} — median ${fmtMs(phase.p50_ms)} (${Math.round(share * 100)}% of the median poll)`}></div>`;
        })}
      </div>
      <div class="debug-legend">
        ${phases.map((phase, index) => html`<span key=${phase.name} class="debug-legend-item">
          <span class="debug-legend-chip" style=${{ background: phaseColor(index) }}></span>
          ${phase.name} <span class="muted">${fmtMs(phase.p50_ms)}</span>
        </span>`)}
      </div>
    </${Fragment}>`}
    <table class="debug-table">
      <thead><tr><th scope="col">Phase</th><th scope="col">p50</th><th scope="col">p90</th><th scope="col">p99</th><th scope="col">max</th></tr></thead>
      <tbody>
        ${phases.map((phase, index) => html`<tr key=${phase.name} data-key=${`phase-${phase.name}`}>
          <td><span class="debug-legend-chip" style=${{ background: phaseColor(index) }}></span>${phase.name}</td>
          <td>${fmtMs(phase.p50_ms)}</td><td>${fmtMs(phase.p90_ms)}</td>
          <td>${fmtMs(phase.p99_ms)}</td><td>${fmtMs(phase.max_ms)}</td>
        </tr>`)}
      </tbody>
    </table>
  </${Fragment}>`;
}

function EndpointCard({ endpoint }) {
  const samples = Array.isArray(endpoint.samples) ? endpoint.samples : [];
  const phases = Array.isArray(endpoint.phases) ? endpoint.phases : [];
  const latest = samples.length ? samples[samples.length - 1].total_ms : NaN;
  return html`<div class="debug-card" data-key=${`debug-${endpoint.endpoint}`}>
    <div class="debug-card-head">
      <span class="rowname">${endpoint.endpoint}</span>
      <span class="muted">${endpoint.count} sample${endpoint.count === 1 ? '' : 's'}</span>
      <span class="spacer"></span>
      <span class="debug-stat">latest <strong>${fmtMs(latest)}</strong></span>
      <span class="debug-stat">p50 <strong>${fmtMs(endpoint.p50_ms)}</strong></span>
      <span class="debug-stat">p90 <strong>${fmtMs(endpoint.p90_ms)}</strong></span>
      <span class="debug-stat">p99 <strong>${fmtMs(endpoint.p99_ms)}</strong></span>
      <span class="debug-stat">max <strong>${fmtMs(endpoint.max_ms)}</strong></span>
    </div>
    <${Sparkline} samples=${samples} />
    <${PhaseBreakdown} phases=${phases} />
  </div>`;
}

function updatedLabel(response) {
  if (!response?.generated_at) return '';
  const generated = new Date(response.generated_at);
  return Number.isNaN(generated.getTime()) ? '' : `updated ${generated.toLocaleTimeString()}`;
}

function DebugContents({ current }) {
  if (current.request.phase === 'loading') {
    return html`<div class="empty">Loading poll timings…</div>`;
  }
  if (current.request.phase === 'error') {
    const operation = current.request.operation === 'reset' ? 'reset' : 'load';
    return html`<div class="island-error" role="alert">Failed to ${operation} poll timings: ${current.request.error}</div>`;
  }
  if (!current.endpoints.length) {
    return html`<div class="empty">No poll samples recorded yet — the recorder fills as the dashboard polls (open the Groups tab for a few seconds, then return here).</div>`;
  }
  return current.endpoints.map((endpoint) =>
    html`<${EndpointCard} key=${endpoint.endpoint} endpoint=${endpoint} />`);
}

export function DebugApp({
  state,
  actions,
  pollMs = DEBUG_POLL_MS,
  setIntervalImpl = globalThis.setInterval,
  clearIntervalImpl = globalThis.clearInterval,
}) {
  const current = state.view.value;
  useEffect(() => {
    if (!current.active) {
      actions.cancel();
      return undefined;
    }
    void actions.load();
    const timer = setIntervalImpl(actions.load, pollMs);
    return () => {
      clearIntervalImpl(timer);
      actions.cancel();
    };
  }, [current.active, actions, pollMs, setIntervalImpl, clearIntervalImpl]);

  const busy = current.request.phase === 'loading' || current.request.phase === 'refreshing';
  return html`<${Fragment}>
    <h2 class="debug-wizard-title">🧪 The Alchemist's Observatory</h2>
    <div class="filter-bar">
      <span class="muted">
        <span class="theme-copy-regular">Wall-clock timings of the dashboard's background polls, recorded in-memory by this daemon process (newest ≈ 34 min at the 2s poll; reset on daemon restart).</span>
        <span class="theme-copy-wizard">Arcane readings of the Tower's background scrying, recorded in this daemon's memory (newest ≈ 34 min at the 2s poll; the readings vanish when the daemon restarts).</span>
      </span>
      <span class="spacer"></span>
      <span id="debug-updated" class="muted" aria-live="polite">${updatedLabel(current.response)}</span>
      <button class="tool" id="debug-reset" disabled=${current.resetting}
        title="Discard every recorded sample and start a fresh distribution — useful after changing what's being measured (agent count, config)"
        onClick=${actions.reset}>
        <span class="theme-copy-regular">${current.resetting ? '… resetting' : '↺ reset stats'}</span>
        <span class="theme-copy-wizard">${current.resetting ? '… clearing' : '⚗ clear readings'}</span>
      </button>
    </div>
    <div id="debug-list" aria-busy=${busy}><${DebugContents} current=${current} /></div>
  </${Fragment}>`;
}

export function mountDebugIsland({
  host,
  state,
  actions,
  registerCleanup,
  pollMs,
  setIntervalImpl,
  clearIntervalImpl,
}) {
  render(html`<${DebugApp} state=${state} actions=${actions} pollMs=${pollMs}
    setIntervalImpl=${setIntervalImpl} clearIntervalImpl=${clearIntervalImpl} />`, host);
  registerCleanup(() => {
    actions.dispose();
    render(null, host);
  });
}
