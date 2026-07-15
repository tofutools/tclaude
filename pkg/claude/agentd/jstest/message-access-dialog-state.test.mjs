import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('dialog state refuses retargeting and resolves chooser cancellation on every teardown', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  assert.equal(state.openMessage({ from: 'agt_alice' }), true);
  assert.equal(state.openMessage({ from: 'agt_bob' }), false);
  assert.equal(state.dialog.value.prefill.from, 'agt_alice');

  const canceledByParent = state.pickAgent({ identity: 'agent' });
  state.close();
  assert.equal(await canceledByParent, '');
  assert.equal(state.picker.value, null);

  const canceledByUnmount = state.pickAgent({ identity: 'conv' });
  state.dispose();
  assert.equal(await canceledByUnmount, '');
  assert.equal(state.dialog.value, null);
});

test('cron target controller preserves explicit missing identities and scope', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  const modes = [];
  state.setCronTargetModeListener((mode) => modes.push(mode));
  state.configureCronTarget({ targetMode: 'group', groupName: 'deleted-group', scopeGroup: '' });
  assert.deepEqual(state.readCronTarget(), { mode: 'group', target: 'group:deleted-group' });
  state.configureCronTarget({ targetMode: 'solo', target: 'agt_stable', scopeGroup: 'team' });
  assert.deepEqual(state.readCronTarget(), { mode: 'solo', target: 'agt_stable' });
  assert.equal(state.cronTarget.value.scopeGroup, 'team');
  assert.deepEqual(modes, ['solo', 'group', 'solo']);
});

test('models keep stable identity, role search, and permission veto/source data', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/message-access-dialog-model.js');
  const snapshot = {
    agents: [{ agent_id: 'agt_alice', conv_id: 'conv-new', title: 'alice', online: true, groups: ['team'], owned_groups: ['team'] }],
    groups: [{ name: 'team', permissions: ['groups.spawn'], members: [{ agent_id: 'agt_alice', conv_id: 'conv-new', title: 'alice', role: 'reviewer', descr: 'cold eyes', online: true }] }],
    permissions: { defaults: ['self.rename'], overrides: { 'conv-new': { 'groups.spawn': 'deny' } } },
    slugs: [
      { slug: 'groups.spawn', description: 'spawn', owner_implied: true },
      { slug: 'self.rename', description: 'rename', owner_implied: false },
    ],
  };
  assert.equal(model.agentCandidates(snapshot, { query: 'cold eyes' })[0].agent_id, 'agt_alice');
  assert.equal(model.senderOnline(snapshot, 'agt_alice', 'conv-old'), true, 'stable agent identity wins across reincarnation');

  const descriptor = { mode: 'agent', conv: 'conv-new' };
  const rows = model.permissionRows(snapshot, descriptor, model.permissionSeed(snapshot, descriptor));
  const spawn = rows.find((row) => row.slug === 'groups.spawn');
  assert.equal(spawn.effect, 'deny');
  assert.equal(spawn.granted, false, 'explicit deny vetoes group and owner sources');
  const rename = rows.find((row) => row.slug === 'self.rename');
  assert.equal(rename.granted, true);
  assert.deepEqual(rename.sources, ['global default']);

  const profile = model.permissionRows(snapshot, { mode: 'buffer', group: 'the spawn group' }, {});
  assert.deepEqual(profile.find((row) => row.slug === 'groups.spawn').sources, [],
    'a reusable profile invents no destination group source');
});
