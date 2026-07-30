import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

test('human notification read writes preserve the operator’s last action', async (t) => {
  const harness = await createPreactHarness(t);
  const { persistHumanMessageRead } = await harness.importDashboardModule(
    'js/groups-notification-reader.js',
  );
  const snapshot = {
    value: {
      messages_unread: 1,
      messages: [{ id: 7, read: false }],
    },
  };
  const state = {
    snapshot,
    publish(next) { snapshot.value = next; },
  };
  const first = deferred();
  const second = deferred();
  const calls = [];
  const savedFetch = globalThis.fetch;
  globalThis.fetch = async (_url, options) => {
    calls.push(JSON.parse(options.body));
    return calls.length === 1 ? first.promise : second.promise;
  };
  t.after(() => { globalThis.fetch = savedFetch; });

  const opened = persistHumanMessageRead(state, 7, true);
  await Promise.resolve();
  const explicit = persistHumanMessageRead(state, 7, false);
  await Promise.resolve();
  assert.deepEqual(calls, [{ id: 7, read: true }],
    'the explicit toggle waits behind the in-flight automatic read');
  assert.equal(snapshot.value.messages[0].read, false,
    'the explicit action is reflected optimistically');

  first.resolve({ ok: true });
  await first.promise;
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(calls, [
    { id: 7, read: true },
    { id: 7, read: false },
  ]);

  second.resolve({ ok: true });
  await Promise.all([opened, explicit]);
  assert.equal(snapshot.value.messages[0].read, false);
  assert.equal(snapshot.value.messages_unread, 1);
});
