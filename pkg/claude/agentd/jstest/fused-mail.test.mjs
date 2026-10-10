import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const self = { id: 'inst_self', name: 'desk', color: '#58a6ff', local: true, online: true };
const forge = { id: 'inst_forge', name: 'forge', color: '#f0883e', local: false, online: true, level: 'unrestricted' };
const lab = { id: 'inst_lab', name: 'lab', color: '#39c5cf', local: false, online: true, level: 'restricted' };

test('the model routes reads and actions to the owning node and merges newest first', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/fused-mail-model.js');
  assert.equal(m.folderURL(self, 'all'), '/api/mailbox?id=all&page=1&page_size=50');
  assert.equal(m.folderURL(forge, 'group:ops', { q: 'x', size: 500 }), '/api/peer/inst_forge/mailbox?id=group%3Aops&page=1&page_size=200&q=x');
  assert.equal(m.ownerRoutes(self).decide('7'), '/api/access-requests/7/decision');
  assert.equal(m.ownerRoutes(forge).decide('7'), '/api/peer/inst_forge/human-inbox/access/7');
  assert.equal(m.ownerRoutes(lab).message, '/api/peer/inst_lab/operator-message');
  assert.equal(m.canActOnHuman(self), true); assert.equal(m.canActOnHuman(forge), true); assert.equal(m.canActOnHuman(lab), false);
  assert.equal(m.readFailure(403, { code: 'permission' }).kind, 'not_shared');
  assert.equal(m.readFailure(403, { code: 'not_trusted' }).kind, 'gone');
  assert.equal(m.readFailure(502, { code: 'peer_unreachable' }).label, 'offline');
  const rows = m.mergeMessages([
    { node: self, kind: 'agent', messages: [{ id: 1, created_at: '2026-01-01T00:00:01Z' }] },
    { node: forge, kind: 'agent', messages: [{ id: 1, created_at: '2026-01-01T00:00:02Z' }] },
  ]);
  assert.deepEqual(rows.map((r) => r.key), ['inst_forge/agent/1', 'inst_self/agent/1'], 'same ID on two nodes stays two messages');
  assert.deepEqual(m.mergeRequests([{ node: self, requests: [{ id: 'a', created_at: '2026-01-02T00:00:00Z' }, { id: 'b', status: 'approved' }] }, { node: forge, requests: [{ id: 'c', created_at: '2026-01-01T00:00:00Z' }] }]).map((r) => r.key), ['inst_forge/access/c', 'inst_self/access/a']);
  assert.equal(m.replyTarget({ from_agent: 'agt_abcd1234' }), 'agt_abcd1234');
  assert.equal(m.replyTarget({ to_agent: 'agt_abcd1234', operator_authored: true }), 'agt_abcd1234');
  assert.equal(m.replyTarget({ from_agent: 'agt_<b>' }), '');
});

function fakeTimers() {
  const queue = []; let seq = 0;
  return { queue, setTimeout(fn, ms) { const id = ++seq; queue.push({ id, fn, ms }); return id; }, clearTimeout(id) { const i = queue.findIndex((q) => q.id === id); if (i >= 0) queue.splice(i, 1); } };
}
const ok = (data) => ({ ok: true, status: 200, json: async () => data });
const fail = (status, data) => ({ ok: false, status, json: async () => data });

test('fused Messages lists every ticked node\'s mail, answers on the owning node, and falls back to opening the node', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/skynet-state.js'), harness.importDashboardModule('js/fused-mail.js')]);
  const activeTab = harness.signals.signal('messages');
  const state = stateMod.createSkynetState({ activeTab, search: '?nodes=all' });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [
    { instance_id: 'inst_forge', label: 'forge', trusted: true, online: true, level: 'unrestricted' },
    { instance_id: 'inst_lab', label: 'lab', trusted: true, online: true, level: 'restricted' },
  ] });
  const snapshot = harness.signals.signal({ access_requests: [{ id: 'r1', perm: 'sudo.x', title: 'local-agent', created_at: '2026-01-01T00:00:00Z' }] });
  const timers = fakeTimers(); const calls = []; const toasts = []; const opened = []; const confirms = [];
  const messages = {
    '/api/': [{ id: 1, from_agent: 'agt_desk0001', from_title: 'desk-bot', subject: 'local hello', body: '<b>raw</b>', created_at: '2026-01-01T00:00:01Z' }],
    '/api/peer/inst_forge/': [{ id: 1, from_agent: 'agt_forge001', from_title: 'forge-bot', subject: 'forge hello', body: 'hi', created_at: '2026-01-01T00:00:03Z' }],
    '/api/peer/inst_lab/': [{ id: 9, from_agent: 'agt_lab00001', from_title: 'lab-bot', subject: 'lab hello', body: 'yo', created_at: '2026-01-01T00:00:02Z' }],
  };
  const respond = async (url, init = {}) => {
    calls.push(init.method === 'POST' ? `POST ${url} ${init.body}` : url);
    if (init.method === 'POST') return url.startsWith('/api/peer/inst_lab/') ? fail(403, { error: 'missing message.direct', code: 'permission' }) : ok({ ok: true });
    const base = Object.keys(messages).sort((a, b) => b.length - a.length).find((b) => url.startsWith(b));
    if (url.endsWith('mailboxes')) return ok({ mailboxes: [{ id: 'group:ops', kind: 'group', title: 'ops', total: 1, unread: 1 }] });
    if (url.endsWith('human-inbox')) return ok({ messages: [], access_requests: [{ id: 'r2', perm: 'agents.spawn', title: 'forge-agent', created_at: '2026-01-02T00:00:00Z' }] });
    return ok({ messages: messages[base], total: 1 });
  };
  const fetchImpl = (url, init) => respond(url, init);
  const localFetch = (url, init) => respond(url, init);
  const confirm = async (o) => { confirms.push(o); await o.action(); return true; };
  const mounted = await harness.mount(harness.html`<${island.FusedMail} state=${state} snapshot=${snapshot} fetchImpl=${fetchImpl} localFetch=${localFetch}
    timers=${timers} now=${() => 0} remote="" toast=${(msg, err) => toasts.push([msg, err])} confirm=${confirm} openOnNode=${(n) => opened.push(n.id)} />`);
  const c = mounted.container;
  const once = async () => { const q = timers.queue.splice(0); await harness.act(async () => { for (const x of q) if (x.ms !== 300) await x.fn(); }); };
  await once();
  const subjects = [...c.querySelectorAll('.mail-row-subject')].map((s) => s.textContent);
  assert.deepEqual(subjects, ['forge hello', 'lab hello', 'local hello'], 'newest first across nodes');
  assert.deepEqual([...c.querySelectorAll('.mail-row .scope-pill')].map((p) => p.textContent), ['forge', 'lab', '⌂ desk']);
  assert.ok(calls.includes('/api/mailbox?id=all&page=1&page_size=50'), 'this node read through localFetch');
  assert.ok(calls.includes('/api/peer/inst_lab/mailbox?id=all&page=1&page_size=50'));
  assert.equal(c.querySelectorAll('.fused-mail-section').length, 3, 'each node\'s folders under its own pill');

  // A reply to forge's agent goes to forge's operator-message route.
  await harness.act(() => harness.fireEvent(c.querySelector('.mail-row-wrap[data-key="inst_forge/agent/1"] .mail-row'), 'click'));
  const reply = c.querySelector('.fused-mail-reply');
  await harness.act(() => { reply.value = 'thanks'; harness.fireEvent(reply, 'input'); });
  await harness.act(async () => { harness.fireEvent(c.querySelector('[data-fused="reply"]'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(calls.includes('POST /api/peer/inst_forge/operator-message {"to":"agt_forge001","subject":"Re: forge hello","body":"thanks"}'));

  // The local body is text, never HTML.
  await harness.act(() => harness.fireEvent(c.querySelector('.mail-row-wrap[data-key="inst_self/agent/1"] .mail-row'), 'click'));
  assert.equal(c.querySelector('#fused-mail-reader pre').textContent, '<b>raw</b>');
  assert.equal(c.querySelector('#fused-mail-reader b b'), null);

  // lab refuses the reply: the toast says so and Open on lab is there.
  await harness.act(() => harness.fireEvent(c.querySelector('.mail-row-wrap[data-key="inst_lab/agent/9"] .mail-row'), 'click'));
  const labReply = c.querySelector('.fused-mail-reply');
  await harness.act(() => { labReply.value = 'x'; harness.fireEvent(labReply, 'input'); });
  await harness.act(async () => { harness.fireEvent(c.querySelector('[data-fused="reply"]'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.match(toasts.at(-1)[0], /lab does not allow it from here.*Open it on lab/);
  await harness.act(() => harness.fireEvent(getByRole(c, 'button', { name: 'Open on lab' }), 'click'));
  assert.deepEqual(opened, ['inst_lab']);

  // Access requests: this node's from the snapshot, forge's (unrestricted)
  // through its human-inbox; lab is never asked.
  calls.length = 0;
  await harness.act(() => harness.fireEvent(getByRole(c, 'button', { name: /Access requests/ }), 'click'));
  await once();
  assert.ok(calls.includes('/api/peer/inst_forge/human-inbox'));
  assert.ok(!calls.some((u) => u.includes('inst_lab/human-inbox')));
  assert.deepEqual([...c.querySelectorAll('.mail-row-subject')].map((s) => s.textContent), ['🔐 sudo.x', '🔐 agents.spawn']);
  await harness.act(() => harness.fireEvent(c.querySelector('.mail-row-wrap[data-key="inst_forge/access/r2"] .mail-row'), 'click'));
  await harness.act(async () => { harness.fireEvent(c.querySelector('[data-fused="approve"]'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.match(confirms[0].body, /Approves agents\.spawn once for forge-agent on forge/);
  assert.ok(calls.includes('POST /api/peer/inst_forge/human-inbox/access/r2 {"decision":"approve"}'));
  await mounted.unmount(); state.dispose();
});
