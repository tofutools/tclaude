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

test('Messages delivery state distinguishes offline notification discard', async (t) => {
  const harness = await createPreactHarness(t);
  const { messageDeliveryState } = await harness.importDashboardModule('js/mail-parts.js');
  assert.equal(messageDeliveryState({ nudge_discarded_at: '2026-07-16T12:00:00Z', delivered_at: '2026-07-16T12:00:00Z' }), 'discarded while offline');
  assert.equal(messageDeliveryState({ delivered_at: '2026-07-16T12:00:00Z' }), 'delivered');
  assert.equal(messageDeliveryState({ direction: 'out' }), 'undelivered');
  assert.equal(messageDeliveryState({ forward_state: 'queued', forwarded_to: 'b', delivered_at: '2026-07-16T12:00:00Z' }), 'forwarded to b');
  assert.equal(messageDeliveryState({ forward_state: 'accepted', forwarded_to: 'b' }), 'delivered');
  assert.equal(messageDeliveryState({ forward_state: 'refused', forwarded_to: 'b' }), 'forwarding refused');
});

test('Messages attention prioritizes the oldest pending access request, then the oldest unread notification', async (t) => {
  const harness = await createPreactHarness(t);
  const {
    nextMessagesAttention, prepareMessagesAttention,
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

test('a peer page names a peer\'s human-messages routes through its proxy', async (t) => {
  const harness = await createPreactHarness(t);
  const { pageHumanRoute } = await harness.importDashboardModule('js/mail-state.js');
  assert.equal(pageHumanRoute('/api/human-messages/delete', ''), '/api/human-messages/delete', 'this node\'s page: its own');
  assert.equal(pageHumanRoute('/api/human-messages/clear', 'inst_forge'), '/api/peer/inst_forge/human-messages/clear');
  assert.equal(pageHumanRoute('/api/mailbox/delete', 'inst_forge'), '/api/mailbox/delete', 'the page\'s fetch already sends the rest there');
});

test('a decision notification offers its options and posts the pick through the reply route', async (t) => {
  const harness = await createPreactHarness(t);
  const { DecisionChoices } = await harness.importDashboardModule('js/mail-parts.js');
  const requests = [];
  const toasts = [];
  let done = 0;
  const fetchImpl = async (url, init) => { requests.push({ url, body: JSON.parse(init.body) }); return { ok: true, status: 200, json: async () => ({}) }; };
  const message = { id: 4, from_agent: 'agt_x', from_title: 'designer', options: ['Tabs', 'Sidebar'], recommended: 1 };
  const view = await harness.mount(harness.html`<${DecisionChoices} message=${message} fetchImpl=${fetchImpl}
    toast=${(text, bad) => toasts.push([text, bad])} onDone=${() => { done += 1; }} />`);
  assert.ok(harness.getByRole(view.container, 'button', { name: 'Tabs ★' }).classList.contains('primary'));
  await harness.act(async () => { harness.getByRole(view.container, 'button', { name: 'Sidebar' }).click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(requests, [{ url: '/api/human-messages/reply', body: { id: 4, choice: 2 } }]);
  assert.deepEqual(toasts, [['Sent “Sidebar” to designer', false]]);
  assert.equal(done, 1);

  const decided = await harness.mount(harness.html`<${DecisionChoices} message=${{ ...message, decision: 'Tabs' }} />`);
  assert.match(decided.container.textContent, /Decided:\s*Tabs/);
  assert.equal(decided.container.querySelectorAll('button').length, 0);
  const plain = await harness.mount(harness.html`<${DecisionChoices} message=${{ id: 5 }} />`);
  assert.equal(plain.container.textContent, '');
});
