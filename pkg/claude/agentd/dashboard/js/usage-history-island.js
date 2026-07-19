import { Fragment, h, render } from 'preact';
import { useEffect } from 'preact/hooks';
import htm from 'htm';
import { AsyncLoadState } from './async-load-state.js';
import { UsageHistoryChart } from './usage-history-chart.js';
import {
  USAGE_HISTORY_SPANS, USAGE_LOOKAHEAD_SPANS, formatUsageResetCountdown, formatUsageTime,
  usageForecastView, usageProviderLabel, usageSeriesKeyOf, usageWindowLabel,
} from './usage-history-model.js';

const html = htm.bind(h);

function UsageSpanControls({ scope, span, onSetHours, onSetLookahead }) {
  return html`<div class="usage-card-controls">
    <div class="usage-control-group" role="group" aria-label=${`History range, ${scope}`}>
      <span class="usage-control-label" aria-hidden="true">History</span>
      ${USAGE_HISTORY_SPANS.map((option) => html`<button type="button"
        class=${`tool${span.hours === option.hours ? ' active' : ''}`}
        aria-label=${`History ${option.label}, ${scope}`} aria-pressed=${span.hours === option.hours}
        onClick=${() => onSetHours(option.hours)}>${option.label}</button>`)}
    </div>
    <span class="usage-control-divider" aria-hidden="true"></span>
    <div class="usage-control-group" role="group" aria-label=${`Forecast lookahead, ${scope}`}>
      <span class="usage-control-label" aria-hidden="true">Look ahead</span>
      ${USAGE_LOOKAHEAD_SPANS.map((option) => html`<button type="button"
        class=${`tool${span.lookaheadHours === option.hours ? ' active' : ''}`}
        aria-label=${`Look ahead ${option.label}, ${scope}`} aria-pressed=${span.lookaheadHours === option.hours}
        onClick=${() => onSetLookahead(option.hours)}>${option.label}</button>`)}
    </div>
  </div>`;
}

// The legend sits under each chart rather than once at the top of the tab: with
// graphs side by side there is no longer a single line of sight from a shared
// legend to the chart you are reading. Scoping the label keeps the repeats
// distinguishable to a screen reader.
function UsageChartLegend({ scope }) {
  return html`<div class="usage-chart-legend" aria-label=${`Usage chart legend, ${scope}`}>
    <span><i class="usage-legend-swatch observed"></i>Observed</span>
    <span><i class="usage-legend-swatch forecast"></i>Forecast</span>
    <span><i class="usage-legend-swatch reset"></i>Reset</span>
    <span><i class="usage-legend-swatch now"></i>Now</span>
  </div>`;
}

function UsageSeriesCard({ series, payload, span, onSetHours, onSetLookahead }) {
  const latest = series.points?.[series.points.length - 1];
  const now = new Date(payload.generated_at).getTime();
  const forecast = usageForecastView(series.forecast, now, latest?.at);
  const resetCount = series.reset_count ?? series.resets?.length ?? 0;
  const scope = `${usageProviderLabel(series.provider)} ${usageWindowLabel(series.window_name, series.duration_seconds)} window`;
  return html`<article class="usage-series-card">
    <div class="usage-card-header">
      <div><span class="usage-provider">${usageProviderLabel(series.provider)}</span>
        <h3>${usageWindowLabel(series.window_name, series.duration_seconds)} window</h3></div>
      <div class="usage-current"><strong>${latest ? `${latest.pct.toFixed(1)}%` : '—'}</strong>
        <span>${latest ? `sampled ${formatUsageTime(latest.at, now)}` : 'no sample'} · ${formatUsageResetCountdown(latest?.resets_at, now)}</span></div>
    </div>
    <${UsageSpanControls} scope=${scope} span=${span} onSetHours=${onSetHours} onSetLookahead=${onSetLookahead} />
    <${UsageHistoryChart} series=${series} from=${series.from ?? payload.from} generatedAt=${payload.generated_at}
      lookaheadHours=${span.lookaheadHours} />
    <${UsageChartLegend} scope=${scope} />
    <div class=${`usage-card-footer usage-forecast ${forecast.tone}`}>
      <strong>${forecast.headline}</strong>
      ${(forecast.lines || []).map((line) => html`<span class="usage-forecast-line-copy" key=${line}>${line}</span>`)}
      ${resetCount ? html`<span class="usage-reset-count">${resetCount} reset${resetCount === 1 ? '' : 's'} detected in view</span>` : null}
    </div>
  </article>`;
}

// groupSeriesByProvider splits the flat series list into one row per provider
// so a provider's quota windows share a line. current.series is sorted by
// usageSeriesSort (provider label, then window duration) in usage-history-state
// — note the server sorts by window *name*, so don't read the order off the API
// — which puts each provider's entries together, so first-seen order preserves
// both the provider order and the window order within a row.
function groupSeriesByProvider(series) {
  const rows = [];
  const byProvider = new Map();
  for (const item of series) {
    let row = byProvider.get(item.provider);
    if (!row) {
      row = { provider: item.provider, series: [] };
      byProvider.set(item.provider, row);
      rows.push(row);
    }
    row.series.push(item);
  }
  return rows;
}

export function UsageHistoryApp({ state, actions }) {
  const current = state.view.value;
  useEffect(() => {
    if (!current.snapshotLoaded) return;
    document.body.classList.toggle('hide-usage-tab', !current.visible);
    if (!current.visible && current.activeTab === 'usage') document.querySelector('nav [data-tab="groups"]')?.click();
  }, [current.snapshotLoaded, current.visible, current.activeTab]);
  useEffect(() => {
    if (!current.active) return undefined;
    void actions.load();
    const timer = setInterval(() => void actions.load(), 60_000);
    return () => clearInterval(timer);
  }, [current.active]);
  const setSpan = (key, hours) => { if (state.setSeriesHours(key, hours)) void actions.load(); };
  const setLookahead = (key, hours) => state.setSeriesLookaheadHours(key, hours);
  // Nothing but load state sits above the graphs: the legend now rides with
  // each chart and the explanatory note is a footnote below the grid, so the
  // tab opens on the data rather than on a paragraph about it.
  return html`<div class="usage-history-island">
    <${AsyncLoadState} label="Usage" request=${current.request} retry=${actions.load} errorClass="usage-history-error" />
    ${current.request.hasLoaded && html`<${Fragment}>
      ${current.series.length
        ? html`<div class="usage-series-grid">${groupSeriesByProvider(current.series).map((row) => html`
            <div class="usage-provider-row" key=${row.provider} style=${`--usage-cols:${row.series.length}`}>
              ${row.series.map((series) => {
                const key = usageSeriesKeyOf(series);
                return html`<${UsageSeriesCard} key=${key} series=${series} payload=${current.payload}
                  span=${current.spanFor(key)} onSetHours=${(hours) => setSpan(key, hours)}
                  onSetLookahead=${(hours) => setLookahead(key, hours)} />`;
              })}
            </div>`)}</div>`
        : html`<div class="empty">No subscription usage samples in this range yet.</div>`}
      <p class="usage-history-note">Account-wide provider limits, sampled every 15 minutes; history and look-ahead spans persist per graph. Forecasts are per provider × quota window. Providers do not expose reliable per-model quota attribution. A dashed line is the current post-reset pace; downward steps of at least 2 points are treated as out-of-cycle resets.</p>
    </${Fragment}>`}
  </div>`;
}

export function mountUsageHistoryIsland({ host, state, actions, registerCleanup }) {
  state.initialize();
  render(html`<${UsageHistoryApp} state=${state} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}
