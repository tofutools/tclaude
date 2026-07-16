import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function transactionModules(t) {
  const harness = await createPreactHarness(t);
  const [stateModule, actionsModule, controller] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
    harness.importDashboardModule('js/transaction-dialog-controller.js'),
  ]);
  return { harness, stateModule, actionsModule, controller };
}

test('shutdown and delete controllers freeze stable-selector descriptors', async (t) => {
  const { stateModule, controller } = await transactionModules(t);
  const state = stateModule.createTransactionDialogState();
  const unregister = controller.registerTransactionDialogController(state);

  const shutdown = controller.openShutdownAgentDialog('agt_stable-one', 'Stable one');
  assert.deepEqual(state.dialog.value.descriptor, {
    kind: 'shutdown-agent', agent: 'agt_stable-one', label: 'Stable one',
  });
  assert.ok(Object.isFrozen(state.dialog.value.descriptor));
  state.close();
  assert.equal(await shutdown, null);

  const deletion = controller.openDeleteAgentDialog('agt_stable-two', 'Stable two');
  assert.deepEqual(state.dialog.value.descriptor, {
    kind: 'delete-agent', agent: 'agt_stable-two', label: 'Stable two',
  });
  assert.ok(Object.isFrozen(state.dialog.value.descriptor));
  state.close();
  assert.equal(await deletion, null);
  unregister();
});

test('shutdown action preserves exact force payload and completes after refresh', async (t) => {
  const { stateModule, actionsModule } = await transactionModules(t);
  for (const row of [
    { force: false, action: 'soft_stopped' },
    { force: true, action: 'killed' },
  ]) {
    await t.test(row.action, async () => {
      const state = stateModule.createTransactionDialogState();
      const requests = [];
      const notices = [];
      let refreshedWhileOwned = null;
      const pending = state.open({
        kind: 'shutdown-agent', agent: 'agt_stable', label: 'Stable target',
      });
      const actions = actionsModule.createTransactionDialogActions({
        state,
        fetchImpl: async (url, init) => {
          requests.push([url, init]);
          return new Response(JSON.stringify({ action: row.action }), {
            status: 200, headers: { 'Content-Type': 'application/json' },
          });
        },
        refresh: async () => { refreshedWhileOwned = state.dialog.value; },
        notify: (...args) => notices.push(args),
      });

      const result = await actions.shutdownAgent({
        agent: 'agt_stable', label: 'Stable target', force: row.force,
      });
      assert.equal(requests[0][0], '/api/agents/agt_stable/stop');
      assert.equal(requests[0][1].method, 'POST');
      assert.equal(requests[0][1].credentials, 'same-origin');
      assert.deepEqual(requests[0][1].headers, { 'Content-Type': 'application/json' });
      assert.equal(requests[0][1].body, JSON.stringify({ force: row.force }));
      assert.equal(refreshedWhileOwned, null, 'successful mutation unpaints before refresh');
      assert.deepEqual(notices, [[`shutdown Stable target: ${row.action}`]]);
      assert.deepEqual(result, { ok: true, action: row.action, response: { action: row.action } });
      assert.deepEqual(await pending, result);
      assert.equal(state.dialog.value, null);
    });
  }
});

test('shutdown failures preserve the frozen transaction for explicit retry', async (t) => {
  const { stateModule, actionsModule } = await transactionModules(t);
  for (const row of [
    {
      name: 'transport failure',
      fetchImpl: async () => { throw new Error('network down'); },
      expected: /network down/,
    },
    {
      name: 'HTTP failure',
      fetchImpl: async () => new Response('stop refused', { status: 503 }),
      expected: /stop refused/,
    },
    {
      name: 'HTTP-200 lifecycle error',
      fetchImpl: async () => new Response(JSON.stringify({
        action: 'error', detail: 'tmux kill failed',
      }), { status: 200, headers: { 'Content-Type': 'application/json' } }),
      expected: /tmux kill failed/,
    },
  ]) {
    await t.test(row.name, async () => {
      const state = stateModule.createTransactionDialogState();
      const pending = state.open({
        kind: 'shutdown-agent', agent: 'agt_frozen', label: 'Frozen target',
      });
      const descriptor = state.dialog.value.descriptor;
      const actions = actionsModule.createTransactionDialogActions({
        state, fetchImpl: row.fetchImpl, refresh: async () => {}, notify: () => {},
      });
      await assert.rejects(actions.shutdownAgent({
        agent: descriptor.agent, label: descriptor.label, force: true,
      }), row.expected);
      assert.equal(state.dialog.value.descriptor, descriptor,
        'the same descriptor remains mounted for renderer-owned inline retry');
      state.close();
      assert.equal(await pending, null);
    });
  }
});

test('delete action sends the optional query only for the frozen opt-in choice', async (t) => {
  const { stateModule, actionsModule } = await transactionModules(t);
  for (const row of [
    {
      name: 'kept worktree', deleteWorktree: false,
      url: '/api/agents/agt_delete', payload: null,
    },
    {
      name: 'truthy non-boolean stays opted out', deleteWorktree: 1,
      url: '/api/agents/agt_delete', payload: null,
    },
    {
      name: 'opted in',
      deleteWorktree: true,
      expectedWorktree: '/repo/worktrees/feature & review?#',
      url: '/api/agents/agt_delete?delete_worktree=1&expected_worktree=%2Frepo%2Fworktrees%2Ffeature+%26+review%3F%23',
      payload: { conv_id: 'current-conv', worktree: 'worktree removed' },
    },
  ]) {
    await t.test(row.name, async () => {
      const state = stateModule.createTransactionDialogState();
      const requests = [];
      const notices = [];
      let refreshes = 0;
      const pending = state.open({
        kind: 'delete-agent', agent: 'agt_delete', label: 'Delete target',
      });
      const actions = actionsModule.createTransactionDialogActions({
        state,
        fetchImpl: async (url, init) => {
          requests.push([url, init]);
          if (!row.payload) return new Response(null, { status: 204 });
          return new Response(JSON.stringify(row.payload), {
            status: 200, headers: { 'Content-Type': 'application/json' },
          });
        },
        refresh: async () => { refreshes += 1; },
        notify: (...args) => notices.push(args),
      });

      const result = await actions.deleteAgent({
        agent: 'agt_delete', label: 'Delete target', deleteWorktree: row.deleteWorktree,
        ...(row.expectedWorktree ? { expectedWorktree: row.expectedWorktree } : {}),
      });
      assert.equal(requests[0][0], row.url);
      assert.equal(requests[0][1].method, 'DELETE');
      assert.equal(requests[0][1].credentials, 'same-origin');
      assert.equal('body' in requests[0][1], false, 'permanent delete has no JSON body');
      assert.equal(refreshes, 1);
      assert.deepEqual(notices, [[row.payload?.worktree
        ? `deleted Delete target · ${row.payload.worktree}`
        : 'deleted Delete target']]);
      assert.deepEqual(result, { ok: true, response: row.payload || {} });
      assert.deepEqual(await pending, result);
    });
  }
});

test('delete failures preserve descriptor and retry ownership', async (t) => {
  const { stateModule, actionsModule } = await transactionModules(t);
  const state = stateModule.createTransactionDialogState();
  const pending = state.open({
    kind: 'delete-agent', agent: 'agt_delete', label: 'Frozen delete target',
  });
  const descriptor = state.dialog.value.descriptor;
  const urls = [];
  const actions = actionsModule.createTransactionDialogActions({
    state,
    fetchImpl: async (url) => {
      urls.push(url);
      return new Response('worktree changed; refresh and retry', { status: 409 });
    },
    refresh: async () => {},
    notify: () => {},
  });
  const frozenRequest = Object.freeze({
    agent: descriptor.agent,
    label: descriptor.label,
    deleteWorktree: true,
    expectedWorktree: '/repo/frozen & exact',
  });
  await assert.rejects(actions.deleteAgent(frozenRequest), /worktree changed/);
  await assert.rejects(actions.deleteAgent(frozenRequest), /worktree changed/);
  assert.deepEqual(urls, [
    '/api/agents/agt_delete?delete_worktree=1&expected_worktree=%2Frepo%2Ffrozen+%26+exact',
    '/api/agents/agt_delete?delete_worktree=1&expected_worktree=%2Frepo%2Ffrozen+%26+exact',
  ], 'explicit retry reuses the exact frozen selector, choice, and worktree path');
  assert.equal(state.dialog.value.descriptor, descriptor);
  state.close();
  assert.equal(await pending, null);
});
