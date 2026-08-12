import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Owner-conferred permission sources use ordinary scope dimensions', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/message-access-dialog-model.js');

  assert.equal(model.ownerSource(['group'], ['dev', 'qa']), 'owner grant: dev, qa');
  assert.equal(model.ownerSource([], ['dev', 'qa']), 'owner grant: global');

  // And the row builder uses it, so the phrasing is not merely available.
  const snapshot = {
    permissions: { defaults: [] },
    groups: [],
    agents: [{ conv_id: 'c1', owned_groups: ['dev'], groups: ['dev'] }],
    slugs: [
      { slug: 'groups.members.spawn', owner_implied: true, scope_dims: ['group'] },
      { slug: 'human.notify', owner_implied: true },
      { slug: 'groups.messages.schedule', owner_implied: true, scope_dims: ['group'], member_implied: true },
    ],
  };
  const rows = model.permissionRows(snapshot, { mode: 'agent', conv: 'c1' }, {});
  const bySlug = Object.fromEntries(rows.map((row) => [row.slug, row]));
  assert.deepEqual(bySlug['groups.members.spawn'].sources, ['owner grant: dev']);
  assert.deepEqual(bySlug['human.notify'].sources, ['owner grant: global']);
  assert.deepEqual(bySlug['groups.messages.schedule'].sources, ['owner grant: dev', 'member: dev']);
});
