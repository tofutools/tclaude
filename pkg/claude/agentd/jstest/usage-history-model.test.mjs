import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Copilot monthly quota uses product and AIC vocabulary', async (t) => {
  const harness = await createPreactHarness(t);
  const { usageProviderLabel, usageWindowLabel, usageWindowScopeLabel } =
    await harness.importDashboardModule('js/usage-history-model.js');
  const series = { provider: 'github', window_name: 'monthly', duration_seconds: 30 * 24 * 3600 };
  assert.equal(usageProviderLabel(series.provider), 'GitHub Copilot');
  assert.equal(usageWindowLabel(series.window_name, series.duration_seconds), 'Monthly premium requests (AIC)');
  assert.equal(usageWindowScopeLabel(series), 'Monthly premium requests (AIC)');
});

test('usage prediction copy states lockout duration and unambiguous average rate', async (t) => {
  const harness = await createPreactHarness(t);
  const { formatUsageDuration, formatUsageResetCountdown, usageForecastView } =
    await harness.importDashboardModule('js/usage-history-model.js');
  const now = Date.UTC(2026, 6, 18, 12);
  const hour = 60 * 60_000;
  const forecast = usageForecastView({
    status: 'before_reset',
    rate_pct_per_hour: 1.875,
    hits_limit_at: new Date(now + 47 * hour).toISOString(),
    reset_at: new Date(now + 7 * 24 * hour).toISOString(),
  }, now);

  assert.equal(formatUsageDuration(2 * 24 * hour + 3 * hour + 17 * 60_000), '2d 3h 17m');
  assert.equal(
    formatUsageResetCountdown(new Date(now + 6 * 24 * hour + 23 * hour + 17 * 60_000).toISOString(), now),
    'resets in 6d 23h 17m',
  );
  assert.equal(formatUsageResetCountdown(new Date(now - 60_001).toISOString(), now), 'reset 1m ago');
  assert.equal(formatUsageResetCountdown('', now), '', 'missing reset metadata adds no confusing unknown/reset copy');
  assert.equal(forecast.headline, 'Prediction: limit hit in 47h (5d 1h before reset)');
  assert.deepEqual(forecast.lines, [
    'Predicted time without quota access: 5d 1h',
    'Average usage rate: 1.9 percentage points/hour',
  ]);
});

// The wizard voice is a parameter, not a separate code path: the same forecast
// must produce the same tone and the same numbers in either theme, so a theme
// flip can never change what the graph claims — only how it says it.
test('usage copy speaks the wizard voice without changing the reading', async (t) => {
  const harness = await createPreactHarness(t);
  const { formatUsageResetCountdown, usageForecastView } =
    await harness.importDashboardModule('js/usage-history-model.js');
  const now = Date.UTC(2026, 6, 18, 12);
  const hour = 60 * 60_000;
  const input = {
    status: 'before_reset',
    rate_pct_per_hour: 1.875,
    hits_limit_at: new Date(now + 47 * hour).toISOString(),
    reset_at: new Date(now + 7 * 24 * hour).toISOString(),
  };
  const plain = usageForecastView(input, now, '', false);
  const wizard = usageForecastView(input, now, '', true);

  assert.equal(
    formatUsageResetCountdown(new Date(now + 3 * hour).toISOString(), now, true),
    'replenishes in 3h',
  );
  assert.equal(formatUsageResetCountdown(new Date(now - 60_001).toISOString(), now, true), 'replenished 1m ago');
  assert.equal(wizard.headline, 'Prophecy: reserves run dry in 47h (5d 1h before replenishment)');
  assert.deepEqual(wizard.lines, [
    'Foretold time without mana: 5d 1h',
    // Keeps "percentage points": the plain copy names that unit deliberately
    // (see the test above), and the axis, tooltips and header all read %.
    'Channeling rate: 1.9 percentage points of mana per hour',
  ]);
  // Same urgency, different wording — and, the point of the check, the same
  // figures. Rather than restate literals the assertions above already pin,
  // pull every number out of the PLAIN copy and require the wizard copy to
  // carry all of them: a future edit that drops or rounds a figure only in the
  // wizard voice then fails here even though both exact-match assertions above
  // were updated in step with it.
  assert.equal(wizard.tone, plain.tone);
  assert.notEqual(wizard.headline, plain.headline);
  const figures = (view) => `${view.headline} ${view.lines.join(' ')}`.match(/\d+(?:\.\d+)?[a-z%]*/g) ?? [];
  const plainFigures = figures(plain);
  assert.ok(plainFigures.length >= 3, `expected the plain copy to carry figures, got ${plainFigures}`);
  const wizardFigures = new Set(figures(wizard));
  for (const figure of plainFigures) {
    assert.ok(wizardFigures.has(figure), `wizard copy dropped or altered the figure ${figure}`);
  }
});

test('forecast selection falls back to the default algorithm, never to nothing', async (t) => {
  const harness = await createPreactHarness(t);
  const { USAGE_FORECAST_ALGOS, usageForecastAlgoOf, usageForecastOf } =
    await harness.importDashboardModule('js/usage-history-model.js');
  assert.deepEqual(USAGE_FORECAST_ALGOS.map((algo) => algo.id), ['span', 'recent', 'fit'],
    'the ids must match usageForecastAlgos in usage_history.go');
  for (const algo of USAGE_FORECAST_ALGOS) {
    assert.ok(algo.label && algo.wizardLabel && algo.description && algo.wizardDescription,
      `${algo.id} needs copy in both voices`);
  }
  assert.equal(usageForecastAlgoOf('recent'), 'recent');
  assert.equal(usageForecastAlgoOf('guesswork'), 'span', 'an unknown stored preference lands on the default');
  assert.equal(usageForecastAlgoOf(undefined), 'span');

  const series = {
    forecast: { algorithm: 'span', status: 'projected' },
    forecasts: {
      span: { algorithm: 'span', status: 'projected' },
      recent: { algorithm: 'recent', status: 'flat' },
    },
  };
  assert.equal(usageForecastOf(series, 'recent').status, 'flat');
  assert.equal(usageForecastOf(series, 'guesswork').status, 'projected', 'unknown selection reads the default entry');
  // A series the server could not compute the selection for, and a payload
  // from before the algorithms existed, both still draw the default line
  // rather than leaving the card and the chart with nothing to show.
  assert.equal(usageForecastOf(series, 'fit'), series.forecast);
  assert.equal(usageForecastOf({ forecast: series.forecast }, 'recent'), series.forecast);
  assert.equal(usageForecastOf({}, 'span'), null);
  assert.equal(usageForecastOf(undefined, 'span'), null);
});
