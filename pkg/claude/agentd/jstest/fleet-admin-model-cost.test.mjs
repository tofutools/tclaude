// Usage sharing step 2: the gateway usage table shows what each peer's
// requests cost.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('usage rows sum priced cost and count unpriced requests', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/fleet-admin-models.js');
  const rows = m.usageRows([
    { proxy: 'team', peer: 'inst_a', model: 'claude-opus-5-5', status: 200, complete: true, charged_tokens: 10, priced: true, charged_micro_usd: 1_300_000 },
    { proxy: 'team', peer: 'inst_a', model: 'claude-opus-5-5', status: 529, complete: false, charged_tokens: 70, priced: true, charged_micro_usd: 0 },
    { proxy: 'team', peer: 'inst_a', model: 'local-x', status: 200, complete: true, charged_tokens: 5 },
  ]);
  const opus = rows.find((r) => r.model === 'claude-opus-5-5');
  assert.equal(opus.microUSD, 1_300_000);
  assert.equal(opus.failed, 1);
  assert.equal(rows.find((r) => r.model === 'local-x').unpriced, 1);
  assert.equal(m.usdText(1_300_001), '$1.31');
  assert.equal(m.usdText(0), '$0.00');
});
