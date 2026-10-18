// tcl-99b5ir gap 2: the human inbox of the operator's other own nodes in the
// Messages sidebar — read at a relaxed cadence from unrestricted, online
// nodes only, answered once, and rendered as text.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const fleet = { self: { id: 'inst_self' }, peers: [
  { id: 'inst_lab', name: 'lab', level: 'unrestricted', online: true },
  { id: 'inst_forge', name: 'forge', level: 'restricted', online: true },
  { id: 'inst_off', name: 'attic', level: 'unrestricted', online: false },
] };

async function load(t) {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/remote-inbox.js');
  m.resetRemoteInboxForTest();
  t.after(() => m.resetRemoteInboxForTest());
  return { harness, m };
}

test('reads ride the snapshot tick: own dashboard, unrestricted online nodes, every minute (five when hidden)', async (t) => {
  const { m } = await load(t);
  // The peer's trust in this node decides, not this node's trust in it.
  const theirs = { inst_lab: { level: 'unrestricted' }, inst_forge: { level: 'restricted', omitted: [{ feature: 'node.messages', requires: 'node.messages.read' }] } };
  const opts = { remote: '', fleet, hidden: false, viewOf: (id) => theirs[id] || null };
  assert.deepEqual(m.claimRemoteInboxRead(0, { ...opts, remote: 'inst_lab' }), [], 'a peer view never reads');
  assert.deepEqual(m.claimRemoteInboxRead(0, opts).map((p) => p.id), ['inst_lab'], 'a node sharing nothing and an offline one are not asked');
  assert.deepEqual(m.claimRemoteInboxRead(30_000, opts), []);
  assert.equal(m.claimRemoteInboxRead(60_000, opts).length, 1);
  assert.deepEqual(m.claimRemoteInboxRead(200_000, { ...opts, hidden: true }), [], 'hidden: every five minutes');
  assert.equal(m.claimRemoteInboxRead(360_000, { ...opts, hidden: true }).length, 1);

  const urls = [];
  await m.readRemoteInboxes([fleet.peers[0]], async (url) => { urls.push(url); return { ok: true, json: async () => (url.includes('/mailbox?') ? { peer_view: { level: 'unrestricted' } } : {
    messages: [{ id: 1, subject: 'old', read: true }, { id: 2, subject: 'new', read: false }],
    access_requests: [{ id: 'r1', perm: 'human.clipboard' }] }) }; });
  assert.deepEqual(urls, ['/api/peer/inst_lab/mailbox?id=human&unread=1&page=1&page_size=50', '/api/peer/inst_lab/human-inbox'], 'not heard from yet: its folder says it trusts this node unrestricted');
  assert.deepEqual(m.remoteInbox.value.inst_lab.messages.map((x) => x.id), [2], 'only unread notifications');
  await m.readRemoteInboxes([fleet.peers[0]], async () => ({ ok: false, status: 502, json: async () => ({ error: 'peer is unreachable' }) }));
  assert.equal(m.remoteInbox.value.inst_lab.access_requests.length, 1, 'a failed read keeps the last items');
  assert.match(m.remoteInbox.value.inst_lab.error, /unreachable/);
  m.claimRemoteInboxRead(1e9, { ...opts, fleet: { peers: [] } });
  assert.deepEqual(m.remoteInbox.value, {}, 'a node that is no longer an unrestricted peer is forgotten');
});

test('the sidebar lists items @node as text; the reader replies, marks read, and approves once with the consequence spelled out', async (t) => {
  const { harness, m } = await load(t);
  const calls = []; const confirms = []; const toasts = [];
  const actions = {
    reply: async (...a) => { calls.push(['reply', ...a]); return {}; },
    markRead: async (...a) => { calls.push(['read', ...a]); return {}; },
    decide: async (...a) => { calls.push(['decide', ...a]); return {}; },
  };
  const confirm = async (o) => { confirms.push(o); return o.action ? o.action() : true; };
  m.remoteInbox.value = { inst_lab: { label: 'lab', error: '',
    messages: [{ id: 7, subject: '<img src=x onerror=alert(1)>', body: 'need a decision', from_title: 'worker', replyable: true, attachments: ['plot.png'] }],
    access_requests: [{ id: 'r1', perm: 'human.clipboard', title: 'worker', path: '/v1/clipboard' }] } };
  const mounted = await harness.mount(harness.html`<${m.RemoteInboxSection} inbox=${m.remoteInbox.value} actions=${actions} confirm=${confirm} toast=${(x, e) => toasts.push([x, e])} />`);
  const q = (s) => harness.document.querySelector(s);
  const settle = () => harness.act(() => new Promise((r) => setTimeout(r, 25)));
  assert.equal(q('#remote-inbox img'), null, 'a hostile subject is text');
  assert.match(q('[data-message="7"]').textContent, /<img src=x.*@lab/);
  assert.match(q('[data-request="r1"]').textContent, /🔐 human\.clipboard @lab/);

  await harness.act(() => q('[data-message="7"]').click());
  assert.match(q('#remote-inbox-reader').textContent, /Attachments stay on lab: plot\.png/);
  assert.equal(q('#remote-inbox-send').disabled, true, 'nothing to send yet');
  const ta = q('#remote-inbox-reply'); ta.value = 'go ahead';
  await harness.act(() => harness.fireEvent(ta, 'input'));
  await harness.act(() => q('#remote-inbox-send').click()); await settle();
  assert.deepEqual(calls.at(-1), ['reply', 'inst_lab', 7, 'go ahead']);
  assert.equal(q('#remote-inbox-reader'), null);

  await harness.act(() => q('[data-request="r1"]').click());
  assert.match(q('#remote-inbox-reader').textContent, /One answer for this one request/);
  await harness.act(() => q('#remote-inbox-approve').click()); await settle();
  assert.match(confirms.at(-1).body, /Approves human\.clipboard once for worker on lab: the blocked call \/v1\/clipboard goes through now, with all its effects on lab\. No "always allow" rule is written/);
  assert.deepEqual(calls.at(-1), ['decide', 'inst_lab', 'r1', 'approve']);
  assert.match(toasts.at(-1)[0], /Approved once on lab/);
  await mounted.unmount();
});

test('the inbox actions hit the peer proxy routes, one-shot only', async (t) => {
  const { m } = await load(t);
  const sent = [];
  const a = m.createRemoteInboxActions({ fetchImpl: async (url, init) => { sent.push([url, JSON.parse(init.body)]); return { ok: true, json: async () => ({}) }; } });
  await a.reply('inst_lab', 7, 'hi'); await a.markRead('inst_lab', 7); await a.decide('inst_lab', 'r/1', 'always');
  assert.deepEqual(sent, [
    ['/api/peer/inst_lab/human-inbox/reply', { id: 7, body: 'hi' }],
    ['/api/peer/inst_lab/human-inbox/read', { id: 7 }],
    ['/api/peer/inst_lab/human-inbox/access/r%2F1', { decision: 'deny' }],
  ], 'anything but approve is a deny');
});

test('a peer we trust unrestricted that does not trust us back is not asked for its human-inbox (tcl-mqlprj)', async (t) => {
  const { m } = await load(t);
  const peers = [{ id: 'inst_mine', name: 'mine', level: 'unrestricted', online: true }];
  const unknown = { fleet: { peers }, remote: '', hidden: false, viewOf: () => null };
  const none = { level: 'restricted', omitted: [{ feature: 'node.messages', requires: 'node.messages.read' }] };
  assert.deepEqual(m.claimRemoteInboxRead(0, { ...unknown, viewOf: () => none }), [], 'it shares nothing: never asked');
  assert.equal(m.claimRemoteInboxRead(0, unknown).length, 1, 'not heard from yet: asked once');
  const urls = [];
  const refuse = async (url) => { urls.push(url); return { ok: false, status: 403, json: async () => ({ code: 'permission', error: 'requires node.messages.read' }) }; };
  await m.readRemoteInboxes(peers, refuse, 0);
  assert.deepEqual(urls, ['/api/peer/inst_mine/mailbox?id=human&unread=1&page=1&page_size=50'], 'its grantable folder first, never human-inbox');
  assert.deepEqual(m.claimRemoteInboxRead(60_000, unknown), [], 'a refusal is not repeated every minute');
  assert.equal(m.claimRemoteInboxRead(m.INBOX_REFUSED_RETRY_MS + 1, unknown).length, 1, 'only after a long while');
});

test('a peer granting node.messages.read shares its notifications through its mailbox; requests and replies stay unrestricted', async (t) => {
  const { harness, m } = await load(t);
  const peers = [{ id: 'inst_lab', name: 'lab', level: 'restricted', online: true }];
  const urls = [];
  const view = { level: 'restricted', included: ['node.messages'], omitted: [{ feature: 'node.messages.mark-read', requires: 'node.messages.mark-read' }] };
  await m.readRemoteInboxes(peers, async (url) => { urls.push(url); return { ok: true, json: async () => ({ peer_view: view,
    messages: [{ id: 3, subject: 'build done', body: 'ok', read: false, attachments: [{ filename: 'log.txt' }] }, { id: 2, read: true }] }) }; });
  assert.deepEqual(urls, ['/api/peer/inst_lab/mailbox?id=human&unread=1&page=1&page_size=50']);
  const lab = m.remoteInbox.value.inst_lab;
  assert.deepEqual(lab.messages.map((x) => [x.id, x.replyable, x.attachments]), [[3, false, ['log.txt']]]);
  assert.deepEqual([lab.access_requests, lab.answers, lab.markable], [[], false, false]);
  const mounted = await harness.mount(harness.html`<${m.RemoteInboxSection} inbox=${m.remoteInbox.value} actions=${{}} confirm=${async () => true} toast=${() => {}} />`);
  await harness.act(() => harness.document.querySelector('[data-message="3"]').click());
  assert.match(harness.document.querySelector('#remote-inbox-reader').textContent, /Answer this one on lab's own dashboard/);
  assert.equal(harness.document.querySelector('#remote-inbox-read'), null, 'no mark-read grant: no Mark read');
  await mounted.unmount();

  // Once it trusts this node unrestricted, its human-inbox (and requests) is read.
  urls.length = 0;
  await m.readRemoteInboxes(peers, async (url) => { urls.push(url); return { ok: true, json: async () => (url.includes('mailbox')
    ? { peer_view: { level: 'unrestricted' }, messages: [] } : { messages: [], access_requests: [{ id: 'r1', perm: 'agents.spawn' }] }) }; });
  assert.deepEqual(urls, ['/api/peer/inst_lab/mailbox?id=human&unread=1&page=1&page_size=50', '/api/peer/inst_lab/human-inbox']);
  assert.equal(m.remoteInbox.value.inst_lab.access_requests.length, 1);
  urls.length = 0;
  await m.readRemoteInboxes(peers, async (url) => { urls.push(url); return { ok: true, json: async () => ({ messages: [], access_requests: [] }) }; });
  assert.deepEqual(urls, ['/api/peer/inst_lab/human-inbox'], 'then straight to human-inbox');
});
