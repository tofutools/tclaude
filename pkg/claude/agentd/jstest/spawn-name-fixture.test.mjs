import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createPreactHarness } from './preact-harness.mjs';

// The dashboard's spawn-name gate and normalizer agree with the one fixture
// the Go CLI and daemon are pinned to, so a name the dialog accepts (or
// rewrites) is exactly what the daemon accepts (or rewrites).
const rows = JSON.parse(readFileSync(new URL('../../agent/testdata/spawn_names.json', import.meta.url), 'utf8'));

test('dashboard spawn-name gate and normalizer match the shared Go fixture', async (t) => {
  const harness = await createPreactHarness(t);
  {
    const model = await harness.importDashboardModule('js/agent-spawn-model.js');
    assert.ok(rows.length > 0);
    for (const row of rows) {
      // An empty name is the optional, auto-labelled case: every caller
      // checks `!raw || SPAWN_NAME_VALID.test(raw)`.
      const valid = !row.in || model.SPAWN_NAME_VALID.test(row.in);
      assert.equal(valid, row.valid, `valid(${JSON.stringify(row.in)})`);
      assert.equal(model.normalizeSpawnName(row.in), row.normalized, `normalize(${JSON.stringify(row.in)})`);
    }
    assert.equal(model.MAX_SPAWN_NAME_LEN, 64);
  }
});
