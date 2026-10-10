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
  const opts = { remote: '', fleet, hidden: false };
  assert.deepEqual(m.claimRemoteInboxRead(0, { ...opts, remote: 'inst_lab' }), [], 'a peer view never reads');
  assert.deepEqual(m.claimRemoteInboxRead(0, opts).map((p) => p.id), ['inst_lab'], 'restricted and offline nodes are not asked');
  assert.deepEqual(m.claimRemoteInboxRead(30_000, opts), []);
  assert.equal(m.claimRemoteInboxRead(60_000, opts).length, 1);
  assert.deepEqual(m.claimRemoteInboxRead(200_000, { ...opts, hidden: true }), [], 'hidden: every five minutes');
  assert.equal(m.claimRemoteInboxRead(360_000, { ...opts, hidden: true }).length, 1);

  const urls = [];
  await m.readRemoteInboxes([fleet.peers[0]], async (url) => { urls.push(url); return { ok: true, json: async () => ({
    messages: [{ id: 1, subject: 'old', read: true }, { id: 2, subject: 'new', read: false }],
    access_requests: [{ id: 'r1', perm: 'human.clipboard' }] }) }; });
  assert.deepEqual(urls, ['/api/peer/inst_lab/human-inbox']);
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
