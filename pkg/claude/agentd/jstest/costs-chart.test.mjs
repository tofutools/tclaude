import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('imperative Costs chart owns descendants, tooltip listeners, updates, and cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const { mountImperativeCostChart } = await harness.importDashboardModule('js/costs-chart.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const chart = {
    scaleMax: 5,
    days: [{ day: '2026-07-10', cost: 5, projected: false, segments: [
      { harness: 'claude', cost: 3, className: 'cost-seg-h0' },
      { harness: 'codex', cost: 2, className: 'cost-seg-h1' },
    ] }],
  };
  const cleanup = mountImperativeCostChart(host, chart);
  assert.equal(host.querySelectorAll('.cost-seg').length, 2);
  const column = host.querySelector('.cost-col[data-tip]');
  harness.fireEvent(column, 'mousemove', { clientX: 20, clientY: 30 });
  assert.equal(harness.document.body.querySelector('.cost-tip .cost-tip-row')?.textContent.includes('claude'), true);
  cleanup();
  assert.equal(host.childElementCount, 0);
  assert.equal(harness.document.body.querySelector('.cost-tip'), null);
  harness.fireEvent(column, 'mousemove', { clientX: 20, clientY: 30 });
  assert.equal(harness.document.body.querySelector('.cost-tip'), null, 'removed listener cannot recreate tooltip');
});

test('Costs chart names a single-day harness when the selected span has multiple harnesses', async (t) => {
  const harness = await createPreactHarness(t);
  const { mountImperativeCostChart } = await harness.importDashboardModule('js/costs-chart.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const chart = {
    scaleMax: 3,
    days: [
      { day: '2026-07-09', cost: 3, projected: false, segments: [
        { harness: 'claude', cost: 3, className: 'cost-seg-h0' },
      ] },
      { day: '2026-07-10', cost: 2, projected: false, segments: [
        { harness: 'codex', cost: 2, className: 'cost-seg-h1' },
      ] },
    ],
  };
  const cleanup = mountImperativeCostChart(host, chart);
  const columns = host.querySelectorAll('.cost-col[data-tip]');

  harness.fireEvent(columns[0], 'mousemove', { clientX: 20, clientY: 30 });
  const tooltip = harness.document.body.querySelector('.cost-tip');
  assert.equal(tooltip.querySelectorAll('.cost-tip-row').length, 1);
  assert.match(tooltip.textContent, /claude/);
  assert.doesNotMatch(tooltip.textContent, /codex/);

  harness.fireEvent(columns[1], 'mousemove', { clientX: 20, clientY: 30 });
  assert.equal(tooltip.querySelectorAll('.cost-tip-row').length, 1);
  assert.match(tooltip.textContent, /codex/);
  assert.doesNotMatch(tooltip.textContent, /claude/);
  cleanup();
});
