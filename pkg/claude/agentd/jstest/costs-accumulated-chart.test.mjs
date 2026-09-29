import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('accumulated hover identifies the sloping band and emphasizes its cumulative tooltip row', async (t) => {
  const harness = await createPreactHarness(t);
  const { CostsAccumulatedChart } = await harness.importDashboardModule('js/costs-accumulated-chart.js');
  const { buildAccumulatedCostChart } = await harness.importDashboardModule('js/costs-model.js');
  const chart = buildAccumulatedCostChart({ stackByProvider: false, stackByModel: true,
    days: [1, 9].map((cost, i) => ({ day: `2026-07-${10 + i}`, cost: cost + 1, projected: !!i,
      segments: [
        { key: 'a', model: 'Alpha', cost, className: 'cost-series-0' },
        { key: 'b', model: 'Beta', cost: 1, className: 'cost-series-1' },
      ] })) });
  chart.scaleMax = 20;
  const mounted = await harness.mount(harness.html`<${CostsAccumulatedChart} chart=${chart} />`);
  t.after(() => mounted.unmount());
  const svg = mounted.container.querySelector('svg');
  svg.getBoundingClientRect = () => ({ left: 0, top: 0, width: 1000, height: 190 });
  const target = mounted.container.querySelector('.cost-accumulated-hover-target');
  const move = async (fraction, cost) => harness.act(() => harness.fireEvent(target, 'mousemove', {
    clientX: 48 + fraction * 936, clientY: 162 - cost / 20 * 147,
  }));
  await move(.4, 4);
  let row = mounted.container.querySelector('.cost-accumulated-tip-row.cost-series-focused');
  assert.match(row.textContent, /Alpha.*\$1.00/,
    'band hit uses interpolated heights even when the nearest day has a smaller total');
  assert.equal(mounted.container.querySelectorAll('g.cost-series-focused polygon').length, 1);
  assert.equal(mounted.container.querySelectorAll('g.cost-series-muted polygon').length, 1);
  await move(.6, 4);
  row = mounted.container.querySelector('.cost-accumulated-tip-row.cost-series-focused');
  assert.match(row.textContent, /Alpha.*≈\$10.00/,
    'the selected projected day shows accumulated model spend');
  await move(.6, 1);
  assert.match(mounted.container.querySelector('.cost-accumulated-tip-row.cost-series-focused').textContent, /Beta/);
  await move(.6, 15);
  assert.equal(mounted.container.querySelectorAll('.cost-series-focused, .cost-series-muted').length, 0);
  assert.ok(mounted.container.querySelector('.cost-accumulated-tip-panel'), 'outside the bands still inspects the day');
  await move(.6, 1);
  await harness.act(() => harness.fireEvent(target, 'mouseleave'));
  assert.equal(mounted.container.querySelectorAll('.cost-series-focused, .cost-series-muted').length, 0);
  assert.equal(mounted.container.querySelector('.cost-accumulated-tip-panel'), null);
});
