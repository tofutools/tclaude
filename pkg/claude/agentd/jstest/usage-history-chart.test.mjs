import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('usage chart renders even time ticks and self-contained hover titles', async (t) => {
  const harness = await createPreactHarness(t);
  const { UsageHistoryChart } = await harness.importDashboardModule('js/usage-history-chart.js');
  const now = Date.UTC(2026, 6, 18, 12);
  const day = 24 * 60 * 60_000;
  const series = {
    provider: 'openai',
    window_name: 'seven_day',
    duration_seconds: 7 * 24 * 60 * 60,
    points: [{ at: new Date(now).toISOString(), pct: 12.5, source: 'codex-cli' }],
    resets: [],
    forecast: {
      status: 'before_reset',
      rate_pct_per_hour: 1,
      hits_limit_at: new Date(now + day).toISOString(),
      reset_at: new Date(now + 7 * day).toISOString(),
    },
  };

  const view = await harness.mount(harness.preact.h(UsageHistoryChart, {
    series,
    from: new Date(now - 7 * day).toISOString(),
    generatedAt: new Date(now).toISOString(),
  }));

  const ticks = [...view.container.querySelectorAll('.usage-x-tick line')];
  assert.equal(ticks.length, 5);
  assert.deepEqual(ticks.map((line) => Number(line.getAttribute('x1'))), [42, 207, 372, 537, 702]);

  const pointTitle = view.container.querySelector('.usage-point title').textContent;
  assert.match(pointTitle, /Codex · 7 day window · 12\.5%/);
  assert.match(pointTitle, /source: codex-cli/);
  const forecastTitle = view.container.querySelector('.usage-forecast-line title').textContent;
  assert.match(forecastTitle, /Codex · 7 day window · forecast/);

  await view.unmount();
});
