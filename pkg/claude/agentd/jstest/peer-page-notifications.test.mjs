import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('a restricted peer\'s page attaches its unread notifications at their own cadence', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-notifications.js');
  mod.resetPageNotificationsForTest();
  const asked = [];
  let answer = { messages: [{ id: 3, from_agent: 'agt_peerworker', read: false }, { id: 2, read: true }], total: 4 };
  const fetchImpl = async (url) => { asked.push(url); const body = await answer; return { ok: true, json: async () => body }; };
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

  // Marked read from here: shown read at once (the ! and the count drop, a
  // reader holding the row sees it read), and the next tick reads anew.
  mod.refreshPageNotifications(3, true);
  answer = { messages: [], total: 3 };
  const after = mod.attachPageNotifications(snap(shares), { remote: 'inst_forge', now: 4000, fetchImpl });
  assert.deepEqual(after.messages.map((m) => [m.id, m.read]), [[3, true]]);
  assert.equal(after.messages_unread, 3);
  after.messages[0].read = false;
  await settle();
  assert.equal(asked.length, 2);
  const read = mod.attachPageNotifications(snap(shares), { remote: 'inst_forge', now: 5000, fetchImpl });
  assert.deepEqual(read.messages, [], 'the peer\'s answer, and a consumer\'s edit never reached the cache');
  assert.equal(read.messages_unread, 3);

  // Marked unread: it shows again at once, ahead of the read.
  mod.refreshPageNotifications(3, false, { id: 3, from_agent: 'agt_peerworker', read: true });
  // That read is answered from before a second write: dropped.
  let release;
  answer = new Promise((r) => { release = r; });
  const pending = mod.attachPageNotifications(snap(shares), { remote: 'inst_forge', now: 6000, fetchImpl });
  assert.deepEqual(pending.messages.map((m) => [m.id, m.read]), [[3, false]]);
  assert.equal(pending.messages_unread, 4);
  mod.refreshPageNotifications(3, true);
  release({ messages: [{ id: 3, read: false }], total: 4 });
  await settle(); await settle();
  const kept = mod.attachPageNotifications(snap(shares), { remote: 'inst_forge', now: 6500, fetchImpl });
  assert.deepEqual(kept.messages.map((m) => [m.id, m.read]), [[3, true]], 'the stale answer is not shown');
  assert.equal(asked.length, 4, 'and the next tick reads anew');
});
