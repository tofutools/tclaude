import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('production probe renders and reacts through the vendored import-map graph', async (t) => {
  const harness = await createPreactHarness(t);
  const probe = await harness.importDashboardModule('js/preact-probe.js');

  const host = harness.document.createElement('span');
  harness.document.body.append(host);
  let unmount;
  await harness.act(() => {
    unmount = probe.mountPreactProbe(host);
  });

  assert.equal(host.firstElementChild?.getAttribute('data-preact-probe'), 'ready');
  assert.equal(host.textContent.trim(), 'ready');
  await harness.act(unmount);
  assert.equal(host.childElementCount, 0);
});
