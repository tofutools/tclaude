import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// The Doctor checks are the node's own: on a peer's page they are read only
// once its snapshot says the peer shares them (unrestricted trust).
test("Doctor reads no checks from a peer's page until the peer is known to share them", async (t) => {
  const harness = await createPreactHarness(t);
  const [{ mountDoctorIsland }, { createDoctorState }] = await Promise.all([
    harness.importDashboardModule('js/doctor-island.js'),
    harness.importDashboardModule('js/doctor-state.js'),
  ]);
  const calls = [];
  t.mock.method(globalThis, 'fetch', async (url) => {
    calls.push(String(url));
    return { ok: true, status: 200, text: async () => JSON.stringify({ checks: [{ id: 'bwrap', category: 'sandbox', severity: 'blocking', title: 'bwrap' }] }) };
  });
  const shared = harness.signals.signal(false);
  const state = createDoctorState({ activeTab: harness.signals.signal('groups'), subtab: harness.signals.signal('audit') });
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const cleanups = [];
  await harness.act(() => mountDoctorIsland({ host, state, registerCleanup: (fn) => cleanups.push(fn), shares: () => shared.value }));
  const flush = async () => harness.act(async () => { for (let i = 0; i < 8; i += 1) await Promise.resolve(); });
  await flush();
  assert.deepEqual(calls, [], 'not shared (or not yet known): no request');
  assert.equal(state.phase.value, 'idle');

  await harness.act(() => { shared.value = true; });
  await flush();
  assert.deepEqual(calls, ['/api/doctor'], 'the first load runs once the node shares the checks');
  assert.equal(state.phase.value, 'ready');
  assert.equal(state.counts.value.blocking, 1);

  await harness.act(() => { shared.value = false; shared.value = true; });
  await flush();
  assert.equal(calls.length, 1, 'the gate opening again does not reload what is loaded');
  await harness.act(() => { for (const fn of cleanups) fn(); });
});
