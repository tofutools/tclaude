import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// Blueprint grant lists (a role's default permissions, a template agent's
// inline grants) may carry scoped entries that the checkbox editors cannot
// express. The rule these tests pin is one-directional: such an entry is
// carried through a save untouched, never flattened to its bare slug — that
// flattening would widen a grant someone deliberately narrowed, off a save
// made for an entirely unrelated reason.

const catalog = [{ name: 'claude', can_sandbox: true, can_approval: true, sandbox_modes: ['off'], approval_modes: ['auto'] }];
const slugs = [
  { slug: 'groups.members.spawn', description: 'spawn' },
  { slug: 'human.notify', description: 'notify' },
];

async function openRoleEditor(harness, seed, actions) {
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'role-editor', seed, options: {}, catalog, slugs });
  const cleanups = [];
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  mountManagementIsland({
    host,
    state,
    actions: { loadUnsandboxedAutonomy: async () => ({ warnings: [] }), ...actions },
    confirmDiscard: async () => true,
    openProfilePermissions() {},
    registerCleanup(fn) { cleanups.push(fn); },
  });
  await harness.act(() => Promise.resolve());
  return { host, cleanup: () => cleanups.reverse().forEach((fn) => fn()) };
}

test('the role editor keeps a scoped grant it cannot express, and labels it', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const seed = {
    name: 'lead', harness: 'claude',
    permissions: [{ slug: 'groups.members.spawn', scope: { group: ['dev'] } }],
  };
  const { host, cleanup } = await openRoleEditor(harness, seed, {
    async saveRole({ payload }) { saved.push(payload); },
  });

  const boxes = [...host.querySelectorAll('.ta-perms-list label')];
  const spawn = boxes.find((label) => label.textContent.includes('groups.members.spawn'));
  assert.equal(spawn.querySelector('input').getAttribute('checked'), 'true',
    'a scoped grant is still a held grant — the box must be ticked');
  assert.equal(spawn.querySelector('.perm-scope-chip').textContent, 'group=dev',
    'and the row says what it is narrowed to, so the operator is not misled');

  // Save with an unrelated edit: the scoped entry must come back out exactly
  // as it went in.
  const notify = boxes.find((label) => label.textContent.includes('human.notify'));
  await harness.act(() => harness.fireEvent(notify.querySelector('input'), 'change'));
  await harness.act(async () => { host.querySelector('#role-editor-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0].permissions,
    [{ slug: 'groups.members.spawn', scope: { group: ['dev'] } }, 'human.notify']);
  cleanup();
});

test('unticking a scoped grant removes it rather than leaving a hidden duplicate', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const seed = {
    name: 'lead', harness: 'claude',
    permissions: [{ slug: 'groups.members.spawn', scope: { group: ['dev'] } }],
  };
  const { host, cleanup } = await openRoleEditor(harness, seed, {
    async saveRole({ payload }) { saved.push(payload); },
  });
  const spawn = [...host.querySelectorAll('.ta-perms-list label')]
    .find((label) => label.textContent.includes('groups.members.spawn'));
  await harness.act(() => harness.fireEvent(spawn.querySelector('input'), 'change'));
  await harness.act(async () => { host.querySelector('#role-editor-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0].permissions, [], 'the entry is gone, whatever shape it had');
  cleanup();
});

test('grant helpers project the union without widening it', async (t) => {
  const harness = await createPreactHarness(t);
  const { grantSlug, grantScopeLabel, hasGrant, toggleGrant, grantToOverride } =
    await harness.importDashboardModule('js/permission-grant-list.js');

  assert.equal(grantSlug('human.notify'), 'human.notify');
  assert.equal(grantSlug({ slug: 'routes.publish', scope: { group: ['a'] } }), 'routes.publish');
  assert.equal(grantScopeLabel('human.notify'), '');
  assert.equal(grantScopeLabel({ slug: 'x', scope: { spawn_profile: ['p1'], group: ['a', 'b'] } }),
    'group=a,b spawn_profile=p1', 'dimensions read in the same order as the daemon renders them');
  assert.equal(hasGrant([{ slug: 'x', scope: { group: ['a'] } }], 'x'), true);
  assert.deepEqual(toggleGrant([{ slug: 'x', scope: { group: ['a'] } }], 'x'), []);
  assert.deepEqual(toggleGrant([], 'x'), ['x']);
  // The extract-to-profile path keeps the scope on the override union.
  assert.equal(grantToOverride('x'), 'grant');
  assert.deepEqual(grantToOverride({ slug: 'x', scope: { group: ['a'] } }),
    { effect: 'grant', scope: { group: ['a'] } });
});
