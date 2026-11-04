import test from 'node:test';
import assert from 'node:assert/strict';
import { readdir, readFile } from 'node:fs/promises';
import { createPreactHarness } from './preact-harness.mjs';

const reply = (status, body) => async () => ({
  ok: status >= 200 && status < 300, status,
  json: async () => { if (body === undefined) throw new SyntaxError('no body'); return body; },
});

test('requestJSON: one error shape, {status, code, message, body}, whatever module sends it', async (t) => {
  const harness = await createPreactHarness(t);
  const { requestJSON, RequestError } = await harness.importDashboardModule('js/request-json.js');
  const { FleetAdminError } = await harness.importDashboardModule('js/fleet-admin-actions.js');

  const coded = await requestJSON('/api/x', { fetchImpl: reply(409, { error: 'already running', code: 'busy', plan: [1] }) }).catch((e) => e);
  assert.ok(coded instanceof RequestError);
  assert.deepEqual({ status: coded.status, code: coded.code, message: coded.message, body: coded.body },
    { status: 409, code: 'busy', message: 'already running', body: { error: 'already running', code: 'busy', plan: [1] } });

  const bare = await requestJSON('/api/x', { fetchImpl: reply(502) }).catch((e) => e);
  assert.deepEqual({ status: bare.status, code: bare.code, message: bare.message, body: bare.body },
    { status: 502, code: '', message: 'HTTP 502', body: null }, 'a body that is not JSON: the status speaks');

  const named = await requestJSON('/api/x', { fetchImpl: reply(403, { error: 'no' }), ErrorClass: FleetAdminError }).catch((e) => e);
  assert.ok(named instanceof FleetAdminError && named instanceof RequestError);
  assert.equal(named.message, 'no');
});

test('requestJSON: sends the cookie and a JSON body, and reads an empty answer as null', async (t) => {
  const harness = await createPreactHarness(t);
  const { requestJSON } = await harness.importDashboardModule('js/request-json.js');
  const sent = [];
  const fetchImpl = async (url, init) => { sent.push([url, init]); return reply(204)(); };
  assert.equal(await requestJSON('/api/y', { method: 'POST', body: { a: 1 }, cache: 'no-store', fetchImpl }), null);
  assert.deepEqual(sent, [['/api/y', { method: 'POST', credentials: 'same-origin', cache: 'no-store',
    headers: { 'Content-Type': 'application/json' }, body: '{"a":1}' }]]);
  assert.equal(await requestJSON('/api/z', { fetchImpl: reply(200, { ok: true }) }).then((d) => d.ok), true);
});

// The copy hunt (tcl-mtlt9x) folded 13 hand-rolled fetch-and-throw helpers
// into requestJSON. A new one would drift again (dropping code, or the text).
test('dashboard modules throw through requestJSON, not a hand-rolled HTTP error', async () => {
  const dir = new URL('../dashboard/js/', import.meta.url);
  const offenders = [];
  for (const name of (await readdir(dir)).filter((n) => n.endsWith('.js') && n !== 'request-json.js')) {
    const src = await readFile(new URL(name, dir), 'utf8');
    if (/new Error\(\w+\?\.error \|\| `HTTP \$\{res\.status\}`\)/.test(src)) offenders.push(name);
  }
  assert.deepEqual(offenders, [], 'use requestJSON from js/request-json.js');
});
