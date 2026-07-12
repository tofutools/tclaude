import test from 'node:test'; import assert from 'node:assert/strict'; import { createPreactHarness } from './preact-harness.mjs';
test('Audit model preserves presentation, sort metadata, and API params', async (t) => {
  const harness = await createPreactHarness(t); const model = await harness.importDashboardModule('js/audit-model.js');
  assert.equal(model.verbClass('agent.delete'), 'audit-verb danger'); assert.equal(model.verbClass('sudo.grant'), 'audit-verb elevate'); assert.equal(model.verbClass('spawn'), 'audit-verb create');
  assert.deepEqual(model.statusView(403), { className: 'state-pill state-awaiting', label: 'denied', title: '403 — permission denied' });
  assert.equal(model.actorTitle({ actor_kind: 'agent', actor_label: 'Worker' }, 'agt_123'), 'Worker agt_123');
  assert.equal(model.targetTitle({ group_name: 'team', target_label: 'Worker' }), 'team Worker');
  const params = model.auditParams({ page: 2, pageSize: 50, sort: 'actor', dir: 'asc', query: ' stop ', outcome: 'failure', source: 'popup' });
  assert.equal(params.toString(), 'page=2&page_size=50&sort=actor&dir=asc&q=stop&outcome=failure&source=popup');
});
