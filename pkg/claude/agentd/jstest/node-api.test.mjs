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

test('a download from another node comes through this node, read from that node in chunks', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/node-api.js');
  const path = '/api/snapshots/known%20good/export';
  const via = (node) => `/api/peer-download/${node}?path=${encodeURIComponent(path)}`;
  // On this node's own page.
  assert.equal(mod.nodeDownloadPath('', path, { self: 'inst_self', remote: null }), path);
  assert.equal(mod.nodeDownloadPath('inst_self', path, { self: 'inst_self', remote: null }), path);
  assert.equal(mod.nodeDownloadPath('inst_forge', path, { self: 'inst_self', remote: null }), via('inst_forge'));
  // On forge's page, forge's own files come through this node too.
  const calls = [];
  const remote = { id: 'inst_forge', localFetch: async (url) => { calls.push(['local', url]); return { ok: true }; } };
  const fetchImpl = async (url) => { calls.push(['page', url]); return { ok: true }; };
  await mod.nodeDownload('', path, { self: 'inst_self', remote, fetchImpl });
  await mod.nodeDownload('inst_self', path, { self: 'inst_self', remote, fetchImpl });
  assert.deepEqual(calls, [['local', via('inst_forge')], ['local', path]]);
  assert.throws(() => mod.nodeDownloadPath('inst_x"]', path, { self: 'inst_self', remote: null }));
});

test('an upload to another node goes through this node, which sends it on in chunks', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/node-api.js');
  const calls = [];
  const fetchImpl = async (url, init) => { calls.push(['page', url, init.method]); return { ok: true }; };
  const localFetch = async (url, init) => { calls.push(['local', url, init.method]); return { ok: true }; };
  const via = (node) => `/api/peer-upload/${node}?path=${encodeURIComponent('/api/spawn-attachments')}`;
  await mod.nodeUpload('', '/api/spawn-attachments', {}, { self: 'inst_self', remote: null, fetchImpl });
  await mod.nodeUpload('inst_forge', '/api/spawn-attachments', {}, { self: 'inst_self', remote: null, fetchImpl });
  const remote = { id: 'inst_forge', localFetch };
  await mod.nodeUpload('', '/api/spawn-attachments', {}, { self: 'inst_self', remote, fetchImpl });
  await mod.nodeUpload('inst_self', '/api/spawn-attachments', {}, { self: 'inst_self', remote, fetchImpl });
  assert.deepEqual(calls, [
    ['page', '/api/spawn-attachments', 'POST'], ['page', via('inst_forge'), 'POST'],
    ['local', via('inst_forge'), 'POST'], ['local', '/api/spawn-attachments', 'POST'],
  ]);
});
