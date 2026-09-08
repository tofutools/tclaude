import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

test('imperative Costs chart owns descendants, tooltip listeners, updates, and cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const { mountImperativeCostChart } = await harness.importDashboardModule('js/costs-chart.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const chart = {
    scaleMax: 5,
    days: [{ day: '2026-07-10', cost: 5, projected: false, segments: [
      { provider: 'anthropic', cost: 3, className: 'cost-seg-h0' },
      { provider: 'openai', cost: 2, className: 'cost-seg-h1' },
    ] }],
  };
  const cleanup = mountImperativeCostChart(host, chart);
  assert.equal(host.querySelectorAll('.cost-seg').length, 2);
  const column = host.querySelector('.cost-col[data-tip]');
  assert.equal(column.getAttribute('tabindex'), '0');
  assert.match(column.getAttribute('aria-label'), /Breakdown: anthropic \$3\.00, openai \$2\.00/);
  harness.fireEvent(column, 'mousemove', { clientX: 20, clientY: 30 });
  assert.equal(harness.document.body.querySelector('.cost-tip .cost-tip-row')?.textContent.includes('anthropic'), true);
  cleanup();
  assert.equal(host.childElementCount, 0);
  assertAbsent(harness.document.body.querySelector('.cost-tip'));
  harness.fireEvent(column, 'mousemove', { clientX: 20, clientY: 30 });
  assertAbsent(harness.document.body.querySelector('.cost-tip'), 'removed listener cannot recreate tooltip');
});

test('Costs chart names a single-day provider when the selected span has multiple providers', async (t) => {
  const harness = await createPreactHarness(t);
  const { mountImperativeCostChart } = await harness.importDashboardModule('js/costs-chart.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const chart = {
    scaleMax: 3,
    days: [
      { day: '2026-07-09', cost: 3, projected: false, segments: [
        { provider: 'anthropic', cost: 3, className: 'cost-seg-h0' },
      ] },
      { day: '2026-07-10', cost: 2, projected: false, segments: [
        { provider: 'openai', cost: 2, className: 'cost-seg-h1' },
      ] },
    ],
  };
  const cleanup = mountImperativeCostChart(host, chart);
  const columns = host.querySelectorAll('.cost-col[data-tip]');

  harness.fireEvent(columns[0], 'mousemove', { clientX: 20, clientY: 30 });
  const tooltip = harness.document.body.querySelector('.cost-tip');
  assert.equal(tooltip.querySelectorAll('.cost-tip-row').length, 1);
  assert.match(tooltip.textContent, /anthropic/);
  assert.doesNotMatch(tooltip.textContent, /openai/);

  harness.fireEvent(columns[1], 'mousemove', { clientX: 20, clientY: 30 });
  assert.equal(tooltip.querySelectorAll('.cost-tip-row').length, 1);
  assert.match(tooltip.textContent, /openai/);
  assert.doesNotMatch(tooltip.textContent, /anthropic/);
  columns[0].focus();
  harness.fireEvent(columns[0], 'focusin');
  assert.match(host.querySelector('.cost-chart-status').textContent, /Breakdown: anthropic \$3\.00/,
    'keyboard inspection announces the same breakdown as hover');
  harness.fireEvent(columns[0], 'keydown', { key: 'ArrowRight' });
  assert.equal(harness.document.activeElement, columns[1], 'arrow keys move between spend days');
  cleanup();
});
