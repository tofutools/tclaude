import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function prefs() {
  const values = new Map();
  return {
    values,
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
  };
}

test('Plugins state filters, tracks busy work, and edits modal drafts immutably', async (t) => {
  const harness = await createPreactHarness(t);
  const { createPluginsState, pluginBusyKey } = await harness.importDashboardModule('js/plugins-state.js');
  const snapshot = harness.signals.signal({
    plugins: [{ name: 'canvas', descr: 'drawing', steps: [{ name: 'server', run: 'docker run canvas' }] }],
    plugins_catalog: [{ name: 'github', descr: 'source control', steps: [] }],
    plugins_warn: 1, plugins_tab_visible: true,
  });
  const poll = harness.signals.signal({ phase: 'ready', requestId: 1, error: null });
  const storage = prefs();
  const state = createPluginsState({ snapshot, poll, prefs: storage });
  state.initialize();

  state.setQuery('docker');
  assert.equal(state.view.value.installed[0].name, 'canvas');
  assert.equal(state.view.value.catalog.length, 0);
  assert.equal(storage.values.get('tclaude.dash.filter.plugins'), 'docker');
  state.setQuery('github');
  assert.equal(state.view.value.installed.length, 0);
  assert.equal(state.view.value.catalog[0].name, 'github');

  const key = pluginBusyKey('plugin-check', 'canvas');
  assert.equal(state.beginBusy(key), true);
  assert.equal(state.beginBusy(key), false);
  assert.ok(state.view.value.busy.has(key));
  assert.equal(state.endBusy(key), true);

  state.openModal(snapshot.value.plugins[0]);
  const originalStep = state.modal.value.steps[0];
  state.updateStep(0, { run: 'docker compose up' });
  assert.notEqual(state.modal.value.steps[0], originalStep);
  assert.equal(state.modal.value.steps[0].run, 'docker compose up');
  state.addStep();
  assert.equal(state.modal.value.steps.length, 2);
  state.removeStep(0);
  assert.equal(state.modal.value.steps.length, 1);
});
