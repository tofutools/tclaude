import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('nodeFetch addresses the page node, this node from a peer page, and any other peer through the proxy', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/node-api.js');
  const calls = [];
  const fetchImpl = async (url, init) => { calls.push(['page', String(url), init?.method || 'GET']); return { ok: true }; };
  const localFetch = async (url) => { calls.push(['local', String(url)]); return { ok: true }; };
  // On this node's own page.
  assert.equal(mod.nodeFetch('', { self: 'inst_self', fetchImpl }), fetchImpl);
  assert.equal(mod.nodeFetch('inst_self', { self: 'inst_self', fetchImpl, remote: null }), fetchImpl);
  await mod.nodeFetch('inst_forge', { self: 'inst_self', fetchImpl, remote: null })('/api/groups/ops?x=1', { method: 'PATCH' });
  assert.deepEqual(calls.pop(), ['page', '/api/peer/inst_forge/groups/ops?x=1', 'PATCH']);
  // On forge's page: forge is the page; this node is reached directly.
  const remote = { id: 'inst_forge', localFetch };
  assert.equal(mod.nodeFetch('inst_forge', { self: 'inst_self', fetchImpl, remote }), fetchImpl);
  await mod.nodeFetch('inst_self', { self: 'inst_self', fetchImpl, remote })('/api/groups/ops');
  assert.deepEqual(calls.pop(), ['local', '/api/groups/ops']);
  await mod.nodeFetch('inst_lab0', { self: 'inst_self', fetchImpl, remote })('/api/groups/ops');
  assert.deepEqual(calls.pop(), ['page', '/api/peer/inst_lab0/groups/ops', 'GET']);
  // Only node API paths, and only well-formed nodes, are addressed.
  assert.throws(() => mod.nodeFetch('inst_x"]', { self: 'inst_self', fetchImpl, remote: null }));
  await assert.rejects(async () => mod.nodeFetch('inst_forge', { self: 'inst_self', fetchImpl, remote: null })('/static/x.js'));
  await assert.rejects(async () => mod.nodeFetch('inst_forge', { self: 'inst_self', fetchImpl, remote: null })('/api/peer/inst_lab0/groups'));
});
