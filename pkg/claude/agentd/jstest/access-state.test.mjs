import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Access state derives partial availability and locally ticks sudo grants', async (t) => {
  const harness = await createPreactHarness(t);
  const { createAccessState } = await harness.importDashboardModule('js/access-state.js');
  const saved = new Map([['tclaude.dash.filter.sudo', 'send']]);
  const prefs = { getItem: (key) => saved.get(key) || null, setItem: (key, value) => saved.set(key, value), removeItem: (key) => saved.delete(key) };
  const now = Date.parse('2026-07-12T00:00:00Z');
  const snapshot = harness.signals.signal({
    generated_at: '2026-07-12T00:00:00Z', agents: [],
    permissions: { defaults: ['agent.send'], overrides: {} }, slugs: [],
    sudo: [{ id: 1, slug: 'agent.send', remaining_seconds: 2 }],
  });
  const state = createAccessState({ snapshot, prefs, now: () => now });
  state.initialize();
  assert.equal(state.view.value.sudoQuery, 'send');
  assert.equal(state.view.value.sudo[0].remaining_seconds, 2);
  state.tick(now + 2000);
  assert.equal(state.view.value.sudo.length, 0, 'expired grants disappear without a new snapshot');
  state.setSubtab('sudo');
  assert.equal(state.view.value.subtab, 'sudo');
  snapshot.value = { permissions: null, slugs: null };
  assert.equal(state.view.value.sudoAvailable, false);
  assert.equal(state.view.value.permissions, null);
});
