// Usage sharing step 1: timed peer grants in the grant dialog's plan.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('a lifetime times new grants, keeps an existing expiry, and extends a held grant without rewriting it', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/fleet-admin-model.js');
  const now = Date.parse('2026-10-19T12:00:00Z');
  const rows = m.grantRows([
    { peer: 'bob', slug: 'message.direct', scope: 'group_id=4', group_name: 'one' },
    { peer: 'bob', slug: 'groups.members.spawn', scope: 'group_id=4', group_name: 'one', spawn_policy: { max_live: 3 }, expires_at: '2026-10-20T12:00:00Z' },
  ]);
  assert.equal(rows.find((r) => r.slug === 'groups.members.spawn').expiresAt, '2026-10-20T12:00:00Z');

  let plan = m.planGrants({ target: 'bob', slugs: ['groups.roster.read'], groups: ['one'], rows, lifetime: '86400', now });
  assert.equal(plan.grants[0].body.expires_at, '2026-10-20T12:00:00.000Z', 'a new grant ends after the lifetime');

  plan = m.planGrants({ target: 'bob', slugs: ['groups.members.spawn'], groups: ['one'], rows, maxLive: 5, now });
  assert.equal(plan.grants[0].body.expires_at, '2026-10-20T12:00:00Z', 'updating a timed spawn grant keeps its expiry');

  plan = m.planGrants({ target: 'bob', slugs: ['message.direct'], groups: ['one'], rows, now });
  assert.equal(plan.grants[0].skip, true, 'a held grant without a lifetime is already granted');
  plan = m.planGrants({ target: 'bob', slugs: ['message.direct'], groups: ['one'], rows, lifetime: '3600', now });
  assert.equal(plan.grants[0].skip, false);
  assert.equal(plan.grants[0].extend, true, 'a lifetime on a held grant only extends it');
  assert.equal(plan.grants[0].scope, 'group_id=4', 'and addresses the held grant by its own scope');

  assert.equal(m.grantExpiryText('2026-10-19T12:20:00Z', now), 'expires in 20 min');
  assert.equal(m.grantExpiryText('2026-10-20T12:00:00Z', now), 'expires in 24 h');
  assert.equal(m.grantExpiryText('2026-10-26T12:00:00Z', now), 'expires in 7 d');
  assert.equal(m.grantExpiryText('', now), '');
});
