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
      rate_pct_per_hour: 87.5 / 24,
      hits_limit_at: new Date(now + day).toISOString(),
      reset_at: new Date(now + 7 * day).toISOString(),
    },
  };

  const view = await harness.mount(harness.preact.h(UsageHistoryChart, {
    series,
    from: new Date(now - 7 * day).toISOString(),
    generatedAt: new Date(now).toISOString(),
    lookaheadHours: 168,
  }));

  const ticks = [...view.container.querySelectorAll('.usage-x-tick line')];
  assert.equal(ticks.length, 5);
  assert.deepEqual(ticks.map((line) => Number(line.getAttribute('x1'))), [42, 207, 372, 537, 702]);

  const pointTitle = view.container.querySelector('.usage-point title').textContent;
  assert.match(pointTitle, /Codex · 7 day window · 12\.5%/);
  assert.doesNotMatch(pointTitle, /source|codex-cli/i);

  const reset = view.container.querySelector('.usage-scheduled-reset');
  assert.ok(reset, 'upcoming reset inside lookahead is rendered');
  assert.match(reset.querySelector('title').textContent, /Codex · 7 day window · scheduled reset/);

  const forecastTarget = view.container.querySelector('.usage-forecast-hit-target');
  assert.match(forecastTarget.getAttribute('aria-label'), /100\.0%.*6d before reset/);
  const svg = view.container.querySelector('svg');
  svg.getBoundingClientRect = () => ({ left: 0, width: 720 });
  await harness.act(() => harness.fireEvent(forecastTarget, 'mousemove', { clientX: 702 }));
  assert.match(view.container.querySelector('.usage-forecast-tooltip').textContent, /100\.0%.*6d before reset/s);
  await harness.act(() => harness.fireEvent(forecastTarget, 'mouseleave'));
  assert.equal(view.container.querySelector('.usage-forecast-tooltip'), null);

  await view.rerender(harness.preact.h(UsageHistoryChart, {
    series,
    from: new Date(now - 7 * day).toISOString(),
    generatedAt: new Date(now).toISOString(),
    lookaheadHours: 5,
  }));
  assert.equal(view.container.querySelector('.usage-scheduled-reset'), null,
    'a reset beyond the chosen lookahead is not clamped onto the chart edge');
  assert.match(view.container.querySelector('.usage-forecast-hit-target').getAttribute('aria-label'), /30\.7%/);

  await view.unmount();
});
