import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// Mounts the Config tab over a given saved config and returns the mounted
// tree plus the adapter, so a test can assert both what the form SHOWS and
// what it would SAVE.
async function mountConfig(harness, config) {
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const fetchImpl = async () => ({
    ok: true,
    json: async () => ({ raw: JSON.stringify(config), path: '/tmp/config.json' }),
  });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{ fetchImpl }} />`);
  await adapter.loadConfigTab();
  assert.notEqual(state.view.value.phase, 'error', state.view.value.error);
  return { mounted, adapter };
}

const selectedDelivery = (mounted) => {
  const select = mounted.container.querySelector('#cfg-notif-delivery');
  return [...select.querySelectorAll('option')].find(o => o.selected)?.value;
};

test('notification delivery round-trips os / browser / both through the Config tab', async (t) => {
  const harness = await createPreactHarness(t);

  for (const delivery of ['browser', 'both']) {
    const { mounted, adapter } = await mountConfig(harness, { notifications: { enabled: true, delivery } });
    assert.equal(selectedDelivery(mounted), delivery, `${delivery} must survive a load`);
    assert.equal(adapter.assembleConfig().notifications.delivery, delivery,
      `${delivery} must survive a save`);
    await mounted.unmount();
  }
});

test('an absent delivery loads as os and is saved back absent, not as an explicit default', async (t) => {
  const harness = await createPreactHarness(t);
  const { mounted, adapter } = await mountConfig(harness, { notifications: { enabled: true } });

  assert.equal(selectedDelivery(mounted), 'os', 'the unset state reads as the desktop default');
  // Writing "os" back would grow every legacy config file with a key that
  // means exactly what its absence already meant.
  assert.equal('delivery' in adapter.assembleConfig().notifications, false);
  await mounted.unmount();
});

test('an unrecognised delivery falls back to os in the form without blanking the select', async (t) => {
  const harness = await createPreactHarness(t);
  // A config hand-written for a newer tclaude, or a typo. The server's
  // Validate is what tells the human it is wrong; the form must still
  // render something coherent rather than an empty select.
  const { mounted } = await mountConfig(harness, { notifications: { enabled: true, delivery: 'carrier-pigeon' } });

  assert.equal(selectedDelivery(mounted), 'os');
  await mounted.unmount();
});

test('the browser permission control reports an unsupported context instead of offering a grant', async (t) => {
  const harness = await createPreactHarness(t);
  // The LinkeDOM test window has no Notification constructor — the same
  // shape as a dashboard reached over plain http at a LAN IP, where the
  // API is absent and no amount of clicking can grant anything.
  const { mounted } = await mountConfig(harness, { notifications: { enabled: true, delivery: 'browser' } });

  const section = mounted.container.querySelector('#cfg-notif-delivery').closest('.cfg-field');
  assert.match(section.textContent, /cannot raise notifications/i);
  assert.equal(section.querySelector('button'), null, 'no grant button when the API is absent');
  await mounted.unmount();
});
