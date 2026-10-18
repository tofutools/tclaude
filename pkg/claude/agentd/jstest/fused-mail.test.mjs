import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const self = { id: 'inst_self', name: 'desk', color: '#58a6ff', local: true, online: true };
const forge = { id: 'inst_forge', name: 'forge', color: '#f0883e', local: false, online: true, level: 'unrestricted' };
const lab = { id: 'inst_lab', name: 'lab', color: '#39c5cf', local: false, online: true, level: 'restricted' };

test('the model routes reads and actions to the owning node and merges newest first', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/fused-mail-model.js');
  assert.equal(m.folderURL(self, 'all'), '/api/mailbox?id=all&page=1&page_size=50');
  assert.equal(m.folderURL(forge, 'group:ops', { q: 'x', size: 900, retired: true }), '/api/peer/inst_forge/mailbox?id=group%3Aops&page=1&page_size=500&q=x&include_retired=1');
  assert.equal(m.folderURL(self, 'human', { page: 3, size: 25 }), '/api/mailbox?id=human&page=3&page_size=25');
  assert.equal(m.rosterURL(self), '/api/mailboxes?agents_limit=100&hide_prev_gens=1');
  assert.equal(m.rosterURL(forge, { limit: 200, q: 'bot', retired: true, empty: true, prevGens: true, expand: ['ops', 'qa'], memberLimits: { qa: 200 } }),
    '/api/peer/inst_forge/mailboxes?agents_limit=200&q=bot&include_retired=1&include_empty=1&expand=ops&expand=qa&members_limit=200%3Aqa');
  assert.equal(m.mergedDepth(2, 50), 100);
  assert.equal(m.mergedDepth(9, 100), 500, 'never deeper than the largest page a node serves');
  assert.equal(m.mergedPages(120, 50), 3);
  assert.equal(m.mergedPages(5000, 200), 2, 'a merge stops where its depth does');
  assert.equal(m.mergedPages(0, 50), 1);
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
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fused-mail.js')]);
  const activeTab = harness.signals.signal('messages');
  const state = stateMod.createFleetState({ activeTab, search: '?nodes=all' });
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
    if (/\/mailboxes(\?|$)/.test(url)) return ok({ mailboxes: [{ id: 'group:ops', kind: 'group', title: 'ops', total: 1, unread: 1 }] });
    if (url.endsWith('human-inbox')) return ok({ messages: [], access_requests: [{ id: 'r2', perm: 'agents.spawn', title: 'forge-agent', created_at: '2026-01-02T00:00:00Z' }] });
    return ok({ messages: messages[base], total: 1 });
  };
  const fetchImpl = (url, init) => respond(url, init);
  const localFetch = (url, init) => respond(url, init);
  const confirm = async (o) => { confirms.push(o); await o.action(); return true; };
  const mounted = await harness.mount(harness.html`<${island.FusedMail} state=${state} snapshot=${snapshot} fetchImpl=${fetchImpl} localFetch=${localFetch}
    timers=${timers} now=${() => 0} remote="" toast=${(msg, err) => toasts.push([msg, err])} confirm=${confirm} openOnNode=${(n) => opened.push(n.id)} />`);
  const c = mounted.container;
  const once = async () => { const q = timers.queue.splice(0); await harness.act(async () => { for (const x of q) await x.fn(); }); };
  await once();
  const subjects = [...c.querySelectorAll('.mail-row-subject')].map((s) => s.textContent);
  assert.deepEqual(subjects, ['forge hello', 'lab hello', 'local hello'], 'newest first across nodes');
  assert.deepEqual([...c.querySelectorAll('.mail-row .scope-pill')].map((p) => p.textContent), ['forge', 'lab', '⌂ desk']);
  assert.ok(calls.includes('/api/mailbox?id=all&page=1&page_size=50'), 'this node read through localFetch');
  assert.ok(calls.includes('/api/peer/inst_lab/mailbox?id=all&page=1&page_size=50'));
  assert.equal(c.querySelectorAll('.fused-mail-section').length, 3, 'each node\'s folders under its own pill');

  // A reply to forge's agent goes to forge's operator-message route.
  await harness.act(() => harness.fireEvent(c.querySelector('.mail-row-wrap[data-key="inst_forge/agent/1"] .mail-row'), 'click'));
  const reply = c.querySelector('.mail-reply-text');
  await harness.act(() => { reply.value = 'thanks'; harness.fireEvent(reply, 'input'); });
  await harness.act(async () => { harness.fireEvent(c.querySelector('[data-fused="reply"]'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(calls.includes('POST /api/peer/inst_forge/operator-message {"to":"agt_forge001","subject":"Re: forge hello","body":"thanks"}'));

  // The local body is text, never HTML.
  await harness.act(() => harness.fireEvent(c.querySelector('.mail-row-wrap[data-key="inst_self/agent/1"] .mail-row'), 'click'));
  assert.equal(c.querySelector('#fused-mail-reader .mail-reader-body').textContent, '<b>raw</b>');
  assert.equal(c.querySelector('#fused-mail-reader').tagName, 'DIV', 'a bare <section> is a hidden tab panel in the dashboard CSS');
  assert.equal(c.querySelector('#fused-mail-reader b b'), null);

  // lab refuses the reply: the toast says so and Open on lab is there.
  await harness.act(() => harness.fireEvent(c.querySelector('.mail-row-wrap[data-key="inst_lab/agent/9"] .mail-row'), 'click'));
  const labReply = c.querySelector('.mail-reply-text');
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

test('switching folders never shows the old folder\'s rows; human actions use each node\'s own routes; lost trust drops a node', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island, dialogs] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fused-mail.js'),
    harness.importDashboardModule('js/message-access-dialog-controller.js')]);
  const replies = [];
  t.after(dialogs.registerMessageAccessDialogController({ openHumanReply: (context) => replies.push(context) }));
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('messages'), search: '?nodes=all' });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true, level: 'unrestricted' }] });
  const timers = fakeTimers(); const calls = [];
  let forgeTrusted = true;
  const respond = async (url, init = {}) => {
    calls.push(init.method === 'POST' ? `POST ${url} ${init.body}` : url);
    if (init.method === 'POST') return ok({ ok: true });
    if (url.startsWith('/api/peer/inst_forge/') && !forgeTrusted) return fail(403, { error: 'not trusted', code: 'not_trusted' });
    if (/\/mailboxes(\?|$)/.test(url)) return ok({ mailboxes: [] });
    if (url.endsWith('human-inbox')) return ok({ messages: [], access_requests: [] });
    const human = url.includes('id=human');
    const from = url.startsWith('/api/peer/') ? 'forge' : 'desk';
    return ok({ messages: [{ id: 5, from_agent: 'agt_abcd1234', from_conv: 'c0ffee00-0000-4000-8000-000000000001', subject: `${from} ${human ? 'human' : 'agent'} note`, body: 'b', created_at: '2026-01-01T00:00:00Z', replyable: true,
      attachments: human ? [{ id: 7, filename: 'notes.txt', content_type: 'text/plain', size_bytes: 10, url: '/api/human-messages/5/attachments/7' }]
        : [{ id: -1, filename: 'shot.png', content_type: 'image/png', size_bytes: 10, url: '/api/agent-messages/5/attachments/-1' }] }], total: 1 });
  };
  const mounted = await harness.mount(harness.html`<${island.FusedMail} state=${state} snapshot=${harness.signals.signal({})} fetchImpl=${respond} localFetch=${respond}
    timers=${timers} now=${() => 0} remote="" toast=${() => {}} confirm=${async () => true} openOnNode=${() => {}} />`);
  const c = mounted.container;
  const once = async () => { const q = timers.queue.splice(0); await harness.act(async () => { for (const x of q) await x.fn(); }); };
  const subjects = () => [...c.querySelectorAll('.mail-row-subject')].map((s) => s.textContent).sort();
  await once();
  assert.deepEqual(subjects(), ['desk agent note', 'forge agent note']);
  // Agent mail shows its files with the same card as a notification; a
  // peer's load through this node's peer route for agent mail files.
  await harness.act(() => harness.fireEvent(c.querySelector('.mail-row-wrap[data-key="inst_self/agent/5"] .mail-row'), 'click'));
  assert.equal(c.querySelector('#fused-mail-reader .mail-attachment a').getAttribute('href'), '/api/agent-messages/5/attachments/-1');
  assert.equal(c.querySelector('#fused-mail-reader .mail-attachment-label').textContent, 'Attached file');
  await harness.act(() => harness.fireEvent(c.querySelector('.mail-row-wrap[data-key="inst_forge/agent/5"] .mail-row'), 'click'));
  assert.equal(c.querySelector('#fused-mail-reader .mail-attachment a').getAttribute('href'), '/api/peer-agent-attachments/inst_forge/5/-1',
    'a peer\'s agent mail file loads through this node\'s peer route, never the local URL the peer sent');
  await harness.act(() => harness.fireEvent(getByRole(c, 'button', { name: /Human notifications/ }), 'click'));
  assert.deepEqual(subjects(), [], 'the agent pages are not relabelled as human notifications');
  assert.match(c.querySelector('#fused-mail-list').textContent, /Loading/);
  await once();
  assert.deepEqual(subjects(), ['desk human note', 'forge human note']);
  // This node's notification gets the same buttons as its own Messages; the
  // page's action handler runs them.
  await harness.act(() => harness.fireEvent(c.querySelector('.mail-row-wrap[data-key="inst_self/human/5"] .mail-row'), 'click'));
  const acts = [...c.querySelectorAll('#fused-mail-reader [data-act]')].map((b) => b.dataset.act);
  for (const act of ['msg-reply', 'msg-focus', 'msg-mark-read', 'msg-delete']) assert.ok(acts.includes(act), act);
  assert.ok(c.querySelector('#fused-mail-reader .mail-download'), 'the download menu is there');
  assert.equal(c.querySelector('#fused-mail-reader .mail-attachment a').getAttribute('href'), '/api/human-messages/5/attachments/7');
  const labels = () => [...c.querySelectorAll('#fused-mail-reader .mail-reader-actions > button, #fused-mail-reader .mail-download > button:first-child')]
    .map((b) => b.textContent.trim()).filter((l) => !l.startsWith('Open on'));
  const localLabels = labels();
  // forge's has the same row, answered through forge's human-inbox; what
  // forge does not take from here stays greyed in place.
  await harness.act(() => harness.fireEvent(c.querySelector('.mail-row-wrap[data-key="inst_forge/human/5"] .mail-row'), 'click'));
  assert.deepEqual(labels(), localLabels, 'the same buttons as this node\'s notification');
  assert.equal(c.querySelector('#fused-mail-reader .mail-attachment a').getAttribute('href'), '/api/peer-attachments/inst_forge/5/7',
    'a peer\'s file loads through this node\'s peer attachment route, never the local URL the peer sent');
  assert.ok(c.querySelector('[data-human="focus"]').disabled && c.querySelector('[data-human="delete"]').disabled);
  await harness.act(async () => { harness.fireEvent(c.querySelector('[data-human="mark"]'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(calls.includes('POST /api/peer/inst_forge/human-inbox/read {"id":5}'));
  // reply opens the same reply dialog, which sends through forge.
  await harness.act(() => harness.fireEvent(c.querySelector('[data-human="reply"]'), 'click'));
  assert.equal(replies.length, 1);
  assert.equal(replies[0].nodeName, 'forge');
  assert.equal(replies[0].online, undefined, 'this node\'s snapshot does not know forge\'s agents: forge decides');
  await harness.act(async () => { await replies[0].send('on it'); });
  assert.ok(calls.includes('POST /api/peer/inst_forge/human-inbox/reply {"id":5,"body":"on it"}'));
  forgeTrusted = false;
  await once();
  assert.deepEqual(subjects(), ['desk human note'], 'a node that withdrew trust keeps nothing on screen');
  assert.match(c.querySelector('.fused-mail-node[data-node="inst_forge"]').textContent, /no longer trusted/);
  await mounted.unmount(); state.dispose();
});

test('each node\'s agent folders arrive one server page at a time, with Show more for the rest', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fused-mail.js')]);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('messages'), search: '?nodes=all' });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true, level: 'unrestricted' }] });
  const timers = fakeTimers(); const calls = [];
  const agent = (i) => ({ id: `conv-${i}`, kind: 'agent', title: `agent-${i}`, total: 1, unread: 0, last_at: `2026-01-01T00:00:${String(59 - i).padStart(2, '0')}Z` });
  const respond = async (url) => {
    calls.push(url);
    const roster = url.match(/\/mailboxes\?agents_limit=(\d+)/);
    if (roster) {
      // forge holds 150 agent folders; this node sends its whole roster and
      // no total, like a node on an older build.
      if (!url.startsWith('/api/peer/inst_forge/')) return ok({ mailboxes: [agent(0)] });
      const n = Math.min(Number(roster[1]), 150);
      return ok({ mailboxes: Array.from({ length: n }, (_, i) => agent(i)), agents_total: 150 });
    }
    if (url.endsWith('human-inbox')) return ok({ messages: [], access_requests: [] });
    return ok({ messages: [], total: 0 });
  };
  const mounted = await harness.mount(harness.html`<${island.FusedMail} state=${state} snapshot=${harness.signals.signal({})} fetchImpl=${respond} localFetch=${respond}
    timers=${timers} now=${() => 0} remote="" toast=${() => {}} confirm=${async () => true} openOnNode=${() => {}} />`);
  const c = mounted.container;
  const once = async () => { const q = timers.queue.splice(0); await harness.act(async () => { for (const x of q) await x.fn(); }); };
  const forgeRows = () => c.querySelectorAll('.fused-mail-section .mailbox').length;
  await once();
  assert.ok(calls.includes('/api/peer/inst_forge/mailboxes?agents_limit=0&hide_prev_gens=1'), 'a folded agent list reads only its totals');
  assert.match(c.querySelector('.fused-mail-section[data-node="inst_forge"] .mailbox-section-toggle:not(.fused-mail-node-head)').textContent, /All agent mailboxes \(150\)/);
  for (const node of ['inst_self', 'inst_forge']) {
    await harness.act(() => harness.fireEvent(c.querySelector(`.fused-mail-section[data-node="${node}"] .mailbox-section-toggle:not(.fused-mail-node-head)`), 'click'));
  }
  calls.length = 0;
  await once();
  assert.ok(calls.includes('/api/peer/inst_forge/mailboxes?agents_limit=100&hide_prev_gens=1'), 'a page, not the whole roster');
  assert.ok(calls.includes('/api/mailboxes?agents_limit=100&hide_prev_gens=1'));
  const more = c.querySelector('.fused-mail-section[data-node="inst_forge"] .mailbox-load-more');
  assert.ok(more, 'forge has more folders than its page');
  assert.match(more.textContent, /Show more · 100 of 150/);
  assert.equal(c.querySelector('.fused-mail-section[data-node="inst_self"] .mailbox-load-more'), null, 'no total, no Show more');
  const before = forgeRows();

  calls.length = 0;
  await harness.act(() => harness.fireEvent(more, 'click'));
  await once();
  assert.ok(calls.includes('/api/peer/inst_forge/mailboxes?agents_limit=200&hide_prev_gens=1'), 'Show more asks forge for a bigger page');
  assert.ok(calls.includes('/api/mailboxes?agents_limit=100&hide_prev_gens=1'), 'other nodes keep their page');
  assert.equal(c.querySelector('.fused-mail-section[data-node="inst_forge"] .mailbox-load-more'), null, 'every folder shown');
  assert.equal(forgeRows(), before + 50);
  await mounted.unmount(); state.dispose();
});

test('the node chips narrow the view to one node; pages, filters and bulk actions work like this node\'s Messages', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fused-mail.js')]);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('messages'), search: '?nodes=all' });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true, level: 'unrestricted' }] });
  const timers = fakeTimers(); const calls = []; const toasts = []; const confirms = [];
  // 30 messages on each node, interleaved in time: desk on even seconds,
  // forge on odd ones.
  const mail = (node, i) => ({ id: i, from_agent: 'agt_abcd1234', from_title: `${node}-bot`, to_title: 'peer', subject: `${node} ${i}`, body: 'b', read: false,
    created_at: new Date(Date.UTC(2026, 0, 1, 0, 0, i * 2 + (node === 'forge' ? 1 : 0))).toISOString() });
  const box = { desk: Array.from({ length: 30 }, (_, i) => mail('desk', i + 1)).reverse(), forge: Array.from({ length: 30 }, (_, i) => mail('forge', i + 1)).reverse() };
  const respond = async (url, init = {}) => {
    calls.push(init.method === 'POST' ? `POST ${url} ${init.body}` : url);
    if (init.method === 'POST') return ok({ ok: true });
    const node = url.startsWith('/api/peer/inst_forge/') ? 'forge' : 'desk';
    if (/\/mailboxes\?/.test(url)) return ok({ mailboxes: [{ id: 'all', kind: 'all', title: 'All agent messages', total: 30 }, { id: 'human', kind: 'human', title: 'Human notifications', total: 2, unread: node === 'forge' ? 2 : 0 }], agents_total: 0 });
    if (url.endsWith('human-inbox')) return ok({ messages: [], access_requests: [] });
    const params = new URL(url, 'http://x').searchParams;
    const size = Number(params.get('page_size')); const page = Number(params.get('page'));
    return ok({ messages: box[node].slice((page - 1) * size, page * size), total: 30, total_unfiltered: 30 });
  };
  const confirm = async (o) => { confirms.push(o); await o.action(); return true; };
  const mounted = await harness.mount(harness.html`<${island.FusedMail} state=${state} snapshot=${harness.signals.signal({})} fetchImpl=${respond} localFetch=${respond}
    timers=${timers} now=${() => 0} remote="" toast=${(m, e) => toasts.push([m, e])} confirm=${confirm} openOnNode=${() => {}} />`);
  const c = mounted.container;
  const once = async () => { const q = timers.queue.splice(0); await harness.act(async () => { for (const x of q) await x.fn(); }); };
  const click = (el) => harness.act(() => harness.fireEvent(el, 'click'));
  const subjects = () => [...c.querySelectorAll('.mail-row-subject')].map((s) => s.textContent);
  await once();

  // Page 1 of the merge: the newest 50 across both nodes.
  assert.equal(subjects().length, 50);
  assert.deepEqual(subjects().slice(0, 3), ['forge 30', 'desk 30', 'forge 29']);
  assert.match(c.querySelector('#fused-mail-pager .mail-pager-pos').textContent, /Page 1 \/ 2/);
  assert.equal(c.querySelector('#fused-mail-filter-count').textContent, '60 messages');
  assert.match(c.querySelector('.fused-mail-node[data-node="inst_forge"]').textContent, /forge\s*2/, 'the chip carries the node\'s unread notifications');
  calls.length = 0;
  await click(c.querySelector('#fused-mail-pager button[title="Next page"]'));
  await once();
  assert.ok(calls.includes('/api/peer/inst_forge/mailbox?id=all&page=1&page_size=100'), 'page 2 of a merge reads each node down to it');
  assert.deepEqual(subjects(), ['forge 5', 'desk 5', 'forge 4', 'desk 4', 'forge 3', 'desk 3', 'forge 2', 'desk 2', 'forge 1', 'desk 1']);

  // forge's chip: only forge's mail, read with forge's own pages.
  calls.length = 0;
  await click(c.querySelector('.fused-mail-chip[data-node="inst_forge"]'));
  await once();
  assert.equal(c.querySelectorAll('.fused-mail-section').length, 1, 'only forge\'s folders');
  assert.ok(subjects().every((s) => s.startsWith('forge')), 'only forge\'s messages');
  assert.ok(!calls.some((u) => u.startsWith('/api/mailbox?')), 'this node\'s page is not read');
  assert.ok(calls.includes('/api/peer/inst_forge/mailbox?id=all&page=1&page_size=50'), 'one node in view pages on that node');
  await click(c.querySelector('.mail-row'));
  const replyBox = c.querySelector('.mail-reply-text');
  let key;
  await harness.act(() => { key = harness.fireEvent(replyBox, 'keydown', { key: 'ArrowLeft' }); });
  assert.equal(key.defaultPrevented, false, 'arrows in the reply box move its caret, not the focus');
  assert.equal(c.querySelector('#fused-mail-filter-count').textContent, '30 messages');

  // Bulk read goes to forge; delete stays disabled, since only this node's
  // mail can be deleted from here.
  const all = c.querySelector('.mail-select-all');
  await harness.act(() => { all.checked = true; harness.fireEvent(all, 'change'); });
  assert.match(c.querySelector('#fused-mail-bulk-bar').textContent, /30 selected/);
  const del = [...c.querySelectorAll('#fused-mail-bulk-bar button')].find((b) => /delete selected/.test(b.textContent));
  assert.equal(del.disabled, true);
  await harness.act(async () => { harness.fireEvent([...c.querySelectorAll('#fused-mail-bulk-bar button')].find((b) => /✓ read/.test(b.textContent)), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(calls.some((u) => u.startsWith('POST /api/peer/inst_forge/mailbox/mark-read {"ids":[30,29,') && u.endsWith('"read":true}')));
  assert.equal(c.querySelector('.mail-row-wrap[data-key="inst_forge/agent/30"] .mail-row-del').disabled, true, 'a peer\'s row cannot be deleted from here');

  // Back to all nodes: deleting a mixed selection deletes this node's row only.
  await click(c.querySelector('.fused-mail-chip[data-scope=""]'));
  await once();
  for (const key of ['inst_self/agent/30', 'inst_forge/agent/30']) {
    const box = c.querySelector(`.mail-row-wrap[data-key="${key}"] .mail-msg-check`);
    await harness.act(() => { box.checked = true; harness.fireEvent(box, 'change'); });
  }
  calls.length = 0;
  await harness.act(async () => { harness.fireEvent([...c.querySelectorAll('#fused-mail-bulk-bar button')].find((b) => /delete selected/.test(b.textContent)), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.match(confirms.at(-1).body, /1 on peers stay/);
  assert.deepEqual(calls.filter((u) => u.startsWith('POST')), ['POST /api/mailbox/delete {"ids":[30]}']);

  // The folder filter and the scope toggles reach every node's reads.
  const boxFilter = c.querySelector('#fused-mail-box-filter');
  await harness.act(() => { boxFilter.value = 'bot'; harness.fireEvent(boxFilter, 'input'); });
  await once();
  calls.length = 0;
  await once();
  assert.ok(calls.includes('/api/peer/inst_forge/mailboxes?agents_limit=100&q=bot&hide_prev_gens=1'), 'a filter shows the matching agent folders');
  const retired = c.querySelector('#fused-mail-show-retired');
  await harness.act(() => { retired.checked = true; harness.fireEvent(retired, 'change'); });
  calls.length = 0;
  await once();
  assert.ok(calls.includes('/api/mailbox?id=all&page=1&page_size=50&include_retired=1'));
  assert.ok(calls.includes('/api/peer/inst_forge/mailboxes?agents_limit=100&q=bot&include_retired=1&hide_prev_gens=1'));
  await mounted.unmount(); state.dispose();
});

test('on a peer\'s page, this node\'s own notification files keep their local route', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fused-mail.js')]);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('messages'), search: '?nodes=all' });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true, level: 'unrestricted' }] });
  const timers = fakeTimers();
  const respond = async (url) => {
    if (/\/mailboxes\?/.test(url)) return ok({ mailboxes: [] });
    if (url.endsWith('human-inbox')) return ok({ messages: [], access_requests: [] });
    const peer = url.startsWith('/api/peer/');
    return ok({ messages: [{ id: peer ? 8 : 5, from_agent: 'agt_abcd1234', from_conv: 'c0ffee00-0000-4000-8000-000000000001', subject: 'note', body: 'b', created_at: '2026-01-01T00:00:00Z',
      attachments: [{ id: 7, filename: 'notes.txt', content_type: 'text/plain', size_bytes: 10, url: `/api/human-messages/${peer ? 8 : 5}/attachments/7` }] }], total: 1 });
  };
  const mounted = await harness.mount(harness.html`<${island.FusedMail} state=${state} snapshot=${harness.signals.signal({})} fetchImpl=${respond} localFetch=${respond}
    timers=${timers} now=${() => 0} remote="inst_forge" toast=${() => {}} confirm=${async () => true} openOnNode=${() => {}} />`);
  const c = mounted.container;
  const once = async () => { const q = timers.queue.splice(0); await harness.act(async () => { for (const x of q) await x.fn(); }); };
  await once();
  await harness.act(() => harness.fireEvent(getByRole(c, 'button', { name: /Human notifications/ }), 'click'));
  await once();
  await harness.act(() => harness.fireEvent(c.querySelector('.mail-row-wrap[data-key="inst_self/human/5"] .mail-row'), 'click'));
  assert.equal(c.querySelector('#fused-mail-reader .mail-attachment a').getAttribute('href'), '/api/human-messages/5/attachments/7');
  assert.equal(c.querySelector('#fused-mail-reader [data-act]'), null, 'a peer\'s page refuses data-act buttons; the reply box answers instead');
  await harness.act(() => harness.fireEvent(c.querySelector('.mail-row-wrap[data-key="inst_forge/human/8"] .mail-row'), 'click'));
  assert.equal(c.querySelector('#fused-mail-reader .mail-attachment a').getAttribute('href'), '/api/peer-attachments/inst_forge/8/7');
  await mounted.unmount(); state.dispose();
});
