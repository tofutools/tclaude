import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('spend banner follows snapshot thresholds and clears when disabled', async (t) => {
  const harness = await createPreactHarness(t);
  const { SpendLimitBanner } = await harness.importDashboardModule('js/spend-limits.js');
  const state = { snapshot: harness.signals.signal(null) };
  const mounted = await harness.mount(harness.html`<${SpendLimitBanner} state=${state} />`);
  assert.equal(mounted.container.querySelector('[role="status"]'), null);
  const update = async (threshold, spend) => harness.act(() => {
    state.snapshot.value = { usage: { spend_limits: [
      { period: 'daily', spend_usd: spend, limit_usd: 10, threshold },
    ] } };
  });
  await update(0, 7);
  assert.equal(mounted.container.querySelector('[role="status"]'), null);
  await update(80, 8);
  assert.match(mounted.container.textContent, /Daily estimated API spend: \$8\.00 \/ \$10\.00 \(80% warning\)/);
  assert.match(mounted.container.textContent, /estimates may differ from charges/);
  await update(100, 12);
  assert.match(mounted.container.textContent, /limit reached/);
  assert.ok(mounted.container.querySelector('.over-limit'));
  await harness.act(() => { state.snapshot.value = { usage: {} }; });
  assert.equal(mounted.container.querySelector('[role="status"]'), null);
  await mounted.unmount();
});
