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

test('failed queued writes restore the last server-confirmed state', async (t) => {
  const harness = await createPreactHarness(t);
  const { persistHumanMessageRead } = await harness.importDashboardModule(
    'js/groups-notification-reader.js',
  );
  const snapshot = {
    value: {
      messages_unread: 1,
      messages: [{ id: 8, read: false }],
    },
  };
  const state = {
    snapshot,
    publish(next) { snapshot.value = next; },
  };
  const first = deferred();
  const second = deferred();
  let calls = 0;
  const savedFetch = globalThis.fetch;
  globalThis.fetch = async () => (++calls === 1 ? first.promise : second.promise);
  t.after(() => { globalThis.fetch = savedFetch; });

  const opened = persistHumanMessageRead(state, 8, true);
  await Promise.resolve();
  const explicit = persistHumanMessageRead(state, 8, false);
  first.resolve({ ok: false, text: async () => 'read failed' });
  await new Promise((resolve) => setTimeout(resolve, 0));
  second.resolve({ ok: false, text: async () => 'unread failed' });
  await Promise.all([opened, explicit]);

  assert.equal(snapshot.value.messages[0].read, false,
    'both failures restore the original server-confirmed unread state');
  assert.equal(snapshot.value.messages_unread, 1);
});

test('attachment card and explicit button download the same published file', async (t) => {
  const harness = await createPreactHarness(t);
  const { GroupsNotificationReader } = await harness.importDashboardModule(
    'js/groups-notification-reader.js',
  );
  const message = {
    id: 42,
    from_agent: 'agt_sender',
    from_conv: 'conv-sender',
    from_title: 'sender',
    subject: 'artifact',
    body: 'Download it',
    read: true,
    attachment: {
      filename: 'report.png',
      content_type: 'image/png',
      size_bytes: 2048,
    },
  };
  const state = {
    snapshot: { value: { messages: [message], messages_unread: 0 } },
    publish() {},
  };
  const mounted = await harness.mount(harness.html`
    <${GroupsNotificationReader}
      descriptor=${{
        sender: { agent: 'agt_sender', conv: 'conv-sender', label: 'sender' },
        messageId: 42,
      }}
      snapshot=${state.snapshot.value}
      state=${state}
      actions=${{ reportError() {} }}
      onSelect=${() => {}}
      onClose=${() => {}}
    />
  `);
  const links = [...mounted.container.querySelectorAll(
    '.human-notification-drawer-attachment a',
  )];
  assert.equal(links.length, 2);
  for (const link of links) {
    assert.equal(link.getAttribute('href'), '/api/human-messages/42/attachment');
    assert.equal(link.getAttribute('download'), 'report.png');
  }
  assert.match(links[0].textContent, /report\.png/);
  assert.match(links[1].textContent, /Download/);
});
