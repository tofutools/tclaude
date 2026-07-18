import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('usage chart renders even time ticks and unified immediate tooltips', async (t) => {
  const harness = await createPreactHarness(t);
  const { UsageHistoryChart } = await harness.importDashboardModule('js/usage-history-chart.js');
  const now = Date.UTC(2026, 6, 18, 12);
  const day = 24 * 60 * 60_000;
  const series = {
    provider: 'openai',
    window_name: 'seven_day',
    duration_seconds: 7 * 24 * 60 * 60,
    points: [
      {
        at: new Date(now - 2 * 60 * 60_000).toISOString(), pct: 10, source: 'codex-cli',
        resets_at: new Date(now + 2 * day).toISOString(),
      },
      {
        at: new Date(now).toISOString(), pct: 12.5, source: 'codex-cli',
        resets_at: new Date(now + 7 * day).toISOString(),
      },
    ],
    resets: [
      { at: new Date(now - 3 * day).toISOString(), pct: 2 },
      { at: new Date(now - day).toISOString(), pct: 3.5 },
    ],
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
  const chart = view.container.querySelector('svg');
  assert.equal(chart.getAttribute('role'), 'group');

  const points = [...view.container.querySelectorAll('.usage-point-hit-target')];
  assert.equal(points.filter((item) => item.getAttribute('tabIndex') === '0').length, 1,
    'only the latest sample is in sequential keyboard navigation');
  assert.ok(points.every((item) => item.getAttribute('r') === '8'), 'samples have a generous hover target');
  const hitTargets = [...chart.querySelectorAll('.usage-forecast-hit-target, .usage-marker-hit-target, .usage-point-hit-target')];
  const firstPointTarget = hitTargets.findIndex((item) => item.classList.contains('usage-point-hit-target'));
  const lastLineTarget = hitTargets.findLastIndex((item) => !item.classList.contains('usage-point-hit-target'));
  assert.ok(firstPointTarget > lastLineTarget, 'sample hit targets render above every dashed-line target');
  const point = points[points.length - 1];
  assert.match(point.getAttribute('aria-label'), /Codex · 7 day window; 12\.5%/);
  assert.match(point.getAttribute('aria-label'), /7d before reset/,
    'screen-reader text includes the point-specific reset timing');
  assert.doesNotMatch(point.getAttribute('aria-label'), /source|codex-cli/i);
  await harness.act(() => harness.fireEvent(point, 'mouseenter'));
  assert.match(view.container.querySelector('.usage-chart-tooltip.observed').textContent,
    /Codex · 7 day window.*12\.5%.*7d before reset/s);
  await harness.act(() => harness.fireEvent(point, 'mouseleave'));

  const keyboardPoints = [...view.container.querySelectorAll('.usage-point-hit-target')];
  const keyboardPoint = keyboardPoints[keyboardPoints.length - 1];
  let focusedPrevious = false;
  keyboardPoints[0].addEventListener('focus', () => { focusedPrevious = true; });
  await harness.act(() => harness.fireEvent(keyboardPoint, 'keydown', { key: 'ArrowLeft' }));
  assert.equal(focusedPrevious, true, 'left arrow moves toward the previous sample');
  const movedPoints = [...view.container.querySelectorAll('.usage-point-hit-target')];
  assert.deepEqual(movedPoints.map((item) => item.getAttribute('tabIndex')), ['0', '-1'],
    'roving tab stop follows the arrow-key selection');
  assert.match(view.container.querySelector('.usage-chart-tooltip.observed').textContent,
    /10\.0%.*2d 2h before reset/s, 'historical sample uses its own reported reset boundary');
  await harness.act(() => harness.fireEvent(keyboardPoints[0], 'blur'));

  const reset = view.container.querySelector('.usage-scheduled-reset');
  assert.ok(reset, 'upcoming reset inside lookahead is rendered');
  const resetTarget = reset.querySelector('.usage-marker-hit-target');
  assert.match(resetTarget.getAttribute('aria-label'), /Next reset; Codex · 7 day window/);
  await harness.act(() => harness.fireEvent(resetTarget, 'mouseenter'));
  assert.match(view.container.querySelector('.usage-chart-tooltip.reset').textContent,
    /Next reset.*Codex · 7 day window.*7d remaining/s);
  await harness.act(() => harness.fireEvent(resetTarget, 'mouseleave'));

  const detectedResetTargets = [...view.container.querySelectorAll('.usage-reset-mark .usage-marker-hit-target')];
  assert.deepEqual(detectedResetTargets.map((item) => item.getAttribute('tabIndex')), ['-1', '0'],
    'detected resets expose one roving tab stop');
  const detectedResetTarget = detectedResetTargets[detectedResetTargets.length - 1];
  assert.match(detectedResetTarget.getAttribute('aria-label'), /new post-reset baseline 3\.5%/);
  await harness.act(() => harness.fireEvent(detectedResetTarget, 'focus'));
  assert.match(view.container.querySelector('.usage-chart-tooltip.reset').textContent,
    /Last reset.*New post-reset baseline: 3\.5%.*1d ago/s);
  await harness.act(() => harness.fireEvent(detectedResetTarget, 'blur'));

  const resetKeyboardTargets = [...view.container.querySelectorAll('.usage-reset-mark .usage-marker-hit-target')];
  let focusedPreviousReset = false;
  resetKeyboardTargets[0].addEventListener('focus', () => { focusedPreviousReset = true; });
  await harness.act(() => harness.fireEvent(resetKeyboardTargets[1], 'keydown', { key: 'ArrowLeft' }));
  assert.equal(focusedPreviousReset, true, 'left arrow moves toward the previous detected reset');
  assert.deepEqual(
    [...view.container.querySelectorAll('.usage-reset-mark .usage-marker-hit-target')]
      .map((item) => item.getAttribute('tabIndex')),
    ['0', '-1'],
  );
  assert.match(view.container.querySelector('.usage-chart-tooltip.reset').textContent, /Previous reset/);
  await harness.act(() => harness.fireEvent(resetKeyboardTargets[0], 'blur'));

  const nowTarget = view.container.querySelector('.usage-now-mark .usage-marker-hit-target');
  assert.equal(view.container.querySelector('.usage-now-mark text'), null, 'now is labelled by its tooltip only');
  await harness.act(() => harness.fireEvent(nowTarget, 'mouseenter'));
  assert.match(view.container.querySelector('.usage-chart-tooltip.now').textContent,
    /Now.*Quota resets in 7d/s);
  await harness.act(() => harness.fireEvent(nowTarget, 'mouseleave'));

  const forecastTarget = view.container.querySelector('.usage-forecast-hit-target');
  assert.equal(forecastTarget.getAttribute('role'), 'img');
  assert.match(forecastTarget.getAttribute('aria-label'), /100\.0%.*6d before reset/);
  chart.getBoundingClientRect = () => ({ left: 0, width: 720 });
  await harness.act(() => harness.fireEvent(forecastTarget, 'mousemove', { clientX: 702 }));
  assert.match(view.container.querySelector('.usage-chart-tooltip.forecast').textContent, /Prediction.*100\.0%.*6d before reset/s);
  await harness.act(() => harness.fireEvent(forecastTarget, 'mouseleave'));
  assert.equal(view.container.querySelector('.usage-chart-tooltip'), null);

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
