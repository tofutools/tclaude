import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('a restricted peer\'s page attaches its unread notifications at their own cadence', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-notifications.js');
  mod.resetPageNotificationsForTest();
  const asked = [];
  let answer = { messages: [{ id: 3, from_agent: 'agt_peerworker', read: false }, { id: 2, read: true }], total: 4 };
  const fetchImpl = async (url) => { asked.push(url); return { ok: true, json: async () => answer }; };
  const snap = (view) => ({ messages: null, peer_view: view });
  const shares = { level: 'restricted', included: ['node.messages'], omitted: [] };
  const settle = () => new Promise((r) => setTimeout(r, 0));

  assert.equal(mod.attachPageNotifications(snap(shares), { remote: '', fetchImpl }).messages, null, 'this node\'s page: untouched');
  assert.equal(mod.attachPageNotifications(snap({ level: 'restricted', included: [], omitted: [] }), { remote: 'inst_forge', fetchImpl }).messages, null, 'a peer sharing none is not asked');
  assert.deepEqual(asked, []);
  const full = { messages: [{ id: 9 }], peer_view: { level: 'unrestricted' } };
  assert.deepEqual(mod.attachPageNotifications(full, { remote: 'inst_forge', fetchImpl }).messages, [{ id: 9 }], 'an unrestricted peer\'s own snapshot carries them');

  assert.equal(mod.attachPageNotifications(snap(shares), { remote: 'inst_forge', now: 1000, fetchImpl }).messages, null, 'first tick: asked, nothing yet');
  await settle();
  assert.deepEqual(asked, ['/api/mailbox?id=human&unread=1&page=1&page_size=100']);
  const next = mod.attachPageNotifications(snap(shares), { remote: 'inst_forge', now: 3000, fetchImpl });
  assert.deepEqual(next.messages.map((m) => m.id), [3], 'unread ones only');
  assert.equal(next.messages_unread, 4, 'the count is the peer\'s total');
  assert.equal(asked.length, 1, 'not again within the cadence');

  // Marked read from here: it leaves at once, and the next tick reads anew.
  mod.refreshPageNotifications(3, true);
  answer = { messages: [], total: 0 };
  const after = mod.attachPageNotifications(snap(shares), { remote: 'inst_forge', now: 4000, fetchImpl });
  assert.deepEqual(after.messages, []);
  assert.equal(after.messages_unread, 3);
  await settle();
  assert.equal(asked.length, 2);
  assert.equal(mod.attachPageNotifications(snap(shares), { remote: 'inst_forge', now: 5000, fetchImpl }).messages_unread, 0);
});
