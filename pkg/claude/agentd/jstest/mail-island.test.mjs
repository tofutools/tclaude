import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent, assertSameNode } from './assertions.mjs';
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
    renderMailTab: noop,
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
    reloadMail: () => {},
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

test('Messages loads immediately when its tab becomes active or is reselected', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const { dashboardState } = await harness.importDashboardModule('js/snapshot-store.js');
  const state = harness.signals.signal(populated());
  const controller = controllerFor(state);
  let loads = 0;
  controller.renderMailTab = () => { loads += 1; };
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controller} />`);

  assert.equal(loads, 0);
  await harness.act(() => { dashboardState.setActiveTab('messages'); });
  assert.equal(loads, 1);
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent(
    'tclaude:tab-reselected', { detail: { tab: 'messages' } },
  )));
  assert.equal(loads, 2);
  await mounted.unmount();
  harness.document.dispatchEvent(new harness.window.CustomEvent(
    'tclaude:tab-reselected', { detail: { tab: 'messages' } },
  ));
  assert.equal(loads, 2);
});

test('Messages island preserves native controls, CSS hooks, focus, reader, and keyed rows across incoming updates', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const { dashboardState } = await harness.importDashboardModule('js/snapshot-store.js');
  dashboardState.publishLocalEdit({
    agents: [{
      agent_id: 'agt_alpha', conv_id: 'conv-a', state: { harness: 'copilot' },
    }],
  });
  t.after(() => dashboardState.publishLocalEdit(null));
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
  const attachmentIndicator = mounted.container.querySelector('.mail-row-attachments');
  assert.equal(attachmentIndicator.getAttribute('title'), '1 file');
  assert.equal(attachmentIndicator.getAttribute('role'), 'img');
  assert.equal(attachmentIndicator.getAttribute('aria-label'), '1 file');
  assert.ok(attachmentIndicator.querySelector('svg'));
  assert.equal(attachmentIndicator.querySelector('.mail-row-attachment-count').textContent, '1',
    'every nonempty type shows its count');
  assert.equal(mounted.container.querySelector('[data-act="msg-focus"]').dataset.harness, 'copilot',
    'message focus carries the live sender harness into the terminal launcher');

  const originalRow = mounted.container.querySelector('.mail-row-wrap');
  const originalReader = mounted.container.querySelector('#mail-reader');
  originalReader.scrollTop = 37;
  messageFilter.focus();
  await harness.act(() => { state.value = { ...state.value, total: 2, totalUnfiltered: 2,
    messages: [message(2), ...state.value.messages] }; });
  assertSameNode(mounted.container.querySelectorAll('.mail-row-wrap')[1], originalRow);
  assertSameNode(mounted.container.querySelector('#mail-reader'), originalReader);
  assert.equal(originalReader.scrollTop, 37);
  assertSameNode(harness.document.activeElement, messageFilter);

  await harness.act(() => { state.value = { ...state.value, messages: state.value.messages.map((item) => (
    item.id === 1 ? { ...item, attachments: [item.attachment, {
      filename: 'screenshot.png', content_type: 'image/png', size_bytes: 2048,
    }], attachment: undefined } : item
  )) }; });
  const multiIndicator = mounted.container.querySelector('.mail-row-attachments');
  assert.equal(multiIndicator.getAttribute('title'), '1 file, 1 image');
  assert.deepEqual([...multiIndicator.querySelectorAll('.mail-row-attachment-count')].map((node) => node.textContent), ['1', '1']);

  harness.document.body.classList.add('wizard');
  await harness.act(() => { state.value = { ...state.value }; });
  assert.equal(mailboxFilter.getAttribute('placeholder'), 'Seek a familiar…');
  assert.equal(messageFilter.getAttribute('placeholder'), 'Search the scrolls…');
  await mounted.unmount();
});

test('Messages access request view retains decree hooks and decision controls', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const request = { id: 'req-1', conv_id: 'conv-a', current_conv_id: 'conv-b', agent_id: 'agt_alpha', conv_title: 'Alpha',
    caller_state: 'active', title_status: 'current',
    perm: 'human.clipboard', created_at: '2026-07-12T00:00:00Z', deadline: '2026-07-12T00:05:00Z', auto_grantable: true };
  const value = { ...populated(), selected: 'access-requests', selectedMsgId: 'req-1', messages: [] };
  const state = harness.signals.signal(value);
  const controller = controllerFor(state);
  controller.messageView = () => ({ access: true, allAccess: [request], pendingAccess: [request], handledAccess: [], search: '' });
  controller.messageCountText = () => '1 pending';
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controller} />`);
  assert.equal(mounted.container.querySelector('.access-row-wrap').dataset.kind, 'decree');
  assert.equal(mounted.container.querySelector('#mail-reader').dataset.kind, 'decree');
  const headerRows = [...mounted.container.querySelectorAll('.mail-headers .mail-hrow')]
    .map((row) => row.textContent.replace(/\s+/g, ' ').trim());
  assert.ok(headerRows.some((row) => row.startsWith('FromAlpha')),
    'the readable caller name must lead while its identifiers stay on hover');
  const fromValue = mounted.container.querySelector('.mail-headers .mail-hval span');
  assert.equal(fromValue.getAttribute('title'), 'agt_alpha / conv-b');
  assert.doesNotMatch(fromValue.textContent, /agt_alpha/);
  assert.ok(headerRows.some((row) => row.startsWith('Request generationconv-a')));
  assert.ok(headerRows.some((row) => row.startsWith('Current generationconv-b')));
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
  assertSameNode(harness.document.activeElement, mounted.container.querySelector('#filter-messages'));

  await harness.act(() => { state.value = { ...state.value, busy: true }; });
  const mailbox = mounted.container.querySelector('.mailbox');
  assert.equal(mailbox.hasAttribute('disabled'), true);
  await mounted.unmount();
});

test('Messages clear action keeps the cleared query when the input has not repainted yet', async (t) => {
  const harness = await createPreactHarness(t);
  // mail.js imports these modules for the live dashboard, but the search
  // action only needs their small exported seams. Stubbing them keeps this
  // regression focused on the production Messages controller without
  // starting the full dashboard bootstrap.
  await harness.replaceDashboardModule('js/dashboard.js', `
    export let lastSnapshot = null;
    export function setLastSnapshot(value) { lastSnapshot = value; }
  `);
  await harness.replaceDashboardModule('js/refresh.js', `
    export function confirmModal() {}
    export function toast() {}
  `);
  const previousFetch = globalThis.fetch;
  globalThis.fetch = async () => ({
    ok: true,
    json: async () => ({ messages: [], total: 0, total_unfiltered: 0 }),
  });
  t.after(() => { globalThis.fetch = previousFetch; });

  const { accessWho, mailController } = await harness.importDashboardModule('js/mail.js');
  assert.equal(accessWho({ agent_id: 'agt_alpha', conv_id: 'conv-a', conv_title: 'Alpha', title_status: 'current' }), 'Alpha');
  assert.equal(accessWho({ agent_id: 'agt_alpha', conv_id: 'conv-a', conv_title: '(title unavailable)', title_status: 'unavailable' }), 'agt_alpha');
  assert.equal(accessWho({
    agent_id: 'agt_alpha', conv_id: 'conv-a', conv_title: '(title unavailable)',
  }), 'agt_alpha');
  assert.equal(accessWho({
    agent_id: 'agt_alpha', conv_id: 'conv-a', conv_title: 'Alpha',
    title_status: 'current', caller_state: 'retired',
  }), 'Alpha · retired');
  assert.equal(accessWho({
    agent_id: 'agt_alpha', conv_id: 'conv-a', conv_title: 'Alpha',
    title_status: 'current', caller_state: 'missing',
  }), 'Alpha · metadata missing');
  const input = harness.document.body.appendChild(harness.document.createElement('input'));
  input.id = 'filter-messages';
  input.value = 'stale query';
  mailController.state.data.messageQuery = 'stale query';

  mailController.setMessageQuery('');

  assert.equal(mailController.state.data.messageQuery, '');
  clearTimeout(mailController.state.data.searchTimer);
  mailController.state.data.searchTimer = null;
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

test('a peer operator\'s access request decides with a TTL and an optional narrower group, never "always"', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const request = { id: 'par-1', origin_peer: 'inst_forge7', conv_title: 'operator@inst_forge7', caller_state: 'operator', title_status: 'available',
    perm: 'sessions.watch', grant_ttl_seconds: 86400, body: 'need to clone', body_label: 'Reason',
    created_at: '2026-07-12T00:00:00Z', deadline: '2026-07-12T00:05:00Z', auto_grantable: false, scope_display: 'group_id=0' };
  const realFetch = globalThis.fetch;
  globalThis.fetch = async () => ({ ok: true, json: async () => ({ groups: [
    { group_id: 4, name: 'ops', federation_links: [{ peer: 'inst_forge7', kind: 'grant', slugs: ['agents.status'] }] },
    { group_id: 5, name: 'private', federation_links: [] },
    { group_id: 6, name: 'mirror', federation_links: [{ peer: 'inst_forge7', kind: 'route', direction: 'out' }] },
  ] }) });
  t.after(() => { globalThis.fetch = realFetch; });
  const state = harness.signals.signal({ ...populated(), selected: 'access-requests', selectedMsgId: 'par-1', messages: [] });
  const controller = controllerFor(state);
  const decided = [];
  controller.decideAccess = (...a) => decided.push(a);
  controller.messageView = () => ({ access: true, allAccess: [request], pendingAccess: [request], handledAccess: [], search: '' });
  controller.messageCountText = () => '1 pending';
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controller} />`);
  await harness.act(() => new Promise((r) => setTimeout(r, 25)));
  const root = mounted.container;
  assert.doesNotMatch(root.textContent, /Always allow/);
  assert.match(root.textContent, /every group on this node, including future ones · 1 day/);
  assert.match(root.textContent, /Approving lets operator@inst_forge7 use sessions.watch \(read-only terminal view of member agents\) in every group on this node, including future ones for 1 day/);
  assert.deepEqual([...root.querySelectorAll('.access-peer-group option')].map((o) => o.textContent), ['every group, including future ones', 'ops'], 'only groups the peer holds grants in narrow');
  const group = root.querySelector('.access-peer-group');
  for (const o of group.querySelectorAll('option')) { if (o.value === '4') o.setAttribute('selected', ''); else o.removeAttribute('selected'); }
  await harness.act(() => harness.fireEvent(group, 'change'));
  const ttl = root.querySelector('.access-peer-ttl');
  assert.deepEqual([...ttl.querySelectorAll('option')].map((o) => o.textContent), ['1 hour', '8 hours', '1 day'], 'approval never lengthens the grant');
  for (const o of ttl.querySelectorAll('option')) { if (o.value === '3600') o.setAttribute('selected', ''); else o.removeAttribute('selected'); }
  await harness.act(() => harness.fireEvent(ttl, 'change'));
  assert.match(root.textContent, /in group ops for 1 hour/);
  await harness.act(() => root.querySelector('.access-btn.approve').click());
  await harness.act(() => root.querySelector('.access-btn.deny').click());
  assert.deepEqual(decided, [['par-1', 'approve', { grant_ttl_seconds: 3600, group_id: 4 }], ['par-1', 'deny']]);
  await mounted.unmount();
});

test('a cross-node link request names the exact edge and decides approve, approve_future or deny', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const request = { id: 'gl-1', purpose: 'group_link', origin_peer: 'inst_alice1', conv_title: 'alice',
    link: { source_peer_label: 'alice', source_group: 'alpha', dest_group: 'research', mode: 'members->members', direction: 'in' },
    created_at: '2026-07-12T00:00:00Z', deadline: '2026-07-13T00:00:00Z' };
  const state = harness.signals.signal({ ...populated(), selected: 'access-requests', selectedMsgId: 'gl-1', messages: [] });
  const controller = controllerFor(state);
  const decided = [];
  controller.decideAccess = (...a) => decided.push(a);
  controller.messageView = () => ({ access: true, allAccess: [request], pendingAccess: [request], handledAccess: [], search: '' });
  controller.messageCountText = () => '1 pending';
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controller} />`);
  const root = mounted.container;
  assert.match(root.textContent, /Cross-node link request/);
  assert.match(root.textContent, /alice:alpha → research · members → members/);
  assert.match(root.textContent, /nothing broader/);
  assert.doesNotMatch(root.textContent, /Always allow/);
  const buttons = [...root.querySelectorAll('.access-reader-actions .access-btn')];
  assert.deepEqual(buttons.map((b) => b.textContent.trim()), ['Approve + allow future links into research', 'Decline', 'Approve']);
  for (const b of buttons) await harness.act(() => b.click());
  assert.deepEqual(decided, [['gl-1', 'approve_future'], ['gl-1', 'deny'], ['gl-1', 'approve']]);
  await mounted.unmount();
});

test('a cross-node link request this node sent is read-only and waits on the peer operator', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const request = { id: 'gl-2', purpose: 'group_link', origin_peer: 'inst_bob1', conv_title: 'bob', status: 'pending',
    link: { source_peer_label: 'bob', source_group: 'alpha', dest_group: 'research', mode: 'members->members', direction: 'out' },
    created_at: '2026-07-12T00:00:00Z', deadline: '2026-07-13T00:00:00Z' };
  const state = harness.signals.signal({ ...populated(), selected: 'access-requests', selectedMsgId: 'gl-2', messages: [] });
  const controller = controllerFor(state);
  controller.messageView = () => ({ access: true, allAccess: [request], pendingAccess: [request], handledAccess: [], search: '' });
  controller.messageCountText = () => '1 pending';
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controller} />`);
  const root = mounted.container;
  assert.match(root.textContent, /You asked/);
  assert.match(root.textContent, /waiting for bob's operator/);
  assert.equal(root.querySelectorAll('.access-reader-actions .access-btn').length, 0, 'no decision buttons on our own request');
  await mounted.unmount();
});

function proposalRequest(proposal) {
  return { id: proposal.id, purpose: 'peer_proposal', origin_peer: 'inst_alice1', status: 'pending', proposal,
    created_at: '2026-07-12T00:00:00Z', deadline: '2026-07-13T00:00:00Z' };
}

const aliceProposal = {
  id: 'pp-1', peer: 'inst_alice1', peer_label: 'alice', revision: 1, state: 'sent', awaiting: 'us', ours: false,
  deadline: '2026-07-13T00:00:00Z', note: 'same release',
  items: [
    { id: 'i1', kind: 'share_visibility', host: 'them', group_id: 'g_a', group_name: 'alpha', member_count: 5, include_members: true, status: 'pending' },
    { id: 'i2', kind: 'share_visibility', host: 'us', group_id: 'g_b', group_name: 'builders', member_count: 4, include_members: false, status: 'pending' },
    { id: 'i3', kind: 'group_link', host: 'us', group_id: 'g_b', group_name: 'builders', direction: 'in', mode: 'members->members',
      peer_group_id: 'g_a', peer_group_name: 'alpha', status: 'pending' },
  ],
};

async function mountProposal(t, proposal) {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const request = proposalRequest(proposal);
  const state = harness.signals.signal({ ...populated(), selected: 'access-requests', selectedMsgId: request.id, messages: [] });
  const controller = controllerFor(state);
  const decided = [];
  controller.decideProposal = async (...a) => { decided.push(a); };
  controller.messageView = () => ({ access: true, allAccess: [request], pendingAccess: [request], handledAccess: [], search: '' });
  controller.messageCountText = () => '1 pending';
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controller} />`);
  return { harness, mounted, root: mounted.container, decided };
}

test('a peer proposal shows member counts, never names, and accepts a ticked subset with the member list narrowed', async (t) => {
  const { harness, mounted, root, decided } = await mountProposal(t, aliceProposal);
  assert.match(root.textContent, /Proposal from alice/);
  assert.match(root.textContent, /you see alice:alpha \(5 members\)/);
  assert.match(root.textContent, /alice sees your builders \(4 members\)/);
  assert.match(root.textContent, /alice:alpha → your builders/);
  assert.equal(root.querySelector('#proposal-primary').textContent.trim(), 'Accept all');
  const membersTick = root.querySelector('[data-item="i1"] .proposal-members-tick');
  assert.ok(membersTick, 'a share including the member list can be narrowed');
  assertAbsent(root.querySelector('[data-item="i2"] .proposal-members-tick'));
  membersTick.checked = false;
  await harness.act(() => harness.fireEvent(membersTick, 'change'));
  const linkTick = root.querySelector('[data-item="i3"] .proposal-tick');
  linkTick.checked = false;
  await harness.act(() => harness.fireEvent(linkTick, 'change'));
  assert.equal(root.querySelector('#proposal-primary').textContent.trim(), 'Accept selected (2 of 3)');
  await harness.act(() => root.querySelector('#proposal-primary').click());
  assert.deepEqual(decided.at(-1), ['pp-1', { revision: 1, decision: 'accept', item_ids: ['i1', 'i2'], without_members: ['i1'] }]);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  await harness.act(() => [...root.querySelectorAll('.access-btn.deny')].at(-1).click());
  assert.deepEqual(decided.at(-1), ['pp-1', { revision: 1, decision: 'decline' }]);
  await mounted.unmount();
});

test('changing a link mode turns the reply into a counter-proposal with the full item list', async (t) => {
  const { harness, mounted, root, decided } = await mountProposal(t, aliceProposal);
  const mode = root.querySelector('[data-item="i3"] .proposal-mode');
  [...mode.options].forEach((option) => option.removeAttribute('selected'));
  [...mode.options].find((option) => option.value === 'owners->members').setAttribute('selected', '');
  await harness.act(() => harness.fireEvent(mode, 'change'));
  assert.match(root.querySelector('#proposal-primary').textContent, /Send counter-proposal \(3 items\)/);
  await harness.act(() => root.querySelector('#proposal-primary').click());
  assert.deepEqual(decided.at(-1), ['pp-1', { revision: 1, decision: 'counter', items: [
    { id: 'i1', kind: 'share_visibility', host: 'them', group_id: 'g_a', include_members: true },
    { id: 'i2', kind: 'share_visibility', host: 'us', group_id: 'g_b', include_members: false },
    { id: 'i3', kind: 'group_link', host: 'us', group_id: 'g_b', source: 'them', source_group_id: 'g_a', mode: 'owners->members' },
  ] }]);
  await mounted.unmount();
});

test('our own proposal waits on the peer and offers withdraw, naming accepted edges as staying', async (t) => {
  const ours = { ...aliceProposal, ours: true, awaiting: 'them',
    items: aliceProposal.items.map((item) => item.id === 'i3' ? { ...item, status: 'accepted' } : item) };
  const { mounted, root, decided, harness } = await mountProposal(t, ours);
  assert.match(root.textContent, /Proposal to alice/);
  assert.match(root.textContent, /waiting for alice's operator/);
  assertAbsent(root.querySelector('#proposal-primary'));
  const withdraw = root.querySelector('#proposal-withdraw');
  assert.equal(withdraw.textContent.trim(), 'Withdraw pending items');
  await harness.act(() => withdraw.click());
  assert.deepEqual(decided.at(-1), ['pp-1', { revision: 1, decision: 'withdraw' }]);
  await mounted.unmount();
});

test('an incoming link into the peer group reads from our group and offers no approve-future', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const request = { id: 'gl-3', purpose: 'group_link', origin_peer: 'inst_alice1', status: 'pending',
    proposal: { id: 'gl-3', ours: false, awaiting: 'us', state: 'sent', items: [] },
    link: { source_peer_label: 'alice', source_group: 'builders', dest_group: 'research', mode: 'members->members', direction: 'out' },
    created_at: '2026-07-12T00:00:00Z', deadline: '2026-07-13T00:00:00Z' };
  const state = harness.signals.signal({ ...populated(), selected: 'access-requests', selectedMsgId: 'gl-3', messages: [] });
  const controller = controllerFor(state);
  controller.messageView = () => ({ access: true, allAccess: [request], pendingAccess: [request], handledAccess: [], search: '' });
  controller.messageCountText = () => '1 pending';
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controller} />`);
  const root = mounted.container;
  assert.match(root.textContent, /builders → alice:research · members → members/);
  assert.deepEqual([...root.querySelectorAll('.access-reader-actions .access-btn')].map((b) => b.textContent.trim()), ['Decline', 'Approve']);
  await mounted.unmount();
});

test('paged sidebar lists offer Show more for the flat agent list and each expanded group', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const state = harness.signals.signal(populated());
  const controller = controllerFor(state);
  const box = (id) => ({ id, kind: 'agent', title: id, total: 1, unread: 0, in: 1, out: 0 });
  const calls = [];
  controller.loadMoreAgents = () => calls.push('agents');
  controller.loadMoreMembers = (name) => calls.push(`members:${name}`);
  controller.mailboxView = () => ({
    empty: false, hasRoster: true, pinned: state.value.mailboxes, prevGens: new Set(),
    filtering: false, agentsExpanded: true,
    groups: [
      { mailbox: { id: 'group:big', kind: 'group', title: 'big', members: 3, total: 3 }, expanded: true,
        members: [box('conv-a')], membersTotal: 3 },
      { mailbox: { id: 'group:small', kind: 'group', title: 'small', members: 1, total: 1 }, expanded: true,
        members: [box('conv-b')], membersTotal: 1 },
    ],
    agents: [box('conv-c')], agentsTotal: 4, agentsUnread: 0, moreAgents: 3,
  });
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controller} />`);
  const buttons = [...mounted.container.querySelectorAll('#mail-sidebar .mailbox-load-more')];
  assert.deepEqual(buttons.map((b) => b.textContent.trim()), ['Show more · 1 of 3', 'Show more · 1 of 4'],
    'one under the group with more members, one under the agent list; none under a complete group');
  assert.ok(buttons[0].classList.contains('nested'));
  for (const b of buttons) await harness.act(() => harness.fireEvent(b, 'click'));
  assert.deepEqual(calls, ['members:big', 'agents']);
  await mounted.unmount();
});

test('a paged sidebar list at the roster cap asks for the filter instead of offering Show more', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const state = harness.signals.signal(populated());
  const controller = controllerFor(state);
  const agents = Array.from({ length: 2000 }, (_, i) => ({ id: `conv-${i}`, kind: 'agent', title: `a${i}`, total: 1, unread: 0, in: 1, out: 0 }));
  controller.mailboxView = () => ({
    empty: false, hasRoster: true, pinned: state.value.mailboxes, prevGens: new Set(),
    filtering: false, agentsExpanded: true, groups: [],
    agents, agentsTotal: 5000, agentsUnread: 0, moreAgents: 3000,
  });
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controller} />`);
  const sidebar = mounted.container.querySelector('#mail-sidebar');
  assert.equal(sidebar.querySelector('button.mailbox-load-more'), null, 'no dead Show more at the cap');
  assert.match(sidebar.querySelector('.mailbox-load-more.capped').textContent, /2000 of 5000 shown · filter to narrow/);
  await mounted.unmount();
});

test('message list counts files, images, and videos separately in normal and wizard modes', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const state = harness.signals.signal(populated());
  state.value = { ...state.value, selectedMsgId: null, messages: [message(1, { attachments: [
    { filename: 'archive.zip', content_type: 'application/zip' },
    { filename: 'notes.md', content_type: 'text/markdown', markdown: true },
    { filename: 'photo.png', content_type: 'image/png', previewable: true },
    { filename: 'drawing.svg', content_type: 'image/svg+xml' },
    { filename: 'screen.png', previewable: true },
    { filename: 'clip.mp4', content_type: 'video/mp4', video: true },
    { filename: 'movie.bin', video: true },
    { filename: 'unplayable.mov', content_type: 'video/quicktime' },
  ] }), message(2)] };
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controllerFor(state)} />`);
  for (const wizard of [false, true]) {
    if (wizard) {
      harness.document.body.classList.add('wizard');
      await harness.act(() => { state.value = { ...state.value }; });
    }
    const rows = mounted.container.querySelectorAll('.mail-row');
    const indicator = rows[0].querySelector('.mail-row-attachments');
    assert.equal(indicator.getAttribute('aria-label'), '2 files, 3 images, 3 videos');
    assert.equal(indicator.getAttribute('title'), '2 files, 3 images, 3 videos');
    assert.deepEqual([...indicator.querySelectorAll('.mail-row-attachment-type')].map((node) => [
      node.dataset.type, node.querySelector('.mail-row-attachment-count').textContent,
    ]), [['file', '2'], ['image', '3'], ['video', '3']]);
    assert.equal(rows[1].querySelector('.mail-row-attachments'), null, 'no empty indicators');
  }
  await harness.act(() => { state.value = { ...state.value, messages: [message(1, {
    attachments: [{ filename: 'only.png', content_type: 'image/png' }],
  })] }; });
  const single = mounted.container.querySelector('.mail-row-attachments');
  assert.equal(single.getAttribute('aria-label'), '1 image');
  assert.equal(single.querySelectorAll('.mail-row-attachment-type').length, 1, 'omit zero types');
  assert.equal(single.querySelector('.mail-row-attachment-count').textContent, '1');
  await mounted.unmount();
});

test('attachment preview arrows stay inside the modal instead of moving Messages panes', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const state = harness.signals.signal(populated());
  const first = { id: 1, filename: 'one.png', url: '/one', previewable: true };
  const second = { id: 2, filename: 'two.png', url: '/two', previewable: true };
  state.value = { ...state.value, messages: [message(1, { attachments: [first, second] })] };
  const controller = controllerFor(state);
  const savedFetch = globalThis.fetch;
  globalThis.fetch = async () => ({ ok: true, status: 200 });
  t.after(() => { globalThis.fetch = savedFetch; });
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controller} />`);
  const settle = () => harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  await harness.act(() => mounted.container.querySelectorAll('.human-attachment-preview-trigger')[1].click());
  await settle();
  const close = mounted.container.querySelector('.image-preview-close');
  await harness.act(() => harness.fireEvent(close, 'keydown', { key: 'ArrowLeft', bubbles: true, cancelable: true }));
  await settle();
  assert.equal(mounted.container.querySelector('.image-preview-dialog h2').textContent, 'one.png');
  assertSameNode(harness.document.activeElement, close, 'pane traversal must not pull focus out of the preview');
  await mounted.unmount();
});

test('on a peer\'s page, Messages answers notifications through the peer and leaves deleting to its operator', async (t) => {
  const harness = await createPreactHarness(t);
  const { MailApp } = await harness.importDashboardModule('js/mail-island.js');
  const dialogs = await harness.importDashboardModule('js/message-access-dialog-controller.js');
  const replies = [];
  t.after(dialogs.registerMessageAccessDialogController({ openHumanReply: (context) => replies.push(context) }));
  const { dashboardState } = await harness.importDashboardModule('js/snapshot-store.js');
  const previousRemote = globalThis.__tclaudeRemoteNode;
  const previousFetch = globalThis.fetch;
  const posts = [];
  globalThis.__tclaudeRemoteNode = { id: 'inst_forge' };
  globalThis.fetch = async (url, init = {}) => { posts.push(`${init.method || 'GET'} ${url} ${init.body || ''}`); return { ok: true, status: 200, json: async () => ({}) }; };
  t.after(() => { globalThis.__tclaudeRemoteNode = previousRemote; globalThis.fetch = previousFetch; dashboardState.publishLocalEdit(null); });
  dashboardState.publishLocalEdit({ peer_view: { peer: 'desk', included: ['human.inbox.answer', 'node.messages'], omitted: [] } });
  const state = harness.signals.signal(populated());
  const mounted = await harness.mount(harness.html`<${MailApp} controller=${controllerFor(state)} />`);
  const c = mounted.container;

  // A notification: the same row as this node's own, answered through the
  // peer's routes; this node's (data-act) buttons, which would act on this
  // node's mail, are not offered.
  for (const act of ['msg-reply', 'msg-focus', 'msg-mark-read', 'msg-delete', 'msg-clear', 'msg-mark-all-read']) assertAbsent(c.querySelector(`[data-act="${act}"]`), act);
  const labels = (root) => [...root.querySelectorAll('button')].map((b) => b.textContent.trim());
  assert.deepEqual(labels(c.querySelector('#mail-reader .mail-reader-actions')).slice(0, 3), ['reply', 'focus', 'mark read']);
  assert.ok(c.querySelector('#mail-reader .mail-download'), 'the download menu is there');
  assert.ok(c.querySelector('[data-human="focus"]').disabled && c.querySelector('[data-human="delete"]').disabled);
  // The bulk bar is the local one, deleting greyed.
  const bar = c.querySelector('#mail-bulk-bar');
  assert.equal(bar.hidden, false);
  assert.ok(labels(bar).some((l) => /mark all read/.test(l)) && labels(bar).some((l) => /delete read/.test(l)));
  assert.ok([...bar.querySelectorAll('button')].filter((b) => /delete/.test(b.textContent)).every((b) => b.disabled), 'no deleting the peer\'s notifications');
  assert.equal(c.querySelector('.mail-row-del').disabled, true);
  await harness.act(async () => { harness.fireEvent(c.querySelector('#mail-mark-all'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(posts.includes('POST /api/human-inbox/read {"all":true}'), posts.join('\n'));
  await harness.act(() => harness.fireEvent(c.querySelector('[data-human="reply"]'), 'click'));
  assert.equal(replies.length, 1);
  await harness.act(async () => { await replies[0].send('ack'); });
  assert.ok(posts.includes('POST /api/human-inbox/reply {"id":1,"body":"ack"}'), posts.join('\n'));
  await harness.act(async () => { harness.fireEvent(c.querySelector('[data-human="mark"]'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(posts.includes('POST /api/human-inbox/read {"id":1}'));

  // Without the peer's answer grant the notification is read-only here.
  await harness.act(() => { dashboardState.publishLocalEdit({ peer_view: { peer: 'desk', included: ['node.messages'], omitted: [] } }); });
  assert.ok(c.querySelector('[data-human="reply"]').disabled && c.querySelector('[data-human="mark"]').disabled);
  assert.match(c.querySelector('[data-human="reply"]').title, /for reading only/);
  // node.messages.mark-read lets this operator mark them read (as opening one
  // does), without answering.
  await harness.act(() => { dashboardState.publishLocalEdit({ peer_view: { peer: 'desk', included: ['node.messages', 'node.messages.mark-read'], omitted: [] } }); });
  assert.ok(c.querySelector('[data-human="reply"]').disabled);
  assert.equal(c.querySelector('[data-human="mark"]').disabled, false);
  assert.equal(c.querySelector('#mail-mark-all').disabled, false);

  // Agent mail: read state goes to the peer; deleting stays its operator's.
  await harness.act(() => { state.value = { ...state.value, selected: 'conv-a' }; });
  const reader = c.querySelector('#mail-reader');
  assert.ok([...reader.querySelectorAll('button')].some((b) => b.textContent === 'mark read' && !b.dataset.act));
  assert.ok([...reader.querySelectorAll('button')].filter((b) => b.textContent === 'delete').every((b) => b.disabled));
  assert.ok([...c.querySelectorAll('#mail-bulk-bar button')].filter((b) => /delete selected/.test(b.textContent)).every((b) => b.disabled));
  await mounted.unmount();
});
