import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('usage limit banner follows crossed steps and clears when nothing is crossed', async (t) => {
  const harness = await createPreactHarness(t);
  const { SpendLimitBanner } = await harness.importDashboardModule('js/spend-limits.js');
  const state = { snapshot: harness.signals.signal(null) };
  const mounted = await harness.mount(harness.html`<${SpendLimitBanner} state=${state} />`);
  assert.equal(mounted.container.querySelector('[role="status"]'), null);
  const update = async (limit) => harness.act(() => {
    state.snapshot.value = { usage: { usage_limits: [
      { scope: 'global', name: 'Monthly', metric: 'total_usd', period: 'month', value: 0, budget: 500, pct: 0, ...limit },
    ] } };
  });
  await update({ value: 100, pct: 20 });
  assert.equal(mounted.container.querySelector('[role="status"]'), null);
  await update({ value: 250, pct: 50, reached: 'notify' });
  assert.match(mounted.container.textContent, /Global limit "Monthly": \$250\.00 \/ \$500\.00 this month \(50%, warning\)/);
  assert.match(mounted.container.textContent, /estimates may differ from charges/);
  assert.equal(mounted.container.querySelector('.over-limit'), null);
  await update({ value: 460, pct: 92, reached: 'wrap_up' });
  assert.match(mounted.container.textContent, /agents asked to wrap up/);
  assert.ok(mounted.container.querySelector('.over-limit'));
  await update({ value: 460, pct: 92, reached: 'wrap_up', dry_run: true });
  assert.equal(mounted.container.querySelector('[role="status"]'), null);
  await harness.act(() => { state.snapshot.value = { usage: {} }; });
  assert.equal(mounted.container.querySelector('[role="status"]'), null);
  await mounted.unmount();
});
