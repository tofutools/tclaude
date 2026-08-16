import { Fragment, h, render } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { AsyncLoadState } from './async-load-state.js';
import { UsageHistoryChart } from './usage-history-chart.js';
import { isWizardActive } from './slop.js';
import {
  USAGE_FORECAST_ALGOS, USAGE_HISTORY_SPANS, USAGE_LOOKAHEAD_SPANS, formatUsageDuration,
  formatUsageResetCountdown, formatUsageTime, usageForecastOf, usageForecastView, usageProviderLabel,
  usageScopeLabel, usageSeriesKeyOf, usageWindowScopeLabel,
} from './usage-history-model.js';

const html = htm.bind(h);

// The Usage island stays mounted while the cosmetic theme cycles, and unlike
// most tabs it cannot lean on the `Words` two-spans/CSS-reveal trick: most of
// its themed copy lives in SVG <text> tooltips and aria-labels, which are
// single-valued. So it repaints on the wizard edge instead and picks its voice
// at render time — same shape as useWizardTheme in groups-island.js, kept local
// for the same reason the `Words` helper is (leaf islands don't share a theme
// module). Repainting preserves the instant-swap the `Words` pattern provides,
// which is why the whole tab can use plain strings on one mechanism.
function useWizardTheme() {
  const [wizard, setWizard] = useState(isWizardActive());
  useEffect(() => {
    const onWizard = (event) => setWizard(
      event.detail?.active == null ? isWizardActive() : Boolean(event.detail.active),
    );
    document.addEventListener('tclaude:wizard', onWizard);
    return () => document.removeEventListener('tclaude:wizard', onWizard);
  }, []);
  return wizard;
}

function UsageSpanControls({ scope, span, onSetHours, onSetLookahead, onSetForecastAlgo, wizard }) {
  return html`<div class="usage-card-controls">
    <div class="usage-control-group" role="group" aria-label=${`${wizard ? 'Chronicle' : 'History'} range, ${scope}`}>
      <span class="usage-control-label" aria-hidden="true">${wizard ? 'Chronicle' : 'History'}</span>
      ${USAGE_HISTORY_SPANS.map((option) => html`<button type="button"
        class=${`tool${span.hours === option.hours ? ' active' : ''}`}
        aria-label=${`${wizard ? 'Chronicle' : 'History'} ${option.label}, ${scope}`} aria-pressed=${span.hours === option.hours}
        onClick=${() => onSetHours(option.hours)}>${option.label}</button>`)}
    </div>
    <span class="usage-control-divider" aria-hidden="true"></span>
    <div class="usage-control-group" role="group" aria-label=${`${wizard ? 'Prophecy' : 'Forecast'} lookahead, ${scope}`}>
      <span class="usage-control-label" aria-hidden="true">${wizard ? 'Scry ahead' : 'Look ahead'}</span>
      ${USAGE_LOOKAHEAD_SPANS.map((option) => html`<button type="button"
        class=${`tool${span.lookaheadHours === option.hours ? ' active' : ''}`}
        aria-label=${`${wizard ? 'Scry ahead' : 'Look ahead'} ${option.label}, ${scope}`} aria-pressed=${span.lookaheadHours === option.hours}
        onClick=${() => onSetLookahead(option.hours)}>${option.label}</button>`)}
    </div>
    <span class="usage-control-divider" aria-hidden="true"></span>
    ${/* Which pace the dashed line is drawn at. It rides with the span controls
        rather than sitting once at the top of the tab: the answer differs per
        graph (a 5-hour window mid-burst and a 7-day window want different
        algorithms), and it reads against the same chart the spans do. */''}
    <div class="usage-control-group" role="group" aria-label=${`${wizard ? 'Prophecy' : 'Prediction'} algorithm, ${scope}`}>
      <span class="usage-control-label" aria-hidden="true">${wizard ? 'Divination' : 'Prediction'}</span>
      ${USAGE_FORECAST_ALGOS.map((option) => {
        const label = wizard ? option.wizardLabel : option.label;
        const description = wizard ? option.wizardDescription : option.description;
        return html`<button type="button"
          class=${`tool${span.algo === option.id ? ' active' : ''}`}
          title=${`${label}: ${description}`}
          aria-label=${`${wizard ? 'Prophecy' : 'Prediction'} ${label}, ${description}, ${scope}`}
          aria-pressed=${span.algo === option.id}
          onClick=${() => onSetForecastAlgo(option.id)}>${label}</button>`;
      })}
    </div>
  </div>`;
}

function UsageEmptyRangeControls({ hours, onSetHours, wizard }) {
  return html`<div class="usage-card-controls usage-empty-range">
    <div class="usage-control-group" role="group"
      aria-label=${wizard ? 'OpenCode activity chronicle range' : 'OpenCode activity history range'}>
      <span class="usage-control-label" aria-hidden="true">${wizard ? 'Search chronicle' : 'Search history'}</span>
      ${USAGE_HISTORY_SPANS.map((option) => html`<button type="button"
        class=${`tool${hours === option.hours ? ' active' : ''}`}
        aria-pressed=${hours === option.hours}
        onClick=${() => onSetHours(option.hours)}>${option.label}</button>`)}
    </div>
  </div>`;
}

// The legend sits under each chart rather than once at the top of the tab: with
// graphs side by side there is no longer a single line of sight from a shared
// legend to the chart you are reading.
//
// aria-hidden, and deliberately unlabelled: it is a visual key to SVG stroke
// styles, which a screen-reader user cannot perceive in the first place — the
// chart itself carries the accessible name (role="group" in
// usage-history-chart.js). Labelling it would also be inert, since aria-label
// on a bare div maps to role=generic, where naming is prohibited and browsers
// drop it. Repeating it per card makes both points sharper: N inert labels, or
// N recitals of "Observed Forecast Reset Now" before each chart.
function UsageChartLegend({ wizard }) {
  return html`<div class="usage-chart-legend" aria-hidden="true">
    <span><i class="usage-legend-swatch observed"></i>${wizard ? 'Channeled' : 'Observed'}</span>
    <span><i class="usage-legend-swatch forecast"></i>${wizard ? 'Prophecy' : 'Forecast'}</span>
    <span><i class="usage-legend-swatch reset"></i>${wizard ? 'Replenishment' : 'Reset'}</span>
    <span><i class="usage-legend-swatch now"></i>${wizard ? 'This moment' : 'Now'}</span>
    <span><i class="usage-legend-swatch excluded"></i>${wizard ? 'Veiled' : 'Excluded'}</span>
  </div>`;
}

function UsageSeriesCard({
  series, payload, span, onSetHours, onSetLookahead, onSetForecastAlgo, onTogglePoint, wizard,
}) {
  const w = (plain, wizardly) => (wizard ? wizardly : plain);
  const includedPoints = (series.points || []).filter((point) => !point.excluded);
  const latest = includedPoints[includedPoints.length - 1];
  const now = new Date(payload.generated_at).getTime();
  const selectedForecast = usageForecastOf(series, span.algo);
  const forecast = usageForecastView(selectedForecast, now, latest?.at, wizard);
  const resetCount = series.reset_count ?? series.resets?.length ?? 0;
  const windowLabel = usageWindowScopeLabel(series, wizard);
  const scope = usageScopeLabel(series, wizard);
  const sampled = latest ? `${w('sampled', 'scried')} ${formatUsageTime(latest.at, now)}` : w('no sample', 'no reading');
  const reset = latest?.resets_at ? formatUsageResetCountdown(latest.resets_at, now, wizard) : '';
  return html`<article class="usage-series-card">
    <div class="usage-card-header">
      <div><span class="usage-provider">${usageProviderLabel(series.provider)}</span>
        <h3>${windowLabel}</h3></div>
      <div class="usage-current"><strong>${latest ? `${latest.pct.toFixed(1)}%` : '—'}</strong>
        <span>${sampled}${reset ? ` · ${reset}` : ''}</span></div>
    </div>
    <${UsageSpanControls} scope=${scope} span=${span} onSetHours=${onSetHours} onSetLookahead=${onSetLookahead}
      onSetForecastAlgo=${onSetForecastAlgo} wizard=${wizard} />
    <${UsageHistoryChart} series=${series} from=${series.from ?? payload.from} generatedAt=${payload.generated_at}
      forecast=${selectedForecast} lookaheadHours=${span.lookaheadHours} wizard=${wizard} onTogglePoint=${onTogglePoint} />
    <${UsageChartLegend} wizard=${wizard} />
    <div class=${`usage-card-footer usage-forecast ${forecast.tone}`}>
      <strong>${forecast.headline}</strong>
      ${(forecast.lines || []).map((line) => html`<span class="usage-forecast-line-copy" key=${line}>${line}</span>`)}
      ${resetCount ? html`<span class="usage-reset-count">${resetCount} ${w(`reset${resetCount === 1 ? '' : 's'} detected in view`, `replenishment${resetCount === 1 ? '' : 's'} witnessed`)}</span>` : null}
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

// The warning fires only when OpenCode activity outran the newest native
// sample for that provider, so the copy names that tail — a sample taken after
// an OpenCode turn already contains its spend, and saying "no coverage in this
// span" when the graph ends on a fresh sample reads as a false alarm.
function UsageCoverageWarnings({ warnings, generatedAt }) {
  if (!warnings?.length) return null;
  const now = new Date(generatedAt).getTime();
  return html`<div class="usage-coverage-warnings" role="status">
    ${warnings.map((warning) => {
      const provider = usageProviderLabel(warning.provider);
      const models = (warning.models || []).length ? ` (${warning.models.join(', ')})` : '';
      const native = usageProviderLabel(warning.native_source);
      // The lag is stated as a duration rather than two relative times: both
      // sides routinely round into the same bucket ("1h ago" … "1h ago"),
      // which reads as a contradiction rather than a gap.
      const lag = new Date(warning.activity_to).getTime() - new Date(warning.native_latest).getTime();
      const source = !warning.native_source
        ? 'This provider has no corresponding native usage source in tclaude.'
        : warning.native_latest
          ? `The most recent OpenCode activity (${formatUsageTime(warning.activity_to, now)}) came ${formatUsageDuration(lag)} after the newest ${native} sample.`
          : `No ${native} native usage-history coverage was recorded for the selected span.`;
      return html`<div class="usage-coverage-warning" key=${warning.provider}>
        <strong>⚠ ${provider}${models}</strong> — OpenCode was active in this span, but OpenCode does not export provider-account usage-limit history.
        ${source} Available graphs remain visible, but they may be incomplete or stale.
      </div>`;
    })}
  </div>`;
}

export function UsageHistoryApp({ state, actions }) {
  const current = state.view.value;
  const wizard = useWizardTheme();
  const w = (plain, wizardly) => (wizard ? wizardly : plain);
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
  const setDefaultSpan = (hours) => { if (state.setDefaultHours(hours)) void actions.load(); };
  const setLookahead = (key, hours) => state.setSeriesLookaheadHours(key, hours);
  const setForecastAlgo = (key, algo) => state.setSeriesForecastAlgo(key, algo);
  const togglePoint = (series, point) => actions.setPointExcluded(series, point, !point.excluded);
  // Nothing but load state sits above the graphs: the legend now rides with
  // each chart and the explanatory note is a footnote below the grid, so the
  // tab opens on the data rather than on a paragraph about it.
  return html`<div class="usage-history-island">
    ${/* Always emitted, revealed by CSS in wizard mode only — same shape as the
        Debug tab's .debug-wizard-title and the Config almanac's. Unlike the
        copy below it, the nameplate has a stable two-state twin, so it does not
        need to wait on the island's repaint. aria-hidden: it is decorative
        chrome, and the nav tab already names the tab for a screen reader. */''}
    <h2 class="usage-wizard-title" aria-hidden="true">🔮 The Mana Reserves</h2>
    <${AsyncLoadState} label=${w('Usage', 'Reserves')} request=${current.request} retry=${actions.load} errorClass="usage-history-error" />
    ${current.request.hasLoaded && html`<${Fragment}>
      <${UsageCoverageWarnings} warnings=${current.payload?.coverage_warnings}
        generatedAt=${current.payload?.generated_at} />
      ${current.series.length
        ? html`<div class="usage-series-grid">${groupSeriesByProvider(current.series).map((row) => html`
            <div class="usage-provider-row" key=${row.provider} style=${`--usage-cols:${row.series.length}`}>
              ${row.series.map((series) => {
                const key = usageSeriesKeyOf(series);
                return html`<${UsageSeriesCard} key=${key} series=${series} payload=${current.payload}
                  span=${current.spanFor(key)} onSetHours=${(hours) => setSpan(key, hours)}
                  onSetLookahead=${(hours) => setLookahead(key, hours)}
                  onSetForecastAlgo=${(algo) => setForecastAlgo(key, algo)}
                  onTogglePoint=${(point) => togglePoint(series, point)} wizard=${wizard} />`;
              })}
            </div>`)}</div>`
        : html`<div class="empty">${w('No subscription usage samples in this range yet.', 'No mana readings have been taken in this span yet.')}
            <${UsageEmptyRangeControls} hours=${current.defaultHours} onSetHours=${setDefaultSpan} wizard=${wizard} />
          </div>`}
    </${Fragment}>`}
    <p class="usage-history-note">${w(
      'Account-wide provider limits, sampled every 15 minutes; click a point to exclude or restore it. Excluded points stay visible but do not affect lines, resets, current values, or predictions. History and look-ahead spans persist per graph. Forecasts are per provider × quota window. Providers do not expose reliable per-model quota attribution. A dashed line is the predicted pace, drawn with the algorithm each graph selects: Span extrapolates the straight line from the first to the last sample in view, Recent uses only the newest few samples, and Fit is a least-squares pace over the whole post-reset segment. Downward steps of at least 2 points are treated as out-of-cycle resets.',
      'Account-wide provider wards, scried every 15 minutes; click a reading to veil or reveal it. Veiled readings remain visible but do not shape traces, replenishments, current reserves, or prophecies. Chronicle and scry-ahead spans persist per graph. Prophecies are cast per provider × mana cycle. The providers reveal no trustworthy per-model attribution. A dashed line is the foretold pace, cast by the divination each graph selects: Arc extends the line from the first reading in the span to the last, Pulse heeds only the last few readings, and Weave averages every reading since the last replenishment. Downward steps of at least 2 percentage points are read as an unforeseen replenishment.',
    )}</p>
  </div>`;
}

export function mountUsageHistoryIsland({ host, state, actions, registerCleanup }) {
  state.initialize();
  render(html`<${UsageHistoryApp} state=${state} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}
