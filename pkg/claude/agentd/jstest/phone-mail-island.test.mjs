// js/phone-mail-island.js: Mail on a phone, driven through what the operator
// taps, over a stub of the mail controller it shares with the desktop panes.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function message(id, overrides = {}) {
  return {
    id, direction: 'in', from_conv: 'conv-a', from_agent: 'agt_alpha', from_title: 'Alpha',
    to_conv: 'human', subject: `Subject ${id}`, body: `Subject ${id}\nFirst line ${id}\nSecond`,
    created_at: '2026-07-12T00:00:00Z', read: false, ...overrides,
  };
}

const MAILBOXES = [
  { id: 'access-requests', kind: 'access-requests', title: 'Access requests', unread: 2 },
  { id: 'human', kind: 'human', title: 'Human notifications', unread: 1 },
  { id: 'all', kind: 'all', title: 'All agent mail', unread: 0 },
];

function controllerFor(view, calls) {
  const record = (name) => (...args) => { calls.push([name, ...args]); };
  const agents = [{ id: 'agt_beta', kind: 'agent', title: 'Beta', unread: 4 }];
  return {
    state: { view },
    setBoxQuery: record('setBoxQuery'), setMessageQuery: record('setMessageQuery'),
    setShowRetired: record('setShowRetired'), setShowEmpty: record('setShowEmpty'), setShowPrevGens: record('setShowPrevGens'),
    selectMailbox: record('selectMailbox'), selectMessage: record('selectMessage'), closeMessage: record('closeMessage'),
    listAgents: record('listAgents'), goToPage: record('goToPage'), decideAccess: record('decideAccess'),
    renderMailTab: record('renderMailTab'), reloadMail: () => {}, reloadMessagesPage: () => {}, loadMoreAgents: () => {},
    toggleMessageSelection: () => {}, togglePageSelection: () => {}, deleteOneMessage: () => {},
    deleteSelectedMessages: () => {}, setMessagesRead: () => {}, markAllAgentRead: () => {}, setPageSize: () => {},
    consumeAccessHighlight: () => {}, highlightedAccessRequest: () => null,
    mailboxLabel: (mailbox) => mailbox.title || mailbox.id,
    mailboxTitleAttr: (mailbox) => mailbox.title || mailbox.id,
    // Like mail.js, a set agent filter drops the pinned and group rows unless
    // the caller asks for them all.
    mailboxView: ({ unfiltered = false } = {}) => ({ empty: false, hasRoster: true,
      pinned: view.value.boxQuery && !unfiltered ? [] : MAILBOXES,
      groups: view.value.boxQuery && !unfiltered ? []
        : [{ mailbox: { id: 'group:webshop', kind: 'group', title: 'webshop', unread: 3 }, members: [] }],
      agents, prevGens: new Set(), filtering: false, agentsExpanded: false }),
    messageView: () => ({ access: false, messages: view.value.messages, search: view.value.messageQuery,
      isAggregate: false, pages: 2 }),
    messageCountText: () => `${view.value.messages.length} messages`,
    counterparty: (item) => item.from_title,
    allSenderLabel: (item) => item.from_title,
    allRecipientLabel: (item) => item.to_title || item.to_conv,
    msgPreview: (item) => item.subject || item.body,
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

function viewState(overrides = {}) {
  return {
    boxQuery: '', messageQuery: '', selected: 'human', showRetired: false, showEmpty: false,
    showPrevGens: false, busy: false, progress: null, selectedMsgId: null,
    selectedMsgs: new Set(), selectedBoxes: new Set(), page: 1, pageSize: 50, total: 2, totalUnfiltered: 2,
    mailboxes: MAILBOXES,
    messages: [
      message(1, { attachment: { filename: 'shot.png', content_type: 'image/png', size_bytes: 10 } }),
      message(2, { read: true, options: ['Ship', 'Wait'] }),
    ],
    ...overrides,
  };
}

async function setup(t, overrides) {
  const harness = await createPreactHarness(t);
  const [island, { phonePage }, { dashboardState }] = await Promise.all([
    harness.importDashboardModule('js/phone-mail-island.js'),
    harness.importDashboardModule('js/phone-page.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
  ]);
  dashboardState.setActiveTab('messages');
  const view = harness.signals.signal(viewState(overrides));
  const calls = [];
  const controller = controllerFor(view, calls);
  const mounted = await harness.mount(harness.html`<${island.PhoneMailApp} controller=${controller} />`);
  const $ = (selector) => mounted.container.querySelector(selector);
  const $$ = (selector) => [...mounted.container.querySelectorAll(selector)];
  const tap = (element) => harness.act(() => element.click());
  return { harness, island, phonePage, view, calls, controller, $, $$, tap };
}

test('the row helpers: subject and preview line, inline decisions, plain access requests', async (t) => {
  const { island } = await setup(t);
  assert.deepEqual(island.rowLines({ subject: 'Hi', body: 'Hi\n\n  there  \nmore' }), { subject: 'Hi', preview: 'there' });
  assert.deepEqual(island.rowLines({ body: 'only body\nnext' }), { subject: 'only body', preview: 'next' });
  assert.deepEqual(island.rowLines({}), { subject: '(no subject)', preview: '' });
  assert.equal(island.pendingDecision({ options: ['a', 'b'] }, 'human'), true);
  assert.equal(island.pendingDecision({ options: ['a', 'b'], decision: 'a' }, 'human'), false);
  assert.equal(island.pendingDecision({ options: ['a', 'b'] }, 'all'), false, 'answered from the human folder only');
  const ctl = { accessIsPending: (request) => !request.status };
  assert.equal(island.accessInline({}, ctl), true);
  assert.equal(island.accessInline({ origin_peer: 'inst_x' }, ctl), false, 'a peer request opens the reader');
  assert.equal(island.accessInline({ purpose: 'group_link' }, ctl), false);
  assert.equal(island.accessInline({ status: 'approved' }, ctl), false);
  assert.equal(island.avatarHue('Alpha'), island.avatarHue('Alpha'));
});

test('the list shows mailbox chips, rows with previews and files, and answers decisions inline', async (t) => {
  const { $, $$, calls, tap } = await setup(t);
  const chips = $$('.phone-mail-chip').map((chip) => chip.textContent.trim());
  assert.deepEqual(chips, ['Human 1', 'All agent mail', 'Access 2', 'webshop 3', 'Agents ▾'], 'pinned folders first, then groups');
  assert.equal($('.phone-mail-chip.on').getAttribute('aria-pressed'), 'true');
  const rows = $$('.phone-mail-row');
  assert.equal(rows.length, 2);
  assert.ok(rows[0].classList.contains('unread'));
  assert.equal(rows[0].querySelector('.phone-mail-preview').textContent, 'First line 1');
  assert.match(rows[0].querySelector('.phone-mail-file').textContent, /shot\.png/);
  assert.ok(rows[1].querySelector('.phone-mail-inline'), 'a pending decision answers on its row');
  assert.equal(rows[0].querySelector('.phone-mail-inline'), null);

  await tap($$('.phone-mail-chip')[3]);
  assert.deepEqual(calls.at(-1), ['selectMailbox', 'group:webshop', { navigate: true }]);
  await tap($('.phone-mail-pager .ui-btn:last-child'));
  assert.deepEqual(calls.at(-1), ['goToPage', 2]);
  await tap(rows[0].querySelector('.phone-mail-open'));
  assert.deepEqual(calls.at(-1), ['selectMessage', 1, { navigate: true }]);
});

test('Agents ▾ lists agent folders only while it is open', async (t) => {
  const { $, $$, calls, tap } = await setup(t);
  assert.deepEqual(calls.filter(([name]) => name === 'listAgents'), [['listAgents', false]]);
  await tap($$('.phone-mail-chip').at(-1));
  assert.deepEqual(calls.filter(([name]) => name === 'listAgents').at(-1), ['listAgents', true]);
  assert.equal($('.phone-mail-list'), null, 'the picker replaces the list');
  const beta = $$('.phone-mail-agents .phone-menu-item')[0];
  assert.match(beta.textContent, /Beta\s*4/);
  await tap(beta);
  assert.ok(calls.some(([name, id]) => name === 'selectMailbox' && id === 'agt_beta'));
  assert.deepEqual(calls.filter(([name]) => name === 'listAgents').at(-1), ['listAgents', false]);
  assert.ok($('.phone-mail-list'), 'picking a folder returns to the list');
});

test('an open message is a full-screen page with a back button and a reply bar', async (t) => {
  const { $, view, phonePage, calls, harness, tap } = await setup(t, { selectedMsgId: 1 });
  assert.ok($('.phone-mail-reader'), 'the reader replaces the list');
  assert.equal($('.phone-mail-list'), null);
  assert.equal(phonePage.value?.hideNav, true, 'the page owns the bottom edge');
  assert.equal(phonePage.value.actions[0].icon, 'Terminal ›');
  assert.match($('.phone-mail-reply').textContent, /Reply to Alpha/);

  // Reached without a tap on the list (a deep link), back closes in place.
  await harness.act(() => phonePage.value.onBack());
  assert.deepEqual(calls.at(-1), ['closeMessage']);
  await harness.act(() => { view.value = { ...view.value, selectedMsgId: null }; });
  assert.equal(phonePage.value, null, 'closing the reader takes its page down');
  assert.ok($('.phone-mail-list'));
  await tap($('.phone-mail-open'));
  assert.deepEqual(calls.at(-1), ['selectMessage', 1, { navigate: true }]);
});

test('an agent filter narrows the agent picker, never the folder chips', async (t) => {
  const { $$ } = await setup(t, { boxQuery: 'bet' });
  assert.deepEqual($$('.phone-mail-chip').map((chip) => chip.textContent.trim()),
    ['Human 1', 'All agent mail', 'Access 2', 'webshop 3', 'Agents ▾']);
});

test('back goes through history only for the reader the list just pushed', async (t) => {
  const { harness, view, phonePage, calls, $, tap } = await setup(t);
  const backs = [];
  const had = Object.getOwnPropertyDescriptor(globalThis, 'history');
  Object.defineProperty(globalThis, 'history', { configurable: true, value: { length: 3, back: () => backs.push('back') } });
  t.after(() => {
    if (had) Object.defineProperty(globalThis, 'history', had);
    else delete globalThis.history;
  });
  const showReader = (id) => harness.act(() => { view.value = { ...view.value, selectedMsgId: id }; });

  await tap($('.phone-mail-open'));
  await showReader(1);
  await harness.act(() => phonePage.value.onBack());
  assert.deepEqual(backs, ['back'], 'opened from the list: back is the browser Back');

  // Opened from the list, closed some other way (the OS back gesture), then
  // reopened by a path that pushes no entry: back must not leave Mail.
  await showReader(null);
  await tap($('.phone-mail-open'));
  await showReader(1);
  await showReader(null);
  await showReader(1);
  await harness.act(() => phonePage.value.onBack());
  assert.deepEqual(backs, ['back']);
  assert.deepEqual(calls.at(-1), ['closeMessage']);
});

test('a refresh that pushes the open message off the page keeps it open', async (t) => {
  const { $, view, harness } = await setup(t, { selectedMsgId: 1 });
  assert.match($('.phone-mail-reader').textContent, /Subject 1/);
  // New mail arrives above it; message 1 moves to the next page.
  await harness.act(() => { view.value = { ...view.value, messages: [message(3), message(2)] }; });
  assert.ok($('.phone-mail-reader'), 'still reading');
  assert.match($('.phone-mail-reader').textContent, /Subject 1/, 'the same message, not "Select a message"');
  await harness.act(() => { view.value = { ...view.value, selectedMsgId: null }; });
  assert.equal($('.phone-mail-reader'), null, 'closing the selection closes it');
});

test('a message opens at its top, and back returns the list to where it was', async (t) => {
  const { $, view, harness, tap } = await setup(t);
  const scrolls = [];
  const saved = { scrollTo: globalThis.scrollTo, scrollY: Object.getOwnPropertyDescriptor(globalThis, 'scrollY') };
  globalThis.scrollTo = (x, y) => scrolls.push(y);
  Object.defineProperty(globalThis, 'scrollY', { configurable: true, value: 640 });
  t.after(() => {
    globalThis.scrollTo = saved.scrollTo;
    if (saved.scrollY) Object.defineProperty(globalThis, 'scrollY', saved.scrollY);
    else delete globalThis.scrollY;
  });
  await tap($('.phone-mail-open'));
  await harness.act(() => { view.value = { ...view.value, selectedMsgId: 1 }; });
  assert.deepEqual(scrolls, [0], 'the reader starts at the top');
  await harness.act(() => { view.value = { ...view.value, selectedMsgId: null }; });
  assert.deepEqual(scrolls, [0, 640], 'the list is back where it was');
});

test('a peer operator\'s reply dialog has its host on a phone too', async (t) => {
  const { harness } = await setup(t, { selectedMsgId: 1 });
  const { PEER_MAIL_EVENT } = await harness.importDashboardModule('js/peer-mail.js');
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent(PEER_MAIL_EVENT, {
    detail: { kind: 'operator', peer: 'inst_abcd', subject: 'Re: hi' },
  })));
  assert.ok(harness.document.querySelector('#peer-mail-subject'), 'the peer mail dialog opens');
});
