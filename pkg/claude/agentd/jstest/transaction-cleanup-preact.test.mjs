import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

function escape(harness) {
  const event = new harness.window.Event('keydown', { bubbles: true });
  Object.defineProperty(event, 'key', { value: 'Escape' });
  harness.document.dispatchEvent(event);
}

async function openCleanup(t, descriptor, actionOverrides = {}) {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, island] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-island.js'),
  ]);
  const state = createTransactionDialogState();
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const opener = harness.document.body.appendChild(harness.document.createElement('button'));
  opener.id = 'cleanup-opener';
  opener.focus();
  const actions = {
    close: state.close,
    cleanup: async () => ({}),
    finishCleanup: async (response) => {
      const result = { kind: 'cleanup', response };
      state.handoff();
      state.finish(result);
      return result;
    },
    handoffCleanupWorktrees: async () => {},
    ...actionOverrides,
  };
  const mounted = await harness.mount(harness.html`
    <${island.TransactionDialogApp}
      state=${state}
      actions=${actions}
      confirmDiscard=${async () => true}
    />
  `, host);
  let pending;
  await harness.act(() => { pending = state.open(descriptor); });
  await harness.act(() => Promise.resolve());
  return { harness, state, host, opener, actions, mounted, pending };
}

test('cleanup descriptor freezes the complete sorted roster and group owner boundary', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, controller] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-controller.js'),
  ]);
  const snapshot = {
    agents: [{
      agent_id: 'agt_active', conv_id: 'active-conv', title: 'Active', online: false,
      state: { last_hook: '2026-03-01T00:00:00Z' }, groups: ['alpha'], owned_groups: ['alpha'],
    }],
    groups: [{ name: 'alpha', members: [
      { agent_id: 'agt_owner', conv_id: 'owner-conv', title: 'Owner', owner: true,
        online: false, state: { last_hook: '2026-01-01T00:00:00Z' } },
      { agent_id: 'agt_online', conv_id: 'online-conv', title: 'Online', owner: false,
        online: true, state: { last_hook: '2025-01-01T00:00:00Z' } },
    ] }],
  };
  const retired = [{
    agent_id: 'agt_retired', conv_id: 'retired-conv', title: 'Retired', online: false,
    retired_at: '2026-02-01T00:00:00Z',
  }];
  const conversations = [{
    conv_id: 'plain-conv', title: 'Plain', modified: '', online: false,
  }];
  const descriptor = controller.buildCleanupDescriptor(
    snapshot, { mode: 'agents', tier: 'retire', categories: ['agent', 'retired'] },
    { retired, conversations },
  );
  assert.deepEqual(descriptor.candidates.map((candidate) => candidate.conv_id), [
    'plain-conv', 'retired-conv', 'active-conv',
  ]);
  assert.equal(descriptor.tier, 'retire');
  assert.deepEqual(descriptor.categories, ['agent', 'retired']);

  const state = createTransactionDialogState();
  const unregister = controller.registerTransactionDialogController(state);
  const pending = controller.openCleanupDialog(descriptor);
  retired[0].title = 'poll mutation';
  conversations.push({ conv_id: 'late-conv', title: 'Late' });
  assert.equal(state.dialog.value.descriptor.candidates[1].title, 'Retired');
  assert.equal(state.dialog.value.descriptor.candidates.length, 3);
  assert.ok(Object.isFrozen(state.dialog.value.descriptor.candidates[0]));
  state.close();
  await pending;
  unregister();

  const group = controller.buildCleanupDescriptor(snapshot, { mode: 'group', group: 'alpha' });
  assert.deepEqual(group.candidates.map((candidate) => candidate.conv_id), ['owner-conv']);
  assert.equal(group.candidates[0].owner, true);
});

test('group cleanup preserves owner opt-in and submits only checked enabled rows', async (t) => {
  const requests = [];
  const opened = await openCleanup(t, {
    kind: 'cleanup', mode: 'group', group: 'alpha', tier: 'unjoin', categories: ['agent'],
    candidates: [
      { agent_id: 'agt_member', conv_id: 'member-conv', title: 'Member', category: 'agent',
        online: false, lastActivity: '', owner: false, groups: [] },
      { agent_id: 'agt_owner', conv_id: 'owner-conv', title: 'Owner', category: 'agent',
        online: false, lastActivity: '', owner: true, groups: [] },
    ],
  }, { cleanup: async (request) => { requests.push(request); return { removed: 1 }; } });
  const { harness, host, pending } = opened;
  assert.equal(host.querySelector('#cleanup-count').textContent, '1 selected');
  assert.equal(host.querySelector('[data-conv="owner-conv"]').disabled, true);

  const owners = host.querySelector('#cleanup-opt-owners');
  owners.checked = true;
  await harness.act(() => harness.fireEvent(owners, 'change'));
  assert.equal(host.querySelector('#cleanup-count').textContent, '2 selected');
  const ownerRow = host.querySelector('[data-conv="owner-conv"]');
  ownerRow.checked = false;
  await harness.act(() => harness.fireEvent(ownerRow, 'change'));
  host.querySelector('#cleanup-submit').click();
  await harness.act(() => Promise.resolve());

  assert.equal(requests.length, 1);
  assert.deepEqual(requests[0].targets, ['agt_member']);
  assert.equal(requests[0].includeOwners, true,
    'the daemon still receives the explicit owner permission gate');
  assert.match(host.querySelector('#cleanup-hint').textContent, /1 removed/);
  assert.equal(host.querySelector('#cleanup-submit').textContent, 'Done');
  assert.equal(host.querySelector('#cleanup-cancel'), null);
  host.querySelector('#cleanup-submit').click();
  assert.deepEqual(await pending, { kind: 'cleanup', response: { removed: 1 } });
});

test('agent cleanup composes tier, category, online, age, and search gates before freezing retry', async (t) => {
  const first = deferred();
  const second = deferred();
  const requests = [];
  const now = Date.now();
  const opened = await openCleanup(t, {
    kind: 'cleanup', mode: 'agents', tier: 'delete',
    categories: ['agent', 'retired', 'conversation'],
    candidates: [
      { agent_id: 'agt_old', conv_id: 'old-agent', title: 'Old agent', category: 'agent',
        online: false, lastActivity: new Date(now - 72 * 3600000).toISOString(), owner: false, groups: ['alpha'] },
      { agent_id: 'agt_live', conv_id: 'live-agent', title: 'Live agent', category: 'agent',
        online: true, lastActivity: new Date(now - 96 * 3600000).toISOString(), owner: false, groups: [] },
      { agent_id: 'agt_retired', conv_id: 'retired-agent', title: 'Retired agent', category: 'retired',
        online: false, lastActivity: new Date(now - 2 * 3600000).toISOString(), owner: false, groups: [] },
      { agent_id: '', conv_id: 'plain-conv', title: 'Plain conversation', category: 'conversation',
        online: false, lastActivity: '', owner: false, groups: [] },
    ],
  }, {
    cleanup: (request) => {
      requests.push(request);
      return requests.length === 1 ? first.promise : second.promise;
    },
  });
  const { harness, host, pending } = opened;

  const retireTier = host.querySelector('[name="cleanup-tier"][value="retire"]');
  retireTier.checked = true;
  await harness.act(() => harness.fireEvent(retireTier, 'change'));
  assert.equal(host.querySelector('#cleanup-opt-shutdown').disabled, false);
  assert.equal(host.querySelector('#cleanup-opt-wt').disabled, true);
  assert.match(host.querySelector('#cleanup-list').textContent, /Old agent/);
  assert.doesNotMatch(host.querySelector('#cleanup-list').textContent, /Retired agent|Plain conversation|Live agent/);

  const deleteTier = host.querySelector('[name="cleanup-tier"][value="delete"]');
  deleteTier.checked = true;
  await harness.act(() => harness.fireEvent(deleteTier, 'change'));
  const conversationCategory = host.querySelector('[data-cat="conversation"]');
  conversationCategory.checked = false;
  await harness.act(() => harness.fireEvent(conversationCategory, 'change'));
  assert.doesNotMatch(host.querySelector('#cleanup-list').textContent, /Plain conversation/);
  conversationCategory.checked = true;
  await harness.act(() => harness.fireEvent(conversationCategory, 'change'));
  const includeOnline = host.querySelector('#cleanup-opt-online');
  includeOnline.checked = true;
  await harness.act(() => harness.fireEvent(includeOnline, 'change'));
  assert.match(host.querySelector('#cleanup-list').textContent, /Live agent/);
  includeOnline.checked = false;
  await harness.act(() => harness.fireEvent(includeOnline, 'change'));

  const age = host.querySelector('#cleanup-age');
  age.value = '24';
  await harness.act(() => harness.fireEvent(age, 'input'));
  assert.equal(host.querySelector('#cleanup-count').textContent, '2 selected',
    'old offline plus missing-activity rows pass while online stays hidden');

  const search = host.querySelector('#cleanup-search');
  search.value = 'plain';
  await harness.act(() => harness.fireEvent(search, 'input'));
  assert.equal(host.querySelector('#cleanup-count').textContent, '1 selected');
  host.querySelector('#cleanup-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(requests[0], {
    mode: 'agents', tier: 'delete', targets: ['plain-conv'],
    includeOwners: false, includeOnline: false, deleteWorktrees: true, shutdown: false,
  });
  assert.equal(search.disabled, true, 'the first destructive attempt freezes the form');
  escape(harness);
  assert.ok(host.querySelector('#cleanup-modal'), 'busy blocks Escape dismissal');

  first.reject(new Error('cleanup backend unavailable'));
  await harness.act(() => first.promise.catch(() => {}));
  assert.equal(host.querySelector('[role="alert"]').textContent, 'cleanup backend unavailable');
  assert.equal(host.querySelector('#cleanup-search').disabled, true,
    'failed attempts remain locked to the exact approved selection');
  host.querySelector('#cleanup-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(requests[1], requests[0], 'retry reuses the same frozen request object');

  second.resolve({ deleted: 1, skipped: 1, failed: 1, warnings: ['worktree kept'], outcomes: [
    { conv_id: 'plain-conv', title: 'Plain conversation', result: 'deleted', detail: 'purged' },
  ] });
  await harness.act(() => second.promise);
  assert.match(host.querySelector('#cleanup-hint').textContent, /1 deleted.*1 skipped.*1 failed/);
  assert.match(host.querySelector('#cleanup-warn').textContent, /worktree kept/);
  assert.match(host.querySelector('#cleanup-list').textContent, /purged/);
  assert.ok(host.querySelector('#cleanup-modal'), 'partial outcomes remain stable until Done');
  host.querySelector('#cleanup-submit').click();
  assert.deepEqual((await pending).response.outcomes[0].result, 'deleted');
});

test('cleanup request actions preserve wire fields and worktree visual handoff', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
  ]);
  const state = createTransactionDialogState();
  const calls = [];
  const worktrees = deferred();
  const actions = createTransactionDialogActions({
    state,
    fetchImpl: async (url, init) => {
      calls.push([url, JSON.parse(init.body)]);
      return new Response(JSON.stringify({ removed: 1 }), {
        status: 200, headers: { 'Content-Type': 'application/json' },
      });
    },
    openWorktreeCleanup: (group) => { calls.push(['worktrees', group]); return worktrees.promise; },
  });
  await actions.cleanup({
    mode: 'group', group: 'alpha', targets: ['agt_one'], includeOwners: true,
  });
  await actions.cleanup({
    mode: 'agents', tier: 'retire', targets: ['agt_two'], includeOwners: false,
    includeOnline: true, deleteWorktrees: false, shutdown: true,
  });
  assert.deepEqual(calls.slice(0, 2), [
    ['/api/cleanup/group', { group: 'alpha', members: ['agt_one'], include_owners: true }],
    ['/api/cleanup/agents', {
      agents: ['agt_two'], mode: 'retire', include_owners: false,
      include_online: true, delete_worktrees: false, shutdown: true,
    }],
  ]);

  const pending = state.open({ kind: 'cleanup', mode: 'agents', candidates: [] });
  const handoff = actions.handoffCleanupWorktrees({ group: '' });
  await Promise.resolve();
  assert.equal(state.dialog.value, null, 'cleanup unpaints before the janitor takes focus');
  assert.deepEqual(calls[2], ['worktrees', '']);
  assert.equal(await state.open({ kind: 'delete-agent', agent: 'foreign' }), null,
    'the handoff keeps competing transaction launchers blocked');
  worktrees.resolve();
  assert.deepEqual(await handoff, { kind: 'cleanup-worktrees', descriptor: { group: '' } });
  assert.deepEqual(await pending, { kind: 'cleanup-worktrees', descriptor: { group: '' } });
});
