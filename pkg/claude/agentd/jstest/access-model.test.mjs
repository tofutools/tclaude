import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Access model preserves permission, filter, sort, and expiry semantics', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/access-model.js');
  const rows = model.permissionRows({ overrides: {
    b: { 'agent.send': 'grant', 'agent.kill': 'deny' },
    a: { 'agent.spawn': 'grant' },
  } }, [{ conv_id: 'b', agent_id: 'agt_b', title: 'Beta' }]);
  assert.deepEqual(rows.map((row) => row.convId), ['a', 'b']);
  assert.deepEqual(rows[1].granted, ['agent.send']);
  assert.deepEqual(rows[1].denied, ['agent.kill']);
  assert.equal(rows[0].title, '(unknown)');

  const grants = [
    { id: 2, conv_title: 'Zulu', slug: 'agent.send', remaining_seconds: 10 },
    { id: 1, conv_title: 'Alpha', slug: 'agent.spawn', remaining_seconds: 30 },
  ];
  assert.equal(model.matchesSudo(grants[0], 'SEND'), true);
  assert.deepEqual(model.sortSudo(grants, { key: 'conv', dir: 'asc' }).map((row) => row.id), [1, 2]);
  assert.deepEqual(model.sortSudo(grants, { key: 'expires', dir: 'desc' }).map((row) => row.id), [1, 2]);
  assert.deepEqual(model.sortSudo([
    { id: 1, reason: '' }, { id: 2, reason: 'alpha' }, { id: 3, reason: 'zulu' },
  ], { key: 'reason', dir: 'desc' }).map((row) => row.id), [3, 2, 1], 'blank cells stay last descending');
  assert.equal(model.remainingSeconds({ expires_at: '2026-07-12T00:00:10Z' }, Date.parse('2026-07-12T00:00:03Z')), 7);
  assert.equal(model.remainingSeconds({ remaining_seconds: 9 }, Date.parse('2026-07-12T00:00:04Z'), Date.parse('2026-07-12T00:00:00Z')), 5);
  assert.equal(model.fmtRemaining(3661), '1h1m');
});
