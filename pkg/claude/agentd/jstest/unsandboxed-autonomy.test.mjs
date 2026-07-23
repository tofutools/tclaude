import test from 'node:test';
import assert from 'node:assert/strict';
import { fetchUnsandboxedAutonomy } from '../dashboard/js/unsandboxed-autonomy.js';

const EMPTY = { warnings: [], sandboxState: '', sandboxSource: '' };

test('fetchUnsandboxedAutonomy maps a good payload and forwards same-origin credentials', async () => {
  const calls = [];
  const fetchImpl = async (url, options) => {
    calls.push({ url, options });
    return { ok: true, json: async () => ({ warnings: ['⚠ unconfined'], sandbox_state: 'off', sandbox_source: '~/.claude/settings.json' }) };
  };
  const result = await fetchUnsandboxedAutonomy(fetchImpl, { harness: 'claude', sandbox: 'off', approval: 'auto', dir: '/repo' });
  assert.deepEqual(result, { warnings: ['⚠ unconfined'], sandboxState: 'off', sandboxSource: '~/.claude/settings.json' });

  assert.equal(calls.length, 1);
  assert.equal(calls[0].options.credentials, 'same-origin');
  const url = new URL(calls[0].url, 'http://x');
  assert.equal(url.pathname, '/api/spawn/effective-sandbox');
  assert.equal(url.searchParams.get('harness'), 'claude');
  assert.equal(url.searchParams.get('sandbox'), 'off');
  assert.equal(url.searchParams.get('approval'), 'auto');
  assert.equal(url.searchParams.get('dir'), '/repo');
});

test('fetchUnsandboxedAutonomy defaults every field so a bare call is valid', async () => {
  const fetchImpl = async (url) => {
    const params = new URL(url, 'http://x').searchParams;
    // A missing field would serialize as the string "undefined"; assert it does not.
    for (const key of ['harness', 'sandbox', 'approval', 'dir']) assert.equal(params.get(key), '');
    return { ok: true, json: async () => ({}) };
  };
  assert.deepEqual(await fetchUnsandboxedAutonomy(fetchImpl), EMPTY);
});

test('fetchUnsandboxedAutonomy returns empty (never throws) on a non-ok response', async () => {
  const fetchImpl = async () => ({ ok: false, status: 403, json: async () => ({ warnings: ['leaked'] }) });
  assert.deepEqual(await fetchUnsandboxedAutonomy(fetchImpl, { harness: 'claude' }), EMPTY);
});

test('fetchUnsandboxedAutonomy returns empty on a thrown fetch (transient network error)', async () => {
  const fetchImpl = async () => { throw new Error('network down'); };
  assert.deepEqual(await fetchUnsandboxedAutonomy(fetchImpl, { harness: 'claude' }), EMPTY);
});

test('fetchUnsandboxedAutonomy tolerates malformed JSON and a non-array warnings field', async () => {
  const badJson = async () => ({ ok: true, json: async () => { throw new SyntaxError('bad'); } });
  assert.deepEqual(await fetchUnsandboxedAutonomy(badJson, {}), EMPTY);

  const nonArray = async () => ({ ok: true, json: async () => ({ warnings: 'nope', sandbox_state: 'on' }) });
  assert.deepEqual(await fetchUnsandboxedAutonomy(nonArray, {}), { warnings: [], sandboxState: 'on', sandboxSource: '' });
});
