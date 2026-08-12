import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// Blueprint grant lists (a role's default permissions, a template agent's
// inline grants) carry the canonical grant/scope shape. The shared permission
// editor uses the override union, so the role entry point adapts in both
// directions without flattening a narrowed grant.

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
  let permissionOptions = null;
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  mountManagementIsland({
    host,
    state,
    actions: { loadUnsandboxedAutonomy: async () => ({ warnings: [] }), ...actions },
    confirmDiscard: async () => true,
    openProfilePermissions(options) { permissionOptions = options; },
    registerCleanup(fn) { cleanups.push(fn); },
  });
  await harness.act(() => Promise.resolve());
  return { host, permissionOptions: () => permissionOptions, cleanup: () => cleanups.reverse().forEach((fn) => fn()) };
}

test('the role editor opens the shared grant-only editor with canonical scoped input', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const seed = {
    name: 'lead', harness: 'claude',
    permissions: [{ slug: 'groups.members.spawn', scope: { group: ['dev'] } }],
  };
  const { host, permissionOptions, cleanup } = await openRoleEditor(harness, seed, {
    async saveRole({ payload }) { saved.push(payload); },
  });
  assert.equal(host.querySelector('.ta-perms-list'), null, 'the old checkbox wall is gone');
  await harness.act(() => host.querySelector('#role-editor-perms').click());
  assert.deepEqual(permissionOptions().overrides, {
    'groups.members.spawn': { effect: 'grant', scope: { group: ['dev'] } },
  });
  assert.equal(permissionOptions().grantOnly, true);
  assert.equal(permissionOptions().subject, 'role');

  await harness.act(() => permissionOptions().onSave({
    'groups.members.spawn': { effect: 'grant', scope: { group: ['dev', 'ops'] } },
    'human.notify': 'grant',
  }));
  await harness.act(async () => { host.querySelector('#role-editor-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0].permissions,
    [{ slug: 'groups.members.spawn', scope: { group: ['dev', 'ops'] } }, 'human.notify']);
  cleanup();
});

test('canceling the permission sub-editor leaves the role draft unchanged', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const seed = {
    name: 'lead', harness: 'claude',
    permissions: [{ slug: 'groups.members.spawn', scope: { group: ['dev'] } }],
  };
  const { host, permissionOptions, cleanup } = await openRoleEditor(harness, seed, {
    async saveRole({ payload }) { saved.push(payload); },
  });
  await harness.act(() => host.querySelector('#role-editor-perms').click());
  assert.ok(permissionOptions(), 'the sub-editor was opened');
  // Cancel does not invoke onSave. Saving the parent therefore emits its
  // original draft, including the exact scope.
  await harness.act(async () => { host.querySelector('#role-editor-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0].permissions, seed.permissions);
  cleanup();
});

test('grant helpers project the union without widening it', async (t) => {
  const harness = await createPreactHarness(t);
  const { grantSlug, grantScopeLabel, hasGrant, toggleGrant, grantToOverride,
    grantListToOverrides, grantOverridesToList } =
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
  assert.deepEqual(grantListToOverrides(['human.notify', { slug: 'x', scope: { group: ['a'] } }]), {
    'human.notify': 'grant', x: { effect: 'grant', scope: { group: ['a'] } },
  });
  assert.deepEqual(grantOverridesToList({
    'human.notify': 'grant', x: { effect: 'grant', scope: { group: ['a'] } }, ignored: 'deny',
  }), ['human.notify', { slug: 'x', scope: { group: ['a'] } }]);
});
