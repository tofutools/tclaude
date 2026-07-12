import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Config state makes lifecycle, dirty, errors, and tab activity explicit', async (t) => {
  const harness = await createPreactHarness(t);
  const { createConfigState } = await harness.importDashboardModule('js/config-state.js');
  const activeTab = harness.signals.signal('groups');
  const state = createConfigState({ activeTab });
  assert.deepEqual(state.view.value, { active: false, phase: 'idle', dirty: false, error: null, metadata: null });
  state.lifecycle.loading();
  assert.equal(state.view.value.phase, 'loading');
  state.lifecycle.loaded({ path: '/tmp/config.json' });
  state.markDirty();
  assert.equal(state.view.value.dirty, true);
  state.lifecycle.saving();
  assert.equal(state.view.value.phase, 'saving');
  state.lifecycle.failed(new Error('server unavailable'));
  activeTab.value = 'config';
  assert.equal(state.view.value.active, true);
  assert.equal(state.view.value.dirty, true);
  assert.equal(state.view.value.error, 'server unavailable');
  state.lifecycle.saved({ path: '/tmp/config.json' });
  assert.equal(state.view.value.dirty, false);
  assert.equal(state.view.value.error, null);
});
