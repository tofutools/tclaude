import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function fakeTimers() {
  const queue = []; let seq = 0;
  return {
    queue,
    setTimeout(fn, ms) { const id = ++seq; queue.push({ id, fn, ms }); return id; },
    clearTimeout(id) { const i = queue.findIndex((q) => q.id === id); if (i >= 0) queue.splice(i, 1); },
  };
}

test('the request dialog sends permission, group, TTL and reason and follows the decision', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-access.js');
  const calls = []; const timers = fakeTimers();
  const actions = {
    request: async (o) => { calls.push(['request', o]); return { id: 'par-9', status: 'pending', grant_ttl_seconds: o.ttl }; },
    status: async (id) => { calls.push(['status', id]); return { id, status: 'approved', grant_ttl_seconds: 86400, grant_group_id: 4 }; },
  };
  const mounted = await harness.mount(harness.html`<${mod.RequestAccessDialog} node="forge" perm="groups.members.clone" groups=${[{ id: 4, name: 'ops' }]} onClose=${() => {}} actions=${actions} timers=${timers} />`);
  const doc = harness.document; const q = (s) => doc.querySelector(s);
  assert.deepEqual([...q('#peer-access-group').querySelectorAll('option')].map((o) => o.value), ['', '4'], 'a group permission may leave the group to the peer');
  const ttl = q('#peer-access-ttl'); for (const o of ttl.querySelectorAll('option')) { if (o.value === '86400') o.setAttribute('selected', ''); else o.removeAttribute('selected'); }
  await harness.act(() => harness.fireEvent(ttl, 'change'));
  const reason = q('#peer-access-reason'); reason.value = ' clone for triage ';
  await harness.act(() => harness.fireEvent(reason, 'input'));
  await harness.act(() => q('#peer-access-submit').click());
  await harness.act(() => new Promise((r) => setTimeout(r, 25)));
  assert.deepEqual(calls[0], ['request', { permission: 'groups.members.clone', groupID: 0, reason: 'clone for triage', ttl: 86400 }]);
  assert.match(q('#peer-access-status').textContent, /par-9: pending/);
  const tick = timers.queue.find((x) => x.ms === 3000);
  assert.ok(tick);
  await harness.act(() => { timers.queue.splice(timers.queue.indexOf(tick), 1); tick.fn(); });
  await harness.act(() => new Promise((r) => setTimeout(r, 25)));
  assert.match(q('#peer-access-status').textContent, /approved.*Granted for 1 day in group #4/s);
  assert.equal(timers.queue.length, 0);
  await mounted.unmount();
});

test('a peer that shares nothing yet explains the 403; the client sends the route shape', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-access.js');
  const sent = [];
  const a = mod.createPeerAccessActions({ fetchImpl: async (url, init) => { sent.push([init.method, url, init.body ? JSON.parse(init.body) : undefined]); return { ok: true, json: async () => ({}) }; } });
  await a.request({ permission: 'node.exec', ttl: 0 });
  await a.status('par 1');
  assert.deepEqual(sent, [['POST', '/api/peer-access-requests', { permission: 'node.exec', grant_ttl_seconds: 0 }], ['GET', '/api/peer-access-requests/par%201', undefined]]);
  const actions = { request: async () => { const e = new Error('forbidden'); e.status = 403; throw e; } };
  const mounted = await harness.mount(harness.html`<${mod.RequestAccessDialog} node="forge" perm="node.exec" onClose=${() => {}} actions=${actions} />`);
  const doc = harness.document;
  assert.equal(doc.querySelector('#peer-access-group'), null, 'node-wide permissions have no group');
  await harness.act(() => doc.querySelector('#peer-access-submit').click());
  await harness.act(() => new Promise((r) => setTimeout(r, 25)));
  assert.match(doc.querySelector('[role=alert]').textContent, /forge only takes requests from peers it already shares something with/);
  await mounted.unmount();
});
