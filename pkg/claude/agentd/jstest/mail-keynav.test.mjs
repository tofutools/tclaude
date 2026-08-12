import test from 'node:test';
import assert from 'node:assert/strict';
import { assertSameNode } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

function message(id, overrides = {}) {
  return {
    id, direction: 'in', from_conv: 'conv-a', from_agent: 'agt_alpha', from_title: 'Alpha',
    to_conv: 'human', subject: `Subject ${id}`, body: `Body ${id}`,
    created_at: '2026-07-12T00:00:00Z', read: false, ...overrides,
  };
}

function mailbox(id, overrides = {}) {
  return { id, kind: 'agent', title: id, total: 1, unread: 0, in: 1, out: 0, ...overrides };
}

// The sidebar's painted order is pinned folders, then each group with its
// expanded members nested beneath it, then the flat agent list. Arrow keys are
// expected to walk exactly that order, so the fixture spans all three bands.
const SIDEBAR_ORDER = ['all', 'human', 'group:tclaude', 'conv-a', 'conv-b', 'conv-c'];

function accessRequest(id, overrides = {}) {
  return {
    id, agent_id: 'agt_alpha', conv_id: 'conv-a', conv_title: 'Alpha',
    perm: 'groups.members.spawn', created_at: '2026-07-12T00:00:00Z',
    deadline: '2026-07-12T00:05:00Z', ...overrides,
  };
}

function baseState() {
  return {
    boxQuery: '', messageQuery: '', selected: 'human', showRetired: false, showEmpty: false,
    showPrevGens: false, busy: false, progress: null, selectedMsgId: 2,
    selectedMsgs: new Set(), selectedBoxes: new Set(), page: 1, pageSize: 50,
    total: 3, totalUnfiltered: 3,
    mailboxes: [],
    messages: [message(1), message(2), message(3)],
  };
}

// The approvals folder is the one pane whose rows regroup on their own: a
// pending request becomes a handled one on an ordinary background refresh.
function accessState() {
  return {
    ...baseState(),
    selected: 'access-requests', selectedMsgId: 'req-1',
    accessRequests: [accessRequest('req-1'), accessRequest('req-2'),
      accessRequest('req-3', { status: 'declined', decided_at: '2026-07-12T00:01:00Z' })],
  };
}

function controllerFor(signal, calls, statefulSelection = false) {
  const noop = () => {};
  return {
    state: { view: signal },
    setBoxQuery: noop, setMessageQuery: noop, setShowRetired: noop, setShowEmpty: noop,
    setShowPrevGens: noop, toggleBoxSelection: noop, clearBoxSelection: noop,
    wipeSelectedMailboxes: noop, toggleGroupExpand: noop, toggleAgentsExpand: noop,
    toggleMessageSelection: noop, togglePageSelection: noop, deleteOneMessage: noop,
    deleteSelectedMessages: noop, setMessagesRead: noop, markAllAgentRead: noop,
    setPageSize: noop, decideAccess: noop, consumeAccessHighlight: noop, renderMailTab: noop,
    selectMessage: (id) => {
      calls.messages.push(id);
      if (!statefulSelection) return;
      signal.value = {
        ...signal.value,
        selectedMsgId: signal.value.selected === 'access-requests' ? String(id || '') : Number(id),
      };
    },
    selectMailbox: (id) => calls.mailboxes.push(id),
    goToPage: (page) => calls.pages.push(page),
    highlightedAccessRequest: () => null,
    mailboxLabel: (box) => box.title || box.id,
    mailboxTitleAttr: (box) => box.title || box.id,
    mailboxView: () => ({
      empty: false, hasRoster: true, filtering: false, agentsExpanded: true,
      prevGens: new Set(),
      pinned: [mailbox('all', { kind: 'all', title: 'All agent messages' }),
        mailbox('human', { kind: 'human', title: 'Human notifications' })],
      groups: [{
        mailbox: mailbox('group:tclaude', { kind: 'group', title: 'tclaude', members: 2 }),
        expanded: true,
        members: [mailbox('conv-a', { title: 'Alpha' }), mailbox('conv-b', { title: 'Beta' })],
      }],
      agents: [mailbox('conv-c', { title: 'Gamma' })],
    }),
    messageView: () => {
      const requests = signal.value.accessRequests;
      if (!requests) {
        return {
          access: false, messages: signal.value.messages, search: signal.value.messageQuery,
          isAggregate: false, pages: 1,
        };
      }
      const pending = (request) => !request.status || request.status === 'pending';
      return {
        access: true, allAccess: requests, messages: [], isAggregate: false, pages: 1,
        pendingAccess: requests.filter(pending),
        handledAccess: requests.filter((request) => !pending(request)),
      };
    },
    messageCountText: () => `${signal.value.totalUnfiltered} messages`,
    counterparty: (item) => item.from_title,
    senderLabel: (item) => item.from_title,
    allRecipientLabel: (item) => item.to_title || item.to_conv,
    msgPreview: (item) => item.subject,
    msgKind: () => 'decree',
    senderOnline: () => true,
    accessIsPending: (request) => !request.status || request.status === 'pending',
    accessWho: (request) => request.conv_title,
    accessSubject: (request) => request.perm,
    accessStatusText: (request) => request.status || 'pending',
    accessOutcome: (status) => ({ cls: status, txt: status }),
    accessCountdown: () => 'auto-declines in 1m',
  };
}

async function mountMail(t, state, { statefulSelection = false } = {}) {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const signal = harness.signals.signal(state ?? baseState());
  const calls = { messages: [], mailboxes: [], pages: [] };
  const controller = controllerFor(signal, calls, statefulSelection);
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controller} />`);
  const rows = (selector) => [...mounted.container.querySelectorAll(selector)];
  // act() does not forward its callback's value, so the dispatched event —
  // whose defaultPrevented says whether the pane claimed the key — is handed
  // back through the closure.
  const press = async (element, key, init = {}) => {
    let event;
    await harness.act(() => { event = harness.fireEvent(element, 'keydown', { key, ...init }); });
    return event;
  };
  return { harness, signal, calls, mounted, rows, press };
}

test('Arrow keys walk the message list, moving the selection and the focus together', async (t) => {
  const { harness, calls, mounted, rows, press } = await mountMail(t);
  const listRows = rows('#mail-list .mail-row');
  assert.deepEqual(listRows.map((row) => row.dataset.id), ['1', '2', '3']);

  listRows[1].focus();
  const down = await press(listRows[1], 'ArrowDown');
  assert.deepEqual(calls.messages, ['3']);
  assertSameNode(harness.document.activeElement, listRows[2]);
  assert.equal(down.defaultPrevented, true);

  await press(listRows[2], 'ArrowUp');
  assert.deepEqual(calls.messages, ['3', '2']);
  assertSameNode(harness.document.activeElement, listRows[1]);
  await mounted.unmount();
});

test('Arrow keys stop at the ends of the rendered page instead of paging', async (t) => {
  const { harness, calls, mounted, rows, press } = await mountMail(t);
  const listRows = rows('#mail-list .mail-row');

  listRows[0].focus();
  const up = await press(listRows[0], 'ArrowUp');
  listRows[2].focus();
  const down = await press(listRows[2], 'ArrowDown');

  assert.deepEqual(calls.messages, []);
  assert.deepEqual(calls.pages, []);
  // Still consumed: the pane must hold still rather than scroll off the row
  // the operator is reading.
  assert.equal(up.defaultPrevented, true);
  assert.equal(down.defaultPrevented, true);
  assertSameNode(harness.document.activeElement, listRows[2]);
  await mounted.unmount();
});

test('Home, End, PageUp, and PageDown move within the rendered page', async (t) => {
  const messages = Array.from({ length: 12 }, (_, index) => message(index + 1));
  const state = {
    ...baseState(), messages, selectedMsgId: 6, total: messages.length,
    totalUnfiltered: messages.length,
  };
  const { harness, calls, mounted, rows, press } = await mountMail(t, state);
  const list = mounted.container.querySelector('#mail-list');
  const listRows = rows('#mail-list .mail-row');
  Object.defineProperty(list, 'clientHeight', { configurable: true, value: 65 });
  Object.defineProperty(listRows[0], 'offsetHeight', { configurable: true, value: 20 });

  listRows[5].focus();
  await press(listRows[5], 'PageDown');
  assert.equal(calls.messages.at(-1), '9', 'PageDown moves one three-row viewport');
  assertSameNode(harness.document.activeElement, listRows[8]);
  await press(listRows[8], 'PageUp');
  assert.equal(calls.messages.at(-1), '6');

  await press(listRows[5], 'Home');
  assert.equal(calls.messages.at(-1), '1');
  assertSameNode(harness.document.activeElement, listRows[0]);
  await press(listRows[0], 'End');
  assert.equal(calls.messages.at(-1), '12');
  assertSameNode(harness.document.activeElement, listRows[11]);

  await press(listRows[11], 'PageDown');
  assert.equal(calls.messages.at(-1), '12', 'PageDown clamps at the rendered last row');
  assert.deepEqual(calls.pages, [], 'row navigation never turns a server page');
  await mounted.unmount();
});

test('A move started from a row control continues from that row', async (t) => {
  const { harness, calls, mounted, rows, press } = await mountMail(t);
  const listRows = rows('#mail-list .mail-row');
  const checkbox = rows('#mail-list .mail-msg-check')[1];

  checkbox.focus();
  await press(checkbox, 'ArrowDown');

  assert.deepEqual(calls.messages, ['3']);
  assertSameNode(harness.document.activeElement, listRows[2]);
  await mounted.unmount();
});

test('Down enters each filter result list, while Up or Escape returns from its first row', async (t) => {
  const { harness, calls, mounted, rows, press } = await mountMail(t);
  const boxFilter = mounted.container.querySelector('#filter-mailboxes');
  const messageFilter = mounted.container.querySelector('#filter-messages');
  const boxes = rows('#mail-sidebar .mailbox');
  const listRows = rows('#mail-list .mail-row');

  boxFilter.focus();
  const intoBoxes = await press(boxFilter, 'ArrowDown');
  assert.equal(intoBoxes.defaultPrevented, true);
  assertSameNode(harness.document.activeElement, boxes[0]);
  assert.deepEqual(calls.mailboxes, ['all']);
  const outOfBoxes = await press(boxes[0], 'ArrowUp');
  assert.equal(outOfBoxes.defaultPrevented, true);
  assertSameNode(harness.document.activeElement, boxFilter);

  messageFilter.focus();
  await press(messageFilter, 'ArrowDown');
  assertSameNode(harness.document.activeElement, listRows[0]);
  assert.deepEqual(calls.messages, ['1']);
  await press(listRows[0], 'Escape');
  assertSameNode(harness.document.activeElement, messageFilter);

  listRows[1].focus();
  const deeperEscape = await press(listRows[1], 'Escape');
  assert.equal(deeperEscape.defaultPrevented, false, 'Escape only returns from the first row');
  assertSameNode(harness.document.activeElement, listRows[1]);
  await mounted.unmount();
});

test('Down in a filter stays native when its result pane has no rows', async (t) => {
  const state = {
    ...baseState(), messages: [], selectedMsgId: null, total: 0, totalUnfiltered: 0,
  };
  const { harness, calls, mounted, press } = await mountMail(t, state);
  const filter = mounted.container.querySelector('#filter-messages');
  filter.focus();

  const down = await press(filter, 'ArrowDown');
  assert.equal(down.defaultPrevented, false);
  assertSameNode(harness.document.activeElement, filter);
  assert.deepEqual(calls.messages, []);
  await mounted.unmount();
});

test('Down in a composing filter stays with the IME candidate list', async (t) => {
  const { harness, calls, mounted, press } = await mountMail(t);
  const filter = mounted.container.querySelector('#filter-messages');
  filter.focus();

  const down = await press(filter, 'ArrowDown', { isComposing: true });
  assert.equal(down.defaultPrevented, false);
  assertSameNode(harness.document.activeElement, filter);
  assert.deepEqual(calls.messages, []);
  await mounted.unmount();
});

test('Left and Right move focus across sidebar, list, and reader', async (t) => {
  const { harness, calls, mounted, rows, press } = await mountMail(t);
  const boxes = rows('#mail-sidebar .mailbox');
  const listRows = rows('#mail-list .mail-row');
  const reader = mounted.container.querySelector('#mail-reader');

  assert.equal(boxes[1].getAttribute('aria-current'), 'true');
  assert.equal(listRows[1].getAttribute('aria-current'), 'true');
  assert.equal(reader.getAttribute('role'), 'region');
  assert.equal(reader.getAttribute('aria-label'), 'Message reader');
  assert.equal(reader.getAttribute('tabindex'), '0');

  boxes[1].focus();
  await press(boxes[1], 'ArrowRight');
  assertSameNode(harness.document.activeElement, listRows[1]);
  await press(listRows[1], 'ArrowRight');
  assertSameNode(harness.document.activeElement, reader);

  const readerDown = await press(reader, 'ArrowDown');
  assert.equal(readerDown.defaultPrevented, false, 'the focused reader keeps native scrolling');
  assertSameNode(harness.document.activeElement, reader);

  await press(reader, 'ArrowLeft');
  assertSameNode(harness.document.activeElement, listRows[1]);
  await press(listRows[1], 'ArrowLeft');
  assertSameNode(harness.document.activeElement, boxes[1]);
  assert.deepEqual(calls.mailboxes, []);
  assert.deepEqual(calls.messages, [], 'pane switches preserve the current selection');

  const modified = await press(boxes[1], 'ArrowRight', { ctrlKey: true });
  assert.equal(modified.defaultPrevented, false);
  assertSameNode(harness.document.activeElement, boxes[1]);
  await mounted.unmount();
});

test('Entering an unselected message list opens the focused fallback row', async (t) => {
  const state = { ...baseState(), selectedMsgId: null };
  const { harness, signal, calls, mounted, rows, press } = await mountMail(
    t, state, { statefulSelection: true },
  );
  const boxes = rows('#mail-sidebar .mailbox');
  const listRows = rows('#mail-list .mail-row');
  const reader = mounted.container.querySelector('#mail-reader');

  boxes[1].focus();
  await press(boxes[1], 'ArrowRight');
  assertSameNode(harness.document.activeElement, listRows[0]);
  assert.equal(signal.value.selectedMsgId, 1);
  assert.equal(mounted.container.querySelector('.mail-subject').textContent, 'Subject 1 #1');

  await harness.act(() => {
    signal.value = { ...signal.value, selectedMsgId: null };
  });
  reader.focus();
  await press(reader, 'ArrowLeft');
  assertSameNode(harness.document.activeElement, listRows[0]);
  assert.equal(signal.value.selectedMsgId, 1);
  assert.equal(mounted.container.querySelector('.mail-subject').textContent, 'Subject 1 #1');
  assert.deepEqual(calls.messages, ['1', '1']);
  await mounted.unmount();
});

test('With no focused row the move starts from the selected one', async (t) => {
  const { calls, mounted, press } = await mountMail(t);
  const list = mounted.container.querySelector('#mail-list');
  assert.equal(list.querySelector('.mail-row.active').dataset.id, '2');

  await press(list, 'ArrowDown');

  assert.deepEqual(calls.messages, ['3']);
  await mounted.unmount();
});

test('Modified navigation keys stay with the browser', async (t) => {
  const { calls, mounted, rows, press } = await mountMail(t);
  const listRows = rows('#mail-list .mail-row');
  listRows[1].focus();

  for (const modifier of ['altKey', 'ctrlKey', 'metaKey', 'shiftKey']) {
    const event = await press(listRows[1], 'ArrowDown', { [modifier]: true });
    assert.equal(event.defaultPrevented, false, `${modifier} must not be swallowed`);
  }
  for (const key of ['Home', 'End', 'PageUp', 'PageDown']) {
    const event = await press(listRows[1], key, { ctrlKey: true });
    assert.equal(event.defaultPrevented, false, `modified ${key} must not be swallowed`);
  }
  assert.deepEqual(calls.messages, []);
  await mounted.unmount();
});

test('A running bulk op takes the arrow keys with the folders it disables', async (t) => {
  const { calls, mounted, rows, press } = await mountMail(t, { ...baseState(), busy: true });
  const sidebarRows = rows('#mail-sidebar .mailbox');
  const listRows = rows('#mail-list .mail-row');

  // Folder buttons are disabled for the duration, and selectMailbox refuses to
  // switch anyway — so the sidebar's arrows go quiet with them.
  const sidebarEvent = await press(sidebarRows[1], 'ArrowDown');
  assert.deepEqual(calls.mailboxes, []);
  assert.equal(sidebarEvent.defaultPrevented, false);

  // Message rows stay clickable through a bulk op (opening one only reads it),
  // so the list keeps its arrows too.
  await press(listRows[1], 'ArrowDown');
  assert.deepEqual(calls.messages, ['3']);
  await mounted.unmount();
});

test('Arrow keys walk the sidebar in painted order, nested group members included', async (t) => {
  const { harness, calls, mounted, rows, press } = await mountMail(t);
  const boxes = rows('#mail-sidebar .mailbox');
  assert.deepEqual(boxes.map((row) => row.dataset.id), SIDEBAR_ORDER);

  // No row holds focus yet, so the move starts from the open folder ("human").
  const sidebar = mounted.container.querySelector('#mail-sidebar');
  await press(sidebar, 'ArrowDown');
  assert.deepEqual(calls.mailboxes, ['group:tclaude']);
  assertSameNode(harness.document.activeElement, boxes[2]);

  // …and crosses into the group's nested members and out again to the flat
  // agent list without stopping on the section heading or the expand caret.
  for (const expected of ['conv-a', 'conv-b', 'conv-c']) {
    await press(harness.document.activeElement, 'ArrowDown');
    assert.equal(calls.mailboxes.at(-1), expected);
  }
  assertSameNode(harness.document.activeElement, boxes[5]);
  await mounted.unmount();
});

test('Clicking a row focuses it, so the arrow keys pick up from there', async (t) => {
  const { harness, mounted, rows } = await mountMail(t);
  const listRows = rows('#mail-list .mail-row');
  const boxes = rows('#mail-sidebar .mailbox');

  // Browsers disagree about whether a click focuses a <button> (macOS does
  // not), so the island focuses it explicitly — otherwise "click a message,
  // then arrow" would be a Linux-only feature.
  await harness.act(() => harness.fireEvent(listRows[2], 'click'));
  assertSameNode(harness.document.activeElement, listRows[2]);
  await harness.act(() => harness.fireEvent(boxes[3], 'click'));
  assertSameNode(harness.document.activeElement, boxes[3]);
  await mounted.unmount();
});

test('A request decided in the background keeps the focused row, and the arrows with it', async (t) => {
  const { harness, signal, calls, mounted, rows, press } = await mountMail(t, accessState());
  const rowFor = (id) => rows('#mail-list .mail-row').find((row) => row.dataset.id === id);
  const focused = rowFor('req-1');
  focused.focus();

  // A decision taken elsewhere — the tray, another browser, or the auto-decline
  // running out — arrives on an ordinary refresh and moves req-1 from the
  // pending group to the handled one, below the divider.
  await harness.act(() => {
    signal.value = {
      ...signal.value,
      accessRequests: signal.value.accessRequests.map((request) => (request.id === 'req-1'
        ? { ...request, status: 'approved', decided_at: '2026-07-12T00:02:00Z' }
        : request)),
    };
  });
  assert.deepEqual(rows('#mail-list .mail-row').map((row) => row.dataset.id),
    ['req-2', 'req-1', 'req-3']);

  // Regrouping must reorder the row, not remount it: a remount drops focus to
  // the document body, and the pane never sees another arrow key.
  // assert.ok, not assert.equal: a failing DOM-node comparison spends ~20s
  // rendering two element diffs nobody reads.
  assert.ok(rowFor('req-1') === focused, 'the decided row must keep its DOM node');
  assertSameNode(harness.document.activeElement, focused);

  await press(harness.document.activeElement, 'ArrowDown');
  assert.deepEqual(calls.messages, ['req-3']);
  await mounted.unmount();
});
