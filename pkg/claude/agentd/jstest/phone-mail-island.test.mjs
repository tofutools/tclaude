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
    listAgents: record('listAgents'), goToPage: record('goToPage'),
    decideAccess: async (...args) => { calls.push(['decideAccess', ...args]); return view.value.decideFails !== true; },
    clearMessageSelection: () => { calls.push(['clearMessageSelection']); view.value = { ...view.value, selectedMsgs: new Set() }; },
    renderMailTab: record('renderMailTab'), reloadMail: () => {}, reloadMessagesPage: () => {}, loadMoreAgents: () => {},
    toggleMessageSelection: (id, on) => {
      calls.push(['toggleMessageSelection', id, on]);
      const next = new Set(view.value.selectedMsgs);
      if (on) next.add(id); else next.delete(id);
      view.value = { ...view.value, selectedMsgs: next };
    },
    togglePageSelection: (on) => { calls.push(['togglePageSelection', on]); if (!on) view.value = { ...view.value, selectedMsgs: new Set() }; },
    deleteOneMessage: record('deleteOneMessage'), deleteSelectedMessages: record('deleteSelectedMessages'),
    setMessagesRead: record('setMessagesRead'), markAllAgentRead: () => {}, setPageSize: () => {},
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

async function setup(t, overrides, patch = null) {
  const harness = await createPreactHarness(t);
  const [island, { phonePage, phoneBar }, { dashboardState }, sheet] = await Promise.all([
    harness.importDashboardModule('js/phone-mail-island.js'),
    harness.importDashboardModule('js/phone-page.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
  ]);
  t.after(() => sheet.closePhoneSheet());
  dashboardState.setActiveTab('messages');
  const view = harness.signals.signal(viewState(overrides));
  const calls = [];
  const controller = controllerFor(view, calls);
  patch?.(controller);
  const mounted = await harness.mount(harness.html`<div><${island.PhoneMailApp} controller=${controller} /><${sheet.PhoneSheetHost} /></div>`);
  const $ = (selector) => mounted.container.querySelector(selector);
  const $$ = (selector) => [...mounted.container.querySelectorAll(selector)];
  const tap = (element) => harness.act(() => element.click());
  const dialog = () => mounted.container.querySelector('.phone-sheet[role=dialog]');
  return { harness, island, phonePage, phoneBar, sheet, dialog, view, calls, controller, $, $$, tap };
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

test('the list shows mailbox chips, rows with previews and files, and marks decisions without answering them', async (t) => {
  const { $, $$, calls, tap } = await setup(t);
  const chips = $$('.phone-mail-chip').map((chip) => chip.textContent.trim());
  assert.deepEqual(chips, ['Human 1', 'All agent mail', 'Access 2', 'webshop 3'], 'pinned folders first, then groups');
  assert.equal($('.phone-mail-chip.on').getAttribute('aria-pressed'), 'true');
  const rows = $$('.phone-mail-row');
  assert.equal(rows.length, 2);
  assert.ok(rows[0].classList.contains('unread'));
  assert.equal(rows[0].querySelector('.phone-mail-preview').textContent, 'First line 1');
  assert.match(rows[0].querySelector('.phone-mail-file').textContent, /🖼 shot\.png · 10 B/, 'a file shows its size');
  assert.equal(rows[0].querySelector('.phone-av').textContent, 'A', 'the sender\'s avatar');
  assert.equal(rows[0].querySelector('.phone-mail-group'), null);
  assert.equal(rows[1].querySelector('.phone-mail-decide').textContent, 'Decide · 2 options ›', 'a pending decision says so');
  assert.equal(rows[0].querySelector('.phone-mail-decide'), null);
  assert.deepEqual([...rows[1].querySelectorAll('button')].map((b) => b.className), ['phone-mail-open'],
    'no answer on the row: its one button opens it');
  await tap(rows[1].querySelector('.phone-mail-decide'));
  assert.deepEqual(calls.at(-1), ['selectMessage', 2, { navigate: true }], 'a tap on the pill opens the message');

  await tap($$('.phone-mail-chip')[3]);
  assert.deepEqual(calls.at(-1), ['selectMailbox', 'group:webshop', { navigate: true }]);
  await tap($('.phone-mail-pager .ui-btn:last-child'));
  assert.deepEqual(calls.at(-1), ['goToPage', 2]);
  await tap(rows[0].querySelector('.phone-mail-open'));
  assert.deepEqual(calls.at(-1), ['selectMessage', 1, { navigate: true }]);
});

test('the title names the mailbox; its ▾ lists every mailbox, agent folders only while open', async (t) => {
  const { $, $$, calls, tap, phoneBar, dialog, harness } = await setup(t);
  assert.equal(phoneBar.value.tab, 'messages');
  assert.equal(phoneBar.value.menu.sub, 'Human');
  assert.deepEqual(phoneBar.value.actions.map((a) => a.icon), ['✓✓', '🔍']);
  assert.equal(calls.filter(([name]) => name === 'listAgents').length, 0, 'no agent roster for the list');
  await harness.act(() => phoneBar.value.menu.onClick());
  assert.equal(dialog().getAttribute('aria-label'), 'Mailboxes');
  assert.deepEqual(calls.filter(([name]) => name === 'listAgents').at(-1), ['listAgents', true]);
  const items = [...dialog().querySelectorAll('.phone-menu-item')].map((b) => b.textContent.replace(/\s+/g, ''));
  assert.deepEqual(items, ['🔔Human1', '✉Allagentmail', '🔑Access2', '#webshop3', 'BBeta4']);
  await tap([...dialog().querySelectorAll('.phone-menu-item')].at(-1));
  assert.ok(calls.some(([name, id]) => name === 'selectMailbox' && id === 'agt_beta'));
  assert.equal(dialog(), null, 'picking one closes the sheet');
  assert.deepEqual(calls.filter(([name]) => name === 'listAgents').at(-1), ['listAgents', false]);
  assert.ok($('.phone-mail-list'));
  assert.equal($$('.phone-mail-chip').length, 4);
});

test('🔍 opens a search over the mailbox; a kept search shows it open', async (t) => {
  const { $, calls, harness, phoneBar } = await setup(t);
  assert.equal($('.phone-mail-search'), null, 'no search field until asked');
  await harness.act(() => phoneBar.value.actions[1].onClick());
  await harness.input($('.phone-mail-search'), 'deploy');
  assert.deepEqual(calls.at(-1), ['setMessageQuery', 'deploy']);
});

test('a search the controller kept shows its field open', async (t) => {
  const { $ } = await setup(t, { messageQuery: 'deploy' });
  assert.equal($('.phone-mail-search').value, 'deploy', 'a kept search is not hidden');
});

test('an open message is a full-screen page with a back button and a reply bar', async (t) => {
  const { $, view, phonePage, calls, harness, tap } = await setup(t, { selectedMsgId: 1 });
  assert.ok($('.phone-mail-reader'), 'the reader replaces the list');
  assert.equal($('.phone-mail-list'), null);
  // The reply bar owns the bottom edge, with the ▾/▴ toggle left of the
  // field: the nav starts shown, and the toggle hides it (and remembers).
  assert.equal(phonePage.value?.hideNav, false, 'the nav starts shown under the reader');
  const toggle = $('.phone-mail-replybar .phone-nav-toggle');
  assert.equal(toggle.getAttribute('aria-label'), 'Hide page selector');
  assert.equal(toggle.getAttribute('aria-expanded'), 'true');
  assert.ok($('.phone-mail-reader.with-nav'), 'the reply bar sits on the nav');
  await tap(toggle);
  assert.equal(phonePage.value?.hideNav, true, 'hidden, the page owns the bottom edge');
  assert.equal($('.phone-mail-replybar .phone-nav-toggle').getAttribute('aria-expanded'), 'false');
  assert.equal($('.phone-mail-reader.with-nav'), null);
  await tap($('.phone-mail-replybar .phone-nav-toggle'));
  assert.equal(phonePage.value?.hideNav, false);
  assert.deepEqual(phonePage.value.actions.map((a) => a.icon), ['⌖', '✉', '🗑']);
  assert.equal($('.mail-headers'), null, 'no desktop header table');
  assert.equal($('.phone-mail-title').textContent, 'Subject 1');
  assert.equal($('.phone-mail-sender-name').textContent, 'Alpha');
  assert.match($('.phone-mail-sender-line').textContent, /^to you/);
  assert.match($('.phone-mail-terminal').textContent, /Terminal ›/);
  assert.match($('.phone-mail-caption').textContent, /shot\.png · 10 B · ↓ save/);
  assert.match($('.phone-mail-reply').textContent, /Reply to Alpha/);
  await harness.act(() => phonePage.value.actions[2].onClick());
  assert.deepEqual(calls.at(-1), ['deleteOneMessage', 1]);

  // With no router to step back through, back closes in place.
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
    ['Human 1', 'All agent mail', 'Access 2', 'webshop 3']);
});

test('back leads to the open folder\'s list: a step back onto it when the router can, else in place', async (t) => {
  const { harness, phonePage, calls } = await setup(t, { selectedMsgId: 1 });
  const [bridge, back] = await Promise.all([
    harness.importDashboardModule('js/mail-bridge.js'),
    harness.importDashboardModule('js/nav-back.js'),
  ]);
  t.after(bridge.registerMailController({ mailLocation: () => ({ tab: 'messages', selection: 'human', item: '1' }) }));
  const asked = [];
  let below = true;
  t.after(back.registerStepBack((parent) => { asked.push(parent); return below; }));
  await harness.act(() => phonePage.value.onBack());
  assert.deepEqual(asked, [{ tab: 'messages', selection: 'human', item: undefined }], 'the parent is the folder\'s list');
  assert.notDeepEqual(calls.at(-1), ['closeMessage'], 'stepped back onto the list\'s own entry');
  below = false;
  await harness.act(() => phonePage.value.onBack());
  assert.deepEqual(calls.at(-1), ['closeMessage'], 'no list under it: the list in place');
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

// pointer drives a row's gestures: a press at x, moves, and a lift.
function pointer(harness, element) {
  const fire = (type, x, y = 10) => {
    const event = new harness.window.Event(type, { bubbles: true });
    Object.defineProperty(event, 'clientX', { value: x });
    Object.defineProperty(event, 'clientY', { value: y });
    Object.defineProperty(event, 'button', { value: 0 });
    element.dispatchEvent(event);
  };
  return {
    swipe: (to) => harness.act(() => { fire('pointerdown', 200); fire('pointermove', 200 + to / 2); fire('pointermove', 200 + to); fire('pointerup', 200 + to); }),
    down: () => harness.act(() => fire('pointerdown', 200)),
    up: () => harness.act(() => fire('pointerup', 200)),
  };
}

test('a swipe right marks agent mail read, a swipe left deletes it; a short drag does nothing', async (t) => {
  const { harness, $$, calls } = await setup(t, { selected: 'agt_beta' });
  const row = $$('.phone-mail-row')[0];
  await pointer(harness, row).swipe(40);
  assert.equal(calls.filter(([n]) => n === 'setMessagesRead').length, 0);
  await pointer(harness, row).swipe(120);
  assert.deepEqual(calls.at(-1), ['setMessagesRead', [1], true]);
  await pointer(harness, row).swipe(-120);
  assert.deepEqual(calls.at(-1), ['deleteOneMessage', 1]);
  assert.ok(!calls.some(([n]) => n === 'selectMessage'), 'a swipe does not open the message');
});

test('a long press starts selecting; taps then pick rows, and the bar acts on them', async (t) => {
  const { harness, $, $$, calls, tap } = await setup(t, { selected: 'agt_beta' });
  const row = $$('.phone-mail-row')[0];
  const press = pointer(harness, row);
  await press.down();
  // The hold fires on its own timer: wait for it, a bounded number of turns.
  const deadline = Date.now() + 3000;
  while (!calls.some(([n]) => n === 'toggleMessageSelection') && Date.now() < deadline) {
    await harness.act(() => new Promise((resolve) => setTimeout(resolve, 25)));
  }
  await press.up();
  assert.deepEqual(calls.find(([n]) => n === 'toggleMessageSelection'), ['toggleMessageSelection', 1, true]);
  assert.match($('.phone-mail-selected').textContent, /1 selected/);
  assert.equal($('.phone-mail-chips'), null, 'the selection bar takes the chips\' place');
  await tap($$('.phone-mail-open')[1]);
  assert.deepEqual(calls.at(-1), ['toggleMessageSelection', 2, true], 'a tap picks, it does not open');
  await tap([...$('.phone-mail-selectbar').querySelectorAll('button')].find((b) => /Delete/.test(b.textContent)));
  assert.deepEqual(calls.at(-1), ['deleteSelectedMessages']);
  await tap($('.phone-mail-selectbar [aria-label="Stop selecting"]'));
  assert.deepEqual(calls.at(-1), ['clearMessageSelection'], 'every page\'s picks, not just this one\'s');
  assert.equal($('.phone-mail-selectbar'), null);
});

test('✎ Write opens the message sheet with a To field over this node\'s agents', async (t) => {
  const { $, tap, dialog, harness } = await setup(t);
  const { dashboardState } = await harness.importDashboardModule('js/snapshot-store.js');
  dashboardState.beginRequest();
  dashboardState.commitRequest(1, { groups: [{ name: 'webshop', members: [
    { agent_id: 'agt_00000000000000000000000000000001', conv_id: 'c1', title: 'lead', online: true },
    { agent_id: 'agt_00000000000000000000000000000002', conv_id: 'c2', title: 'checkout-ui', online: false },
  ] }] });
  await tap($('.phone-mail-write'));
  assert.equal(dialog().getAttribute('aria-label'), 'New message');
  const to = dialog().querySelector('.phone-compose-to-input');
  assert.equal(harness.document.activeElement, to, 'To takes focus');
  await harness.input(to, 'check');
  const hits = [...dialog().querySelectorAll('.phone-compose-recipient')].map((b) => b.textContent);
  assert.deepEqual(hits, ['checkout-uiwebshopoffline']);
  await tap(dialog().querySelector('.phone-compose-recipient'));
  assert.match(dialog().querySelector('.phone-compose-to-picked').textContent, /checkout-ui/);
});

test('Reply opens the message sheet as a reply to the sender', async (t) => {
  const { $, tap, dialog } = await setup(t, { selectedMsgId: 1 });
  await tap($('.phone-mail-send'));
  assert.equal(dialog().getAttribute('aria-label'), 'Reply to Alpha');
  assert.equal(dialog().querySelector('.phone-compose-title').textContent, 'Reply to Alpha');
});

test('a plain access request is its approval card, answered through the controller', async (t) => {
  const request = { id: 'ar_1', perm: 'git.push', conv_title: 'checkout-ui', body: 'push PR #1', path: '/api/git/push',
    created_at: '2026-07-12T00:00:00Z', auto_grantable: true };
  const access = (controller) => {
    controller.messageView = () => ({ access: true, allAccess: [request], pendingAccess: [request], handledAccess: [] });
  };
  const { $, calls, tap } = await setup(t, { selected: 'access-requests' }, access);
  const card = $('.phone-mail-card .phone-box-key');
  assert.ok(card, 'the approval card');
  const labels = [...card.querySelectorAll('.phone-btns button')].map((b) => b.textContent.trim());
  assert.deepEqual(labels, ['Deny', '+5 min', 'Approve'], 'Deny, as on Now (B15)');
  await tap(card.querySelectorAll('.phone-btns button')[0]);
  assert.deepEqual(calls.find(([n]) => n === 'decideAccess'), ['decideAccess', 'ar_1', 'deny']);
});

test('a decision Mail could not record frees the card\'s buttons again', async (t) => {
  const request = { id: 'ar_1', perm: 'git.push', conv_title: 'checkout-ui', created_at: '2026-07-12T00:00:00Z' };
  const access = (controller) => {
    controller.messageView = () => ({ access: true, allAccess: [request], pendingAccess: [request], handledAccess: [] });
  };
  const { $, tap, harness } = await setup(t, { selected: 'access-requests', decideFails: true }, access);
  const approve = () => [...$('.phone-mail-card .phone-btns').querySelectorAll('button')].at(-1);
  await tap(approve());
  await harness.act(async () => {});
  assert.equal(approve().textContent.trim(), 'Approve', 'not stuck on Approving…');
  assert.equal(approve().disabled, false);
});

test('a decision\'s reader answers it with a note, as on Now, or dismisses it', async (t) => {
  const posts = [];
  const fetchBefore = globalThis.fetch;
  globalThis.fetch = async (url, init) => { posts.push([url, JSON.parse(init.body)]); return { ok: true }; };
  t.after(() => { globalThis.fetch = fetchBefore; });
  const { $, $$, tap, harness } = await setup(t, { selectedMsgId: 2 });
  const block = $('.phone-mail-decision');
  assert.ok(block, 'the phone decision block');
  assert.equal($('.mail-decision'), null, 'not the desktop Decide: box');
  assert.deepEqual([...block.querySelectorAll('.phone-decision-options button')].map((b) => b.textContent.trim()), ['Ship', 'Wait', 'Dismiss']);
  const note = block.querySelector('.phone-decision-note');
  await harness.act(() => { note.value = 'after lunch'; note.dispatchEvent(new globalThis.Event('input', { bubbles: true })); });
  await tap($$('.phone-decision-options button')[1]);
  await harness.act(async () => {});
  assert.deepEqual(posts, [['/api/human-messages/reply', { id: 2, choice: 2, body: 'after lunch' }]]);
});

test('an answered decision\'s reader says what was decided', async (t) => {
  const { $ } = await setup(t, { selectedMsgId: 2, messages: [message(2, { options: ['Ship', 'Wait'], decision: 'Ship' })] });
  assert.equal($('.phone-mail-decision'), null);
  assert.match($('.mail-decision.decided').textContent, /Decided:\s*Ship/);
});

test('a sudo plan\'s reader reads its body as facts: what, for how long, why', async (t) => {
  const request = { id: 'ar_2', perm: 'sudo.agent.spawn', conv_title: 'cold-reviewer', created_at: '2026-07-12T00:00:00Z',
    body: 'Sudo request:\n  slugs:\n    - agent.spawn\n  duration: 30m0s\n  expires_at: 2026-07-12T00:30:00Z\n  reason: spawn a test runner' };
  const access = (controller) => {
    controller.messageView = () => ({ access: true, allAccess: [request], pendingAccess: [request], handledAccess: [] });
  };
  const reader = await setup(t, { selected: 'access-requests', selectedMsgId: 'ar_2' }, access);
  const facts = reader.$$('.phone-mail-facts > div').map((d) => [d.querySelector('dt').textContent, d.querySelector('dd').textContent]);
  assert.deepEqual(facts, [['Permissions', 'agent.spawn'], ['For', '30 min'], ['Reason', 'spawn a test runner']], 'no raw body');
  const card = reader.$('.phone-box-key');
  assert.match(card.textContent, /Plan for 30 min: agent\.spawn/);
  assert.match(card.querySelector('.phone-plan-reason').textContent, /“spawn a test runner”/);
  assert.match(card.getAttribute('aria-label'), /because: spawn a test runner$/);
});

test('a plain access request\'s reader stacks what it asks for, with Always allow', async (t) => {
  const request = { id: 'ar_1', perm: 'git.push', conv_title: 'checkout-ui', body: 'push PR #1', path: '/api/git/push',
    created_at: '2026-07-12T00:00:00Z', auto_grantable: true };
  const access = (controller) => {
    controller.messageView = () => ({ access: true, allAccess: [request], pendingAccess: [request], handledAccess: [] });
  };
  const reader = await setup(t, { selected: 'access-requests', selectedMsgId: 'ar_1' }, access);
  assert.equal(reader.$('.mail-headers'), null, 'not the desktop table');
  assert.deepEqual(reader.$$('.phone-mail-facts dt').map((dt) => dt.textContent), ['Permission', 'Endpoint', 'Body']);
  assert.match(reader.$('.phone-mail-always').textContent, /Always allow/);
});

test('the viewer is a step of its own: Back closes it and leaves the reader (B4)', async (t) => {
  const { harness, $, phonePage } = await setup(t, { selectedMsgId: 1, messages: [
    message(1, { attachment: { filename: 'shot.png', content_type: 'image/png', size_bytes: 2048, previewable: true } }),
  ] });
  const routes = await harness.importDashboardModule('js/overlay-routes.js');
  // With the phone's history installed too: the viewer is a Back layer as
  // well, but a routed one, so it is still one step and not two.
  const history = await harness.importDashboardModule('js/phone-history.js');
  const uninstall = history.installPhoneHistory({ view: harness.signals.signal(null), doc: harness.document });
  t.after(uninstall);
  await new Promise((resolve) => setTimeout(resolve, 5));
  const phoneBefore = routes.overlayParams().phone;
  const announced = [];
  harness.document.addEventListener(routes.OVERLAY_EVENT, (event) => announced.push(event.detail));
  assert.match($('.phone-mail-zoom').textContent, /tap to zoom/);
  await harness.act(() => $('.phone-mail-preview-img').click());
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 5)));
  assert.equal(routes.overlayParams().file, '0');
  assert.equal(routes.overlayParams().phone, phoneBefore, 'no phone step of its own');
  assert.deepEqual(announced, [{ closed: false, replace: false }], 'opening is one history step');
  assert.ok(harness.document.querySelector('[role="dialog"]'), 'the viewer is open');
  await harness.act(() => routes.restoreOverlays({}));
  assert.equal(routes.overlayParams().file, '');
  assert.ok($('.phone-mail-reader'), 'Back leaves the reader');
  assert.ok(phonePage.value);
});

test('Find › sits beside Terminal ›, on in either case; a sender that is not live explains a terminal tap', async (t) => {
  const { $, phonePage, harness, tap, island } = await setup(t, { selectedMsgId: 1 }, (controller) => { controller.senderOnline = () => false; });
  const route = await harness.importDashboardModule('js/agent-route.js');
  const terminal = $('.phone-mail-terminal');
  assert.equal(terminal.disabled, false, 'tappable, so it can say why');
  assert.equal(terminal.getAttribute('aria-disabled'), 'true');
  assert.equal(phonePage.value.actions[0].muted, true, '⌖ too');
  const find = $('.phone-mail-find');
  assert.equal(find.textContent, 'Find ›');
  assert.equal(find.getAttribute('aria-label'), 'Find Alpha in Agents');
  assert.equal(find.disabled, false, 'an offline sender is what Find is for');
  assert.equal(terminal.nextElementSibling, find);
  assert.equal(route.agentPath('agt_alpha'), '/groups?agent=agt_alpha');

  // Why a terminal cannot open: from the snapshot, then the retired list.
  const snapshot = { agents: [{ agent_id: 'agt_alpha', conv_id: 'conv-a', online: false }], ungrouped: [] };
  const msg = { from_agent: 'agt_alpha', from_conv: 'conv-a' };
  const never = async () => { throw new Error('not asked'); };
  assert.match(await island.whyNotLive(msg, 'Alpha', { snapshot, fetchImpl: never }), /^Alpha is offline\. Find › shows it in Agents/);
  const asked = [];
  const retired = (rows) => async (url) => { asked.push(url); return { ok: true, json: async () => ({ rows }) }; };
  assert.equal(await island.whyNotLive(msg, 'Alpha', { snapshot: {}, fetchImpl: retired([{ agent_id: 'agt_alpha' }]) }), 'Alpha was retired.');
  assert.equal(asked.at(-1), '/api/retired?limit=20&q=agt_alpha');
  assert.equal(await island.whyNotLive(msg, 'Alpha', { snapshot: {}, fetchImpl: retired([{ agent_id: 'agt_alphabet' }]) }),
    'Alpha no longer exists (deleted).', 'q is a contains filter: only an exact id counts');
  assert.match(await island.whyNotLive({}, 'x', { snapshot: {}, fetchImpl: never }), /^Unknown sender/);
  assert.equal(await island.whyNotLive(msg, 'Alpha', { snapshot: {}, fetchImpl: async () => ({ ok: false }) }), 'Alpha is not running.');
  await tap(terminal);
});

test('Find › is for this node\'s agents: none without from_agent, nor for mail a peer relayed', async (t) => {
  const { $ } = await setup(t, { selectedMsgId: 1, messages: [message(1, { from_agent: '' })] });
  assert.equal($('.phone-mail-find'), null);
});
