import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Messages bridge routes ordinary attention clicks but preserves explicit deep links', async (t) => {
  const harness = await createPreactHarness(t);
  const bridge = await harness.importDashboardModule('js/mail-bridge.js');
  const snapshots = [];
  const unregister = bridge.registerMailController({
    focusNextAttention(snapshot) { snapshots.push(snapshot); },
  });

  const first = { messages_unread: 1 };
  bridge.focusNextMessagesAttention(first);
  assert.deepEqual(snapshots, [first]);

  bridge.suppressNextMessagesAttention();
  bridge.focusNextMessagesAttention({ access_requests_pending: 1 });
  assert.deepEqual(snapshots, [first], 'the explicit navigation owns its nav click');

  bridge.focusNextMessagesAttention({ messages_unread: 2 });
  assert.equal(snapshots.length, 2, 'suppression is consumed by only one click');
  unregister();
});
