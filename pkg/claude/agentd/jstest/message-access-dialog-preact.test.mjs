import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('child chooser keeps the keyed parent draft mounted and cancellation returns focus', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createMessageAccessDialogState }, { MessageAccessDialogApp }] = await Promise.all([
    harness.importDashboardModule('js/message-access-dialog-state.js'),
    harness.importDashboardModule('js/message-access-dialog-island.js'),
  ]);
  const state = createMessageAccessDialogState();
  state.openMessage({ from: 'agt_sender' });
  const snapshot = {
    agents: [{ agent_id: 'agt_sender', conv_id: 'conv-s', title: 'sender', online: true }],
    groups: [], permissions: { defaults: [], overrides: {} }, slugs: [], sudo: [],
  };
  const actions = {
    sendMessage: async () => {}, replyHuman: async () => {}, grantSudo: async () => {}, savePermissions: async () => {},
  };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${MessageAccessDialogApp} state=${state} actions=${actions}
    snapshot=${snapshot} confirmDiscard=${async () => true}/>` , host);
  const parent = host.querySelector('#message-create-modal');
  const body = host.querySelector('#message-create-body');
  await harness.input(body, 'draft survives');
  const pickerButton = host.querySelector('#message-create-from-pick');
  pickerButton.focus();
  pickerButton.click();
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#cron-pick-target-modal'));
  assert.equal(host.querySelector('#message-create-modal'), parent, 'opening the keyed child does not recreate its parent');
  assert.equal(host.querySelector('#message-create-body'), body);
  assert.equal(body.value, 'draft survives');

  state.finishPicker('');
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#cron-pick-target-modal'), null);
  assert.equal(host.querySelector('#message-create-body').value, 'draft survives');
  assert.equal(harness.document.activeElement, pickerButton, 'child teardown restores its invoker');
  await mounted.unmount();
});
