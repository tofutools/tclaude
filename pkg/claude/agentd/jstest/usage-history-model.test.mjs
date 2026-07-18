import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('usage prediction copy states lockout duration and unambiguous average rate', async (t) => {
  const harness = await createPreactHarness(t);
  const { formatUsageDuration, usageForecastView } =
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
  assert.equal(forecast.headline, 'Prediction: limit hit in 47h (5d 1h before reset)');
  assert.deepEqual(forecast.lines, [
    'Predicted time without quota access: 5d 1h',
    'Average usage rate: 1.9 percentage points/hour',
  ]);
});
