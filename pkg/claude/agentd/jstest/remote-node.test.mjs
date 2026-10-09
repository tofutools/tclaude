import test from 'node:test'; import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../dashboard/js/remote-node.js', import.meta.url), 'utf8');

function boot(search) {
  const calls = []; const events = [];
  const classes = new Set();
  const window = {
    location: { search, href: `http://127.0.0.1:7777/groups${search}`, origin: 'http://127.0.0.1:7777' },
    fetch: async (input, init) => {
      const url = typeof input === 'string' ? input : input.url;
      calls.push({ url, method: init?.method || 'GET' });
      if (url.includes('/snapshot')) return new Response(JSON.stringify({ error: 'peer is unreachable', code: 'peer_unreachable', reason: 'peer_offline' }), { status: 502 });
      return new Response('{}', { status: 200 });
    },
    dispatchEvent: (event) => events.push(event),
  };
  const document = { documentElement: { classList: { add: (c) => classes.add(c) } } };
  vm.runInNewContext(source, { window, document, URL, URLSearchParams, Request, Response, CustomEvent: class { constructor(type, init) { this.type = type; this.detail = init?.detail; } }, Date, JSON, Set, Object, String });
  return { window, calls, events, classes };
}

test('without ?node= the shim leaves fetch alone', () => {
  const { window, classes } = boot('?wizard=1');
  assert.equal(window.__tclaudeRemoteNode, undefined);
  assert.equal(classes.size, 0);
});

test('a malformed node id is ignored rather than proxied', () => {
  assert.equal(boot('?node=../../api').window.__tclaudeRemoteNode, undefined);
});

test('per-node API reads go through the peer proxy; fleet reads stay local; other methods are refused', async () => {
  const { window, calls, classes } = boot('?node=inst_forge7');
  assert.ok(classes.has('remote-node'));
  await window.fetch('/api/groups?x=1');
  await window.fetch('/api/federation/status?summary=1');
  await window.fetch('/api/peer/inst_lab/node-summary');
  await window.fetch('/api/node-summary');
  await window.fetch('/api/node/update', { method: 'POST' });
  await window.fetch('/static/js/x.js');
  await window.fetch('/api/operator-message', { method: 'POST' });
  assert.deepEqual(calls.map((c) => c.url), [
    '/api/peer/inst_forge7/groups?x=1', '/api/federation/status?summary=1', '/api/peer/inst_lab/node-summary', '/api/node-summary', '/api/node/update', '/static/js/x.js', '/api/peer/inst_forge7/operator-message',
  ]);
  const refused = await window.fetch('/api/groups/x', { method: 'DELETE' });
  assert.equal(refused.status, 403);
  assert.equal((await refused.json()).code, 'peer_view_read_only');
  assert.equal(calls.length, 7, 'a refused method never reaches any agentd');
});

test('snapshot failures publish remote health for the marker', async () => {
  const { window, events } = boot('?node=inst_forge7');
  const r = await window.fetch('/api/snapshot?static_version=1');
  assert.equal(r.status, 502);
  assert.equal(events.at(-1).type, 'tclaude:remote-health');
  assert.equal(events.at(-1).detail.failure.code, 'peer_unreachable');
  assert.equal(window.__tclaudeRemoteNode.health.ok, false);
});

test('the operator\'s own surfaces stay local; Request objects are rewritten with their method and body', async () => {
  const { window, calls } = boot('?node=inst_forge7');
  await window.fetch('/api/dashboard/prefs');
  await window.fetch('/api/human-messages/read', { method: 'POST' });
  await window.fetch('/api/auth/session');
  await window.fetch(new Request('http://127.0.0.1:7777/api/operator-message', { method: 'POST', body: '{"x":1}' }));
  assert.deepEqual(calls.map((c) => c.url), ['/api/dashboard/prefs', '/api/human-messages/read', '/api/auth/session', 'http://127.0.0.1:7777/api/peer/inst_forge7/operator-message']);
});
