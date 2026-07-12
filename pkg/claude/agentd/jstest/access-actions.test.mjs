import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Access revoke preserves endpoint and exposes authorization failures', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createAccessState }, { createAccessActions }] = await Promise.all([
    harness.importDashboardModule('js/access-state.js'), harness.importDashboardModule('js/access-actions.js'),
  ]);
  const state = createAccessState({ snapshot: harness.signals.signal(null), prefs: { getItem: () => null, setItem: () => {}, removeItem: () => {} } });
  const calls = []; const notices = [];
  let fail = false;
  const actions = createAccessActions({ state, confirm: async () => true, openGrant: () => {}, notify: (...args) => notices.push(args),
    requestMutation: async (...args) => { calls.push(args); if (fail) throw Object.assign(new Error('HTTP 403'), { body: { error: 'permission denied' } }); },
  });
  assert.equal(await actions.revoke({ id: 'a/b', slug: 'agent.send', conv_title: 'Alpha' }), true);
  assert.deepEqual(calls[0], ['/api/sudo/a%2Fb', { method: 'DELETE' }]);
  fail = true;
  assert.equal(await actions.revoke({ id: 9, slug: 'agent.kill' }), false);
  assert.match(state.mutation.value.error, /permission denied/);
  assert.equal(state.mutation.value.busy.size, 0);
  assert.equal(notices.at(-1)[1], true);
});
