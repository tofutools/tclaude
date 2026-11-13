import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('parseTokens reads suffixed decimals without float drift', async (t) => {
  const harness = await createPreactHarness(t);
  const { parseTokens, carryBody, emptyCarry } = await harness.importDashboardModule('js/morph-carry.js');
  assert.equal(parseTokens('4.1m'), 4_100_000);
  assert.equal(parseTokens('0.5M'), 500_000);
  assert.equal(parseTokens('600k'), 600_000);
  assert.equal(parseTokens('300_000'), 300_000);
  assert.equal(parseTokens(''), null);
  assert.ok(Number.isNaN(parseTokens('1.5')), 'a bare token count is whole');
  assert.ok(Number.isNaN(parseTokens('lots')));
  assert.deepEqual(carryBody({ ...emptyCarry(), window: '4.1m' }).target_window_tokens, 4_100_000);
});
