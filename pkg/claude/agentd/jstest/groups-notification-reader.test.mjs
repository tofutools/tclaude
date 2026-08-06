import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

test('opening an unread notification does not mark it read', async (t) => {
  const harness = await createPreactHarness(t);
  const { GroupsNotificationReader } = await harness.importDashboardModule(
    'js/groups-notification-reader.js',
  );
  const message = {
    id: 41,
    from_agent: 'agt_sender',
    from_conv: 'conv-sender',
    from_title: 'sender',
    subject: 'needs a decision',
    body: 'Please look at this.',
    read: false,
  };
  const published = [];
  const state = {
    snapshot: { value: { messages: [message], messages_unread: 1 } },
    publish(next) { published.push(next); state.snapshot.value = next; },
  };
  const calls = [];
  const savedFetch = globalThis.fetch;
  globalThis.fetch = async (url, options) => {
    calls.push({ url, body: JSON.parse(options.body) });
    return { ok: true, text: async () => '' };
  };
  t.after(() => { globalThis.fetch = savedFetch; });

  const mounted = await harness.mount(harness.html`
    <${GroupsNotificationReader}
      descriptor=${{
        sender: { agent: 'agt_sender', conv: 'conv-sender', label: 'sender' },
        messageId: 41,
      }}
      snapshot=${state.snapshot.value}
      state=${state}
      actions=${{ reportError() {} }}
      onSelect=${() => {}}
      onClose=${() => {}}
    />
  `);
  await new Promise((resolve) => setTimeout(resolve, 0));

  assert.deepEqual(calls, [], 'rendering the reader must not write read state');
  assert.deepEqual(published, [], 'rendering the reader must not mutate the snapshot');
  assert.equal(state.snapshot.value.messages_unread, 1);
  const chip = mounted.container.querySelector('.human-notification-drawer-state');
  assert.equal(chip.textContent.trim(), 'unread');

  // Clearing the mark is the operator's action, taken through the button.
  const mark = [...mounted.container.querySelectorAll('.human-notification-drawer-actions button')]
    .find((button) => button.textContent === 'Mark read');
  assert.ok(mark, 'quick reader offers the operator a Mark read action');
  await harness.act(() => mark.click());
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(calls.map((call) => call.body), [{ id: 41, read: true }]);
  assert.equal(calls[0].url, '/api/human-messages/read');
  assert.equal(state.snapshot.value.messages_unread, 0);
  await mounted.unmount();
});

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

  // The operator marks read and immediately changes their mind: the second
  // write must queue behind the first rather than race it.
  const marked = persistHumanMessageRead(state, 7, true);
  await Promise.resolve();
  const explicit = persistHumanMessageRead(state, 7, false);
  await Promise.resolve();
  assert.deepEqual(calls, [{ id: 7, read: true }],
    'the second toggle waits behind the in-flight write');
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
  await Promise.all([marked, explicit]);
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

  const marked = persistHumanMessageRead(state, 8, true);
  await Promise.resolve();
  const explicit = persistHumanMessageRead(state, 8, false);
  first.resolve({ ok: false, text: async () => 'read failed' });
  await new Promise((resolve) => setTimeout(resolve, 0));
  second.resolve({ ok: false, text: async () => 'unread failed' });
  await Promise.all([marked, explicit]);

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

// `notify-human --subject … --attach …` sends no body: the published file IS
// the message. The drawer must say so where the body would be, or the operator
// reads a subject over a blank gap and cannot tell it from a broken message.
test('a notification with no body says the attachment is the message', async (t) => {
  const harness = await createPreactHarness(t);
  const { GroupsNotificationReader } = await harness.importDashboardModule(
    'js/groups-notification-reader.js',
  );
  const message = {
    id: 43,
    from_agent: 'agt_sender',
    from_conv: 'conv-sender',
    from_title: 'sender',
    subject: 'dashboard mock',
    body: '',
    read: true,
    attachment: { filename: 'mock.png', content_type: 'image/png', size_bytes: 2048 },
  };
  const state = {
    snapshot: { value: { messages: [message], messages_unread: 0 } },
    publish() {},
  };
  const mounted = await harness.mount(harness.html`
    <${GroupsNotificationReader}
      descriptor=${{
        sender: { agent: 'agt_sender', conv: 'conv-sender', label: 'sender' },
        messageId: 43,
      }}
      snapshot=${state.snapshot.value}
      state=${state}
      actions=${{ reportError() {} }}
      onSelect=${() => {}}
      onClose=${() => {}}
    />
  `);
  const notice = mounted.container.querySelector('.notification-bodiless');
  assert.ok(notice, 'a bodiless notification explains itself instead of rendering nothing');
  assert.match(notice.textContent, /the attached file is the notification/);
  // The subject and the download are still the point of the message.
  assert.match(mounted.container.querySelector('h2').textContent, /dashboard mock/);
  assert.ok(mounted.container.querySelector('.human-notification-drawer-attachment a'));
  await mounted.unmount();
});

// A message that is empty for no good reason must NOT get the explanation —
// there is no attachment to point at, so the notice would be a lie.
test('an empty message with no attachment gets no bodiless notice', async (t) => {
  const harness = await createPreactHarness(t);
  const { GroupsNotificationReader } = await harness.importDashboardModule(
    'js/groups-notification-reader.js',
  );
  const message = {
    id: 44,
    from_agent: 'agt_sender',
    from_conv: 'conv-sender',
    from_title: 'sender',
    subject: 'nothing here',
    body: '',
    read: true,
  };
  const state = {
    snapshot: { value: { messages: [message], messages_unread: 0 } },
    publish() {},
  };
  const mounted = await harness.mount(harness.html`
    <${GroupsNotificationReader}
      descriptor=${{
        sender: { agent: 'agt_sender', conv: 'conv-sender', label: 'sender' },
        messageId: 44,
      }}
      snapshot=${state.snapshot.value}
      state=${state}
      actions=${{ reportError() {} }}
      onSelect=${() => {}}
      onClose=${() => {}}
    />
  `);
  assert.equal(mounted.container.querySelector('.notification-bodiless'), null);
  await mounted.unmount();
});

test('reply button opens the shared reply dialog with notification context', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ GroupsNotificationReader }, dialogController] = await Promise.all([
    harness.importDashboardModule('js/groups-notification-reader.js'),
    harness.importDashboardModule('js/message-access-dialog-controller.js'),
  ]);
  const opened = [];
  const unregister = dialogController.registerMessageAccessDialogController({
    openHumanReply(context) { opened.push(context); },
  });
  t.after(unregister);
  const message = {
    id: 43,
    from_agent: 'agt_sender',
    from_conv: 'conv-sender',
    from_title: 'sender',
    subject: 'review result',
    body: 'The report is ready.',
    read: true,
  };
  const state = {
    snapshot: { value: { messages: [message], messages_unread: 0 } },
    publish() {},
  };
  const closed = [];
  const mounted = await harness.mount(harness.html`
    <${GroupsNotificationReader}
      descriptor=${{
        sender: { agent: 'agt_sender', conv: 'conv-sender', label: 'sender' },
        messageId: 43,
      }}
      snapshot=${state.snapshot.value}
      state=${state}
      actions=${{ reportError() {} }}
      onSelect=${() => {}}
      onClose=${(restoreFocus) => closed.push(restoreFocus)}
    />
  `);

  const reply = [...mounted.container.querySelectorAll('.human-notification-drawer-actions button')]
    .find((button) => button.textContent === 'Reply');
  assert.ok(reply, 'quick reader renders a Reply action');
  await harness.act(() => reply.click());
  assert.deepEqual(opened, [{
    id: 43,
    agent: 'agt_sender',
    conv: 'conv-sender',
    label: 'sender',
    subject: 'review result',
  }]);
  assert.deepEqual(closed, [], 'reply keeps the quick reader open beneath the reply dialog');
});

test('quick reader yields Escape to a stacked reply dialog', async (t) => {
  const harness = await createPreactHarness(t);
  const { GroupsNotificationReader } = await harness.importDashboardModule(
    'js/groups-notification-reader.js',
  );
  const message = {
    id: 44,
    from_agent: 'agt_sender',
    from_conv: 'conv-sender',
    from_title: 'sender',
    subject: 'follow-up',
    body: 'Please reply.',
    read: true,
  };
  const state = {
    snapshot: { value: { messages: [message], messages_unread: 0 } },
    publish() {},
  };
  const closed = [];
  const mounted = await harness.mount(harness.html`
    <${GroupsNotificationReader}
      descriptor=${{
        sender: { agent: 'agt_sender', conv: 'conv-sender', label: 'sender' },
        messageId: 44,
      }}
      snapshot=${state.snapshot.value}
      state=${state}
      actions=${{ reportError() {} }}
      onSelect=${() => {}}
      onClose=${(restoreFocus) => closed.push(restoreFocus)}
    />
  `);

  const overlay = harness.document.createElement('div');
  overlay.className = 'modal-overlay show';
  harness.document.body.append(overlay);
  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assert.deepEqual(closed, [], 'the reader stays open while the reply dialog owns Escape');

  overlay.remove();
  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assert.deepEqual(closed, [true], 'Escape closes the reader when no modal is stacked');
  await mounted.unmount();
});
