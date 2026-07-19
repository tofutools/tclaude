import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { createPreactHarness } from './preact-harness.mjs';

test('browser request digest matches the agentd client SHA-256 contract', async (t) => {
  const harness = await createPreactHarness(t);
  const { sha256Hex, idempotentRequestHeaders } = await harness.importDashboardModule('js/request-idempotency.js');
  assert.equal(sha256Hex(''), createHash('sha256').update('').digest('hex'));
  assert.equal(sha256Hex('abc'), createHash('sha256').update('abc').digest('hex'));
  assert.equal(sha256Hex('Release train 🚂'), createHash('sha256').update('Release train 🚂').digest('hex'));
  assert.equal(sha256Hex('a'.repeat(1000)), createHash('sha256').update('a'.repeat(1000)).digest('hex'));

  const method = 'POST'; const path = '/v1/process/templates'; const body = '{"name":"Release train"}';
  const key = '11111111-2222-4333-8444-555555555555';
  const headers = idempotentRequestHeaders(method, path, body, key);
  assert.deepEqual(headers, {
    'Content-Type': 'application/json',
    'Idempotency-Key': key,
    'X-Tclaude-Request-Digest': createHash('sha256')
      .update(`${method}\x00${path}\x00${body}`).digest('hex'),
  });
});
