import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('turning Collab off takes the address bar off the Steward panel', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ applyCollabVisibility }, { configPanel, setConfigPanel }] = await Promise.all([
    harness.importDashboardModule('js/processes.js'),
    harness.importDashboardModule('js/config-panel.js'),
  ]);
  const section = harness.document.body.appendChild(harness.document.createElement('section'));
  section.id = 'tab-config';
  section.classList.add('active');
  t.after(() => section.remove());
  const corrections = [];
  harness.document.addEventListener('tclaude:navigated', (event) => corrections.push(event.detail));

  setConfigPanel('steward');
  applyCollabVisibility({ collab_enabled: true });
  assert.deepEqual(corrections, [], 'Collab on keeps the panel');

  applyCollabVisibility({ collab_enabled: false });
  assert.equal(configPanel.value, 'appearance');
  assert.deepEqual(corrections, [{ location: { tab: 'config', subtab: 'appearance' }, correction: true }]);
});
