import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// TCL-1013: the dialog names the owned groups as an owner-conferred
// slug's source. That is right for a group- or member-scoped slug, but
// wrong for an unscoped one (human.notify, process.runs.read): those come
// from owning ANYTHING, so naming particular groups reads as a limit the
// gate does not impose.
test('Owner-conferred permission sources carry their scope', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/message-access-dialog-model.js');

  assert.equal(model.ownerSource('group', ['dev', 'qa']), 'owner: dev, qa');
  assert.equal(model.ownerSource('member', ['dev', 'qa']), 'owner: members of dev, qa');
  assert.equal(model.ownerSource('any', ['dev', 'qa']), 'owner: any group owned');
  // A daemon that predates owner_scope sends none; keep the old wording.
  assert.equal(model.ownerSource(undefined, ['dev']), 'owner: dev');
  // A scope a NEWER daemon invented must not be guessed into the
  // narrower group phrasing — that would misreport the reach.
  assert.equal(model.ownerSource('fleet', ['dev']), 'owner: conferred by group ownership');

  // And the row builder uses it, so the phrasing is not merely available.
  const snapshot = {
    permissions: { defaults: [] },
    groups: [],
    agents: [{ conv_id: 'c1', owned_groups: ['dev'] }],
    slugs: [
      { slug: 'groups.spawn', owner_implied: true, owner_scope: 'group' },
      { slug: 'human.notify', owner_implied: true, owner_scope: 'any' },
    ],
  };
  const rows = model.permissionRows(snapshot, { mode: 'agent', conv: 'c1' }, {});
  const bySlug = Object.fromEntries(rows.map((row) => [row.slug, row]));
  assert.deepEqual(bySlug['groups.spawn'].sources, ['owner: dev']);
  assert.deepEqual(bySlug['human.notify'].sources, ['owner: any group owned']);
});
