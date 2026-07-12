import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function message(id, overrides = {}) {
  return {
    id, direction: 'in', from_conv: 'conv-a', from_agent: 'agt_alpha', from_title: 'Alpha',
    to_conv: 'human', subject: `Subject ${id}`, body: `Body ${id}\nhttps://example.com/report.`,
    created_at: '2026-07-12T00:00:00Z', read: false, ...overrides,
  };
}

function controllerFor(signal) {
  const noop = () => {};
  return {
    state: { view: signal },
    setBoxQuery: noop, setMessageQuery: noop, setShowRetired: noop, setShowEmpty: noop,
    setShowPrevGens: noop, toggleBoxSelection: noop, clearBoxSelection: noop,
    wipeSelectedMailboxes: noop, selectMailbox: noop, toggleGroupExpand: noop,
    toggleAgentsExpand: noop, selectMessage: noop, toggleMessageSelection: noop,
    togglePageSelection: noop, deleteOneMessage: noop, deleteSelectedMessages: noop,
    setMessagesRead: noop, markAllAgentRead: noop, goToPage: noop, setPageSize: noop,
    decideAccess: noop, consumeAccessHighlight: noop,
    highlightedAccessRequest: () => null,
    mailboxLabel: (mailbox) => mailbox.title || (mailbox.kind === 'human' ? 'Human notifications' : mailbox.id),
    mailboxTitleAttr: (mailbox) => mailbox.title || mailbox.id,
    mailboxView: () => ({ empty: false, hasRoster: true, pinned: signal.value.mailboxes,
      groups: [], agents: [], prevGens: new Set(), filtering: false, agentsExpanded: false }),
    messageView: () => ({ access: false, messages: signal.value.messages, search: signal.value.messageQuery,
      isAggregate: false, pages: 1 }),
    messageCountText: () => `${signal.value.totalUnfiltered} messages`,
    counterparty: (item) => item.from_title,
    allSenderLabel: (item) => item.from_title,
    allRecipientLabel: (item) => item.to_title || item.to_conv,
    msgPreview: (item) => item.subject || item.body,
    msgKind: (item) => item.parent_id ? 'reply' : 'decree',
    senderOnline: () => true,
    accessIsPending: (request) => !request.status || request.status === 'pending',
    accessWho: (request) => request.conv_title,
    accessSubject: (request) => request.perm,
    accessStatusText: (request) => request.status || 'pending',
    accessOutcome: (status) => ({ cls: status, txt: status }),
    accessCountdown: () => 'auto-declines in 1m',
  };
}

function populated() {
  return {
    boxQuery: '', messageQuery: '', selected: 'human', showRetired: false, showEmpty: false,
    showPrevGens: false, busy: false, progress: null, selectedMsgId: 1,
    selectedMsgs: new Set(), selectedBoxes: new Set(), page: 1, pageSize: 50,
    total: 1, totalUnfiltered: 1,
    mailboxes: [{ id: 'human', kind: 'human', title: 'Human notifications', total: 1, unread: 1, in: 1, out: 0 }],
    messages: [message(1, { attachment: {
      filename: 'report.md', content_type: 'text/markdown', size_bytes: 1536,
    } })],
  };
}

test('Messages island preserves native controls, CSS hooks, focus, reader, and keyed rows across incoming updates', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const state = harness.signals.signal(populated());
  const controller = controllerFor(state);
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controller} />`);

  const mailboxFilter = mounted.container.querySelector('#filter-mailboxes');
  const messageFilter = mounted.container.querySelector('#filter-messages');
  assert.equal(mailboxFilter.getAttribute('type'), 'text');
  assert.equal(mailboxFilter.getAttribute('placeholder'), 'Filter mailboxes (name / id)');
  assert.equal(messageFilter.getAttribute('placeholder'), 'Filter messages (sender / recipient / subject / body)');
  assert.equal(mounted.container.querySelector('#mail-show-retired').hasAttribute('checked'), false);
  assert.equal(mounted.container.querySelector('#mail-reader').dataset.kind, 'decree');
  assert.equal(mounted.container.querySelector('.mail-row-wrap').dataset.kind, 'decree');
  assert.equal(mounted.container.querySelector('.mail-reader-body a').getAttribute('href'), 'https://example.com/report');
  assert.equal(mounted.container.querySelector('.mail-attachment a').getAttribute('download'), 'report.md');
  assert.equal(mounted.container.querySelector('.mail-attachment-size').textContent, '1.5 KiB');

  const originalRow = mounted.container.querySelector('.mail-row-wrap');
  const originalReader = mounted.container.querySelector('#mail-reader');
  originalReader.scrollTop = 37;
  messageFilter.focus();
  await harness.act(() => { state.value = { ...state.value, total: 2, totalUnfiltered: 2,
    messages: [message(2), ...state.value.messages] }; });
  assert.equal(mounted.container.querySelectorAll('.mail-row-wrap')[1], originalRow);
  assert.equal(mounted.container.querySelector('#mail-reader'), originalReader);
  assert.equal(originalReader.scrollTop, 37);
  assert.equal(harness.document.activeElement, messageFilter);

  harness.document.body.classList.add('wizard');
  await harness.act(() => { state.value = { ...state.value }; });
  assert.equal(mailboxFilter.getAttribute('placeholder'), 'Seek a familiar…');
  assert.equal(messageFilter.getAttribute('placeholder'), 'Search the scrolls…');
  await mounted.unmount();
});

test('Messages access request view retains decree hooks and decision controls', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const request = { id: 'req-1', conv_id: 'conv-a', agent_id: 'agt_alpha', conv_title: 'Alpha',
    perm: 'human.clipboard', created_at: '2026-07-12T00:00:00Z', deadline: '2026-07-12T00:05:00Z', auto_grantable: true };
  const value = { ...populated(), selected: 'access-requests', selectedMsgId: 'req-1', messages: [] };
  const state = harness.signals.signal(value);
  const controller = controllerFor(state);
  controller.messageView = () => ({ access: true, allAccess: [request], pendingAccess: [request], handledAccess: [], search: '' });
  controller.messageCountText = () => '1 pending';
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controller} />`);
  assert.equal(mounted.container.querySelector('.access-row-wrap').dataset.kind, 'decree');
  assert.equal(mounted.container.querySelector('#mail-reader').dataset.kind, 'decree');
  assert.match(mounted.container.textContent, /Always allow/);
  assert.match(mounted.container.textContent, /Approve/);
  await mounted.unmount();
});

test('Messages interactions freeze mailbox changes while busy, wire agent mark-all, and restore filter focus', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const state = harness.signals.signal({ ...populated(), selected: 'conv-a', messageQuery: 'old query',
    mailboxes: [{ id: 'conv-a', kind: 'agent', title: 'Alpha', total: 1, unread: 1, in: 1, out: 0 }] });
  const controller = controllerFor(state);
  let markAllCalls = 0;
  let clearedTo = null;
  controller.markAllAgentRead = () => { markAllCalls += 1; };
  controller.setMessageQuery = (value) => {
    clearedTo = value;
    state.value = { ...state.value, messageQuery: value };
  };
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controller} />`);

  mounted.container.querySelector('#mail-agent-mark-all').click();
  await harness.act(() => Promise.resolve());
  assert.equal(markAllCalls, 1);

  mounted.container.querySelector('#filter-messages-clear').click();
  await harness.act(() => Promise.resolve());
  assert.equal(clearedTo, '');
  assert.equal(harness.document.activeElement, mounted.container.querySelector('#filter-messages'));

  await harness.act(() => { state.value = { ...state.value, busy: true }; });
  const mailbox = mounted.container.querySelector('.mailbox');
  assert.equal(mailbox.hasAttribute('disabled'), true);
  await mounted.unmount();
});

test('Messages production mount registers cleanup for listeners, bridge ownership, and Preact DOM', async (t) => {
  const harness = await createPreactHarness(t);
  const { mountMailIsland } = await harness.importDashboardModule('js/mail-island.js');
  const state = harness.signals.signal(populated());
  const controller = controllerFor(state);
  let disposed = 0;
  controller.initMail = () => () => { disposed += 1; };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const cleanups = [];
  mountMailIsland({ host, controller, registerCleanup: (cleanup) => cleanups.push(cleanup) });
  assert.ok(host.querySelector('.mail-client'));
  assert.equal(cleanups.length, 3);
  for (const cleanup of cleanups.reverse()) cleanup();
  assert.equal(disposed, 1);
  assert.equal(host.childElementCount, 0);
});
