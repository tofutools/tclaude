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

async function openBulk(t, descriptor, options = {}) {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, island] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-island.js'),
  ]);
  const state = createTransactionDialogState();
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const opener = harness.document.body.appendChild(harness.document.createElement('button'));
  opener.id = 'bulk-retire-opener';
  opener.focus();
  const actions = {
    close: state.close,
    retireGroupPreview: async () => ({}),
    retireUngroupedPreview: async () => ({}),
    finishBulkRetire: async (result) => {
      state.handoff();
      state.finish(result);
      return result;
    },
    ...options.actions,
  };
  const mounted = await harness.mount(harness.html`
    <${island.TransactionDialogApp}
      state=${state}
      actions=${actions}
      confirmDiscard=${options.confirmDiscard || (async () => true)}
    />
  `, host);
  let pending;
  await harness.act(() => { pending = state.open(descriptor); });
  await harness.act(() => Promise.resolve());
  return { harness, state, host, opener, actions, mounted, pending };
}

const candidates = [
  {
    agent_id: 'agt_alpha', conv_id: 'alpha-1111-2222-3333-444444444444',
    title: 'Alpha worker', status: 'idle', role: 'builder',
  },
  {
    agent_id: 'agt_beta', conv_id: 'beta-1111-2222-3333-444444444444',
    title: 'Beta worker', status: 'idle', role: 'reviewer',
  },
];

test('bulk retire launchers conv-dedupe and freeze candidates at the controller seam', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, controller] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-controller.js'),
  ]);
  const state = createTransactionDialogState();
  const unregister = controller.registerTransactionDialogController(state);
  const source = [candidates[0], { ...candidates[0], title: 'duplicate' }, candidates[1]];
  const pending = controller.openGroupRetirePreviewDialog('alpha team', 'idle', source);
  const descriptor = state.dialog.value.descriptor;
  assert.equal(descriptor.kind, 'retire-group-preview');
  assert.equal(descriptor.group, 'alpha team');
  assert.equal(descriptor.status, 'idle');
  assert.deepEqual(descriptor.candidates.map((candidate) => candidate.title), [
    'Alpha worker', 'Beta worker',
  ]);
  assert.ok(Object.isFrozen(descriptor));
  assert.ok(Object.isFrozen(descriptor.candidates));
  assert.ok(Object.isFrozen(descriptor.candidates[0]));
  source[0].title = 'mutated after open';
  source.push({ ...candidates[0], conv_id: 'late' });
  assert.equal(descriptor.candidates[0].title, 'Alpha worker');
  assert.equal(descriptor.candidates.length, 2);
  state.close();
  await pending;

  const ungrouped = controller.openUngroupedRetirePreviewDialog([
    candidates[1], { ...candidates[1] }, candidates[0],
  ]);
  assert.equal(state.dialog.value.descriptor.kind, 'retire-ungrouped-preview');
  assert.deepEqual(state.dialog.value.descriptor.candidates.map((candidate) => candidate.conv_id), [
    candidates[1].conv_id, candidates[0].conv_id,
  ]);
  state.close();
  await ungrouped;
  unregister();
});

test('bulk retire actions preserve the exact group and ungrouped wire contracts', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
  ]);
  const state = createTransactionDialogState();
  const requests = [];
  let refreshes = 0;
  const actions = createTransactionDialogActions({
    state,
    fetchImpl: async (url, init) => {
      requests.push([url, init]);
      return new Response(JSON.stringify({ ok: true }), {
        status: 200, headers: { 'Content-Type': 'application/json' },
      });
    },
    refresh: async () => { refreshes += 1; },
    notify: () => {},
  });
  const groupRequest = Object.freeze({
    group: 'alpha/team', convs: [candidates[0].conv_id, candidates[1].conv_id],
    shutdown: false, deleteWorktree: false,
  });
  assert.deepEqual(await actions.retireGroupPreview(groupRequest), { ok: true });
  assert.equal(requests[0][0], '/api/groups/alpha%2Fteam/retire');
  assert.equal(requests[0][1].method, 'POST');
  assert.equal(requests[0][1].credentials, 'same-origin');
  assert.deepEqual(JSON.parse(requests[0][1].body), {
    convs: groupRequest.convs, shutdown: false, delete_worktree: false,
  });

  const looseRequest = Object.freeze({
    agents: ['agt_alpha', 'agt_beta'], shutdown: true, deleteWorktrees: true,
  });
  assert.deepEqual(await actions.retireUngroupedPreview(looseRequest), { ok: true });
  assert.equal(requests[1][0], '/api/cleanup/agents');
  assert.deepEqual(JSON.parse(requests[1][1].body), {
    agents: looseRequest.agents,
    mode: 'retire',
    include_online: true,
    shutdown: true,
    delete_worktrees: true,
  });

  const completion = state.open({
    kind: 'retire-group-preview', group: 'alpha/team', status: 'idle', candidates,
  });
  const result = { kind: 'retire-group-preview', response: { members: [] } };
  assert.equal(refreshes, 0, 'transport success alone does not refresh beneath the result phase');
  assert.deepEqual(await actions.finishBulkRetire(result), result);
  assert.equal(refreshes, 1, 'Done/dismissal refreshes only after handing off visual ownership');
  assert.equal(state.dialog.value, null);
  assert.deepEqual(await completion, result);
});

test('group retire preview keeps hidden checks, freezes retry choices, and renders partial worktree results', async (t) => {
  const first = deferred();
  const second = deferred();
  const requests = [];
  let finishes = 0;
  const mounted = await openBulk(t, {
    kind: 'retire-group-preview', group: 'alpha team', status: 'idle', candidates,
  }, {
    actions: {
      retireGroupPreview: (request) => {
        requests.push(request);
        return requests.length === 1 ? first.promise : second.promise;
      },
      finishBulkRetire: async (result) => {
        finishes += 1;
        mounted.state.handoff();
        mounted.state.finish(result);
        return result;
      },
    },
  });
  const { harness, host, opener } = mounted;
  assert.equal(host.querySelectorAll('#retire-preview-modal').length, 1);
  assert.equal(host.querySelector('#retire-preview-title .theme-copy-regular').textContent,
    'Retire idle agents in "alpha team"');
  assert.equal(host.querySelector('#retire-preview-title .theme-copy-wizard').textContent,
    'Banish idle familiars in "alpha team"');
  assert.equal(host.querySelector('#retire-preview-count').textContent, '2 of 2 selected');
  assert.equal(harness.document.activeElement.id, 'retire-preview-submit');

  const shutdown = host.querySelector('#retire-preview-shutdown');
  const worktrees = host.querySelector('#retire-preview-wt');
  assert.equal(shutdown.hasAttribute('checked'), true);
  assert.equal(worktrees.hasAttribute('checked'), true);
  shutdown.checked = false;
  await harness.act(() => harness.fireEvent(shutdown, 'change'));
  assert.equal(worktrees.disabled, true);
  assert.equal(worktrees.hasAttribute('checked'), false);
  shutdown.checked = true;
  await harness.act(() => harness.fireEvent(shutdown, 'change'));
  assert.equal(worktrees.disabled, false);
  assert.equal(worktrees.hasAttribute('checked'), false,
    're-enabling shutdown preserves the human-visible unticked worktree choice');

  const search = host.querySelector('#retire-preview-search');
  search.value = 'alpha';
  await harness.act(() => harness.fireEvent(search, 'input'));
  assert.equal(host.querySelectorAll('#retire-preview-list input[type="checkbox"]').length, 1);
  host.querySelector('#retire-preview-select-none').click();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#retire-preview-count').textContent, '1 of 2 selected');

  host.querySelector('#retire-preview-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(requests[0], {
    group: 'alpha team', convs: [candidates[1].conv_id],
    shutdown: true, deleteWorktree: false,
  });
  assert.notEqual(requests[0].convs[0], candidates[1].agent_id,
    'group retirement stays in the canonical conv_id identity domain');
  assert.ok(Object.isFrozen(requests[0]));
  assert.ok(Object.isFrozen(requests[0].convs));
  assert.equal(host.querySelector('#retire-preview-search').disabled, true);
  assert.equal(host.querySelector('#retire-preview-cancel').disabled, true);

  escape(harness);
  const overlay = host.querySelector('#retire-preview-modal');
  overlay.dispatchEvent(new harness.window.Event('mousedown', { bubbles: true }));
  overlay.dispatchEvent(new harness.window.Event('click', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#retire-preview-modal'), 'busy blocks Escape and backdrop dismissal');

  first.reject(new Error('retire backend unavailable'));
  await harness.act(() => first.promise.catch(() => {}));
  assert.equal(host.querySelector('[role="alert"]').textContent, 'retire backend unavailable');
  assert.equal(host.querySelector('#retire-preview-submit').textContent, 'Retry retire');
  host.querySelector('#retire-preview-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(requests.length, 2);
  assert.equal(requests[1], requests[0], 'retry reuses the exact frozen submitted request');

  second.resolve({
    members: [
      {
        conv_id: candidates[1].conv_id, title: 'Beta worker', action: 'retired',
        detail: 'demoted', worktree: { action: 'kept', detail: 'shared worktree kept' },
      },
      {
        conv_id: 'failed-1111', title: 'Failed worker', action: 'error',
        detail: 'permission cleanup failed',
      },
    ],
    warnings: ['group "other" now has no owners'],
  });
  await harness.act(() => second.promise);
  assert.match(host.querySelector('#retire-preview-hint').textContent, /1 retired.*1 failed/);
  assert.equal(host.querySelectorAll('#retire-preview-list .cleanup-row').length, 2);
  assert.match(host.querySelector('#retire-preview-list').textContent, /shared worktree kept/);
  assert.match(host.querySelector('#retire-preview-error').textContent, /no owners/);
  assert.equal(host.querySelector('#retire-preview-submit').textContent, 'Done');
  assert.equal(host.querySelector('#retire-preview-cancel'), null);
  assert.equal(finishes, 0, 'the accepted response remains mounted in a stable result phase');

  host.querySelector('#retire-preview-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(finishes, 1);
  assert.equal(host.querySelector('#retire-preview-modal'), null);
  assert.equal(harness.document.activeElement, opener);
  assert.deepEqual(await mounted.pending, {
    kind: 'retire-group-preview', response: await second.promise,
  });
  await mounted.mounted.unmount();
});

test('ungrouped retire preview uses visible-only controls but submits every checked frozen candidate', async (t) => {
  let submitted = null;
  const response = {
    retired: 1,
    skipped: 1,
    failed: 0,
    outcomes: [
      { conv_id: candidates[0].conv_id, title: 'Alpha worker', result: 'retired', detail: 'demoted' },
      { conv_id: candidates[1].conv_id, title: 'Beta worker', result: 'skipped', detail: 'already retired' },
    ],
  };
  const mounted = await openBulk(t, {
    kind: 'retire-ungrouped-preview', candidates,
  }, {
    actions: {
      retireUngroupedPreview: async (request) => { submitted = request; return response; },
    },
  });
  const { harness, host } = mounted;
  assert.equal(host.querySelector('#retire-preview-title .theme-copy-regular').textContent,
    'Retire ungrouped agents');
  assert.equal(host.querySelector('#retire-preview-title .theme-copy-wizard').textContent,
    'Banish unbound familiars');
  const search = host.querySelector('#retire-preview-search');
  search.value = 'alpha';
  await harness.act(() => harness.fireEvent(search, 'input'));
  host.querySelector('#retire-preview-select-none').click();
  await harness.act(() => Promise.resolve());
  search.value = 'beta';
  await harness.act(() => harness.fireEvent(search, 'input'));
  host.querySelector('#retire-preview-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(submitted, {
    agents: ['agt_beta'], shutdown: true, deleteWorktrees: true,
  });
  assert.notEqual(submitted.agents[0], candidates[1].conv_id,
    'ungrouped cleanup preserves the stable agent_id-first selector contract');
  assert.match(host.querySelector('#retire-preview-hint').textContent, /1 retired.*1 skipped/);
  assert.equal(host.querySelectorAll('#retire-preview-list .cleanup-row').length, 2);
  host.querySelector('#retire-preview-submit').click();
  await harness.act(() => Promise.resolve());
  await mounted.pending;
  await mounted.mounted.unmount();
});
