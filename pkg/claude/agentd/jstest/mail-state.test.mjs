import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Messages delete routing distinguishes the human and agent stores', async (t) => {
  const harness = await createPreactHarness(t);
  const { messageDeleteEndpoint } = await harness.importDashboardModule('js/mail-state.js');
  assert.equal(messageDeleteEndpoint('human'), '/api/human-messages/delete');
  assert.equal(messageDeleteEndpoint('conv-a'), '/api/mailbox/delete');
  assert.equal(messageDeleteEndpoint('all'), '/api/mailbox/delete');
});

test('Messages attention prioritizes the oldest pending access request, then the oldest unread notification', async (t) => {
  const harness = await createPreactHarness(t);
  const {
    adjacentAttentionPages, nextMessagesAttention, prepareMessagesAttention,
  } = await harness.importDashboardModule('js/mail-state.js');
  const messages = [
    { id: 30, read: false },
    { id: 20, read: true },
    { id: 10, read: false },
  ];
  const accessRequests = [
    { id: 'oldest', status: 'pending' },
    { id: 'handled', status: 'approved' },
    { id: 'newer', status: 'pending' },
  ];

  assert.deepEqual(nextMessagesAttention({
    messages, access_requests: accessRequests,
  }), { kind: 'access', id: 'oldest' });
  assert.deepEqual(nextMessagesAttention({ messages, access_requests: [accessRequests[1]] }), {
    kind: 'notification', id: 10, index: 2,
  });
  assert.equal(nextMessagesAttention({
    messages: messages.map((message) => ({ ...message, read: true })), access_requests: [],
  }), null);

  const jumpState = { messageQuery: 'old filter', selectedMsgs: new Set([10, 30]) };
  prepareMessagesAttention(jumpState);
  assert.equal(jumpState.messageQuery, '');
  assert.deepEqual([...jumpState.selectedMsgs], []);
  assert.deepEqual(adjacentAttentionPages(3, 5), [4, 2]);
  assert.deepEqual(adjacentAttentionPages(1, 5), [2]);
  assert.deepEqual(adjacentAttentionPages(5, 5), [4]);
});

test('Messages state publishes atomic snapshots and does not leak mutable selections', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMailState } = await harness.importDashboardModule('js/mail-state.js');
  const state = createMailState({ selected: 'human', selectedMsgs: new Set([1]), selectedBoxes: new Set(['a']) });
  const first = state.view.value;
  first.selectedMsgs.add(2);
  first.selectedBoxes.clear();
  assert.deepEqual([...state.data.selectedMsgs], [1]);
  assert.deepEqual([...state.data.selectedBoxes], ['a']);
  state.data.selectedMsgs.add(3);
  state.touch();
  assert.deepEqual([...state.view.value.selectedMsgs], [1, 3]);
  assert.notEqual(state.view.value, first);
});

test('Messages request state rejects stale data, retains mailbox roster, and exposes message failures', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMailState } = await harness.importDashboardModule('js/mail-state.js');
  const state = createMailState({ mailboxes: [{ id: 'old' }], messages: [{ id: 1 }], total: 1, totalUnfiltered: 1 });
  const oldToken = state.messageRequest.beginRequest();
  const newToken = state.messageRequest.beginRequest();
  assert.equal(state.messageRequest.commitRequest(oldToken, { messages: [{ id: 2 }] }), false);
  assert.equal(state.messageRequest.commitRequest(newToken, { messages: [{ id: 3 }], total: 1, total_unfiltered: 1 }), true);
  assert.equal(state.view.value.messages[0].id, 3);

  const mailboxToken = state.mailboxRequest.beginRequest();
  state.mailboxRequest.failRequest(mailboxToken, new Error('offline'));
  assert.equal(state.view.value.mailboxes[0].id, 'old');
  assert.equal(state.view.value.mailboxRequest.phase, 'error');

  const messageToken = state.messageRequest.beginRequest();
  state.messageRequest.failRequest(messageToken, new Error('broken'));
  assert.equal(state.view.value.messageRequest.phase, 'error');
  assert.equal(state.view.value.messageRequest.error, 'broken');
});
