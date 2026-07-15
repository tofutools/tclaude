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

const day = 86400000;
const now = Date.now();
const candidates = [
  {
    agent_id: 'agt_new', conv_id: 'newest-1111-2222-3333-444444444444',
    title: 'Newest retired', retired_at: new Date(now - day).toISOString(),
    retired_by_display: 'Lead agent', online: false,
  },
  {
    agent_id: 'agt_old', conv_id: 'older-1111-2222-3333-444444444444',
    title: 'Older retired', retired_at: new Date(now - (10 * day)).toISOString(),
    retired_by: 'agt_lead', online: false,
  },
  {
    agent_id: 'agt_invalid', conv_id: 'invalid-1111-2222-3333-444444444444',
    title: 'Invalid timestamp', retired_at: 'not-a-time', online: true,
  },
  {
    agent_id: 'agt_future', conv_id: 'future-1111-2222-3333-444444444444',
    title: 'Future timestamp', retired_at: new Date(now + day).toISOString(), online: false,
  },
];

async function openDeleteRetired(t, options = {}) {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, island] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-island.js'),
  ]);
  const state = createTransactionDialogState();
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const opener = harness.document.body.appendChild(harness.document.createElement('button'));
  opener.id = 'delete-retired-opener';
  opener.focus();
  const actions = {
    close: state.close,
    deleteRetiredPreview: async () => ({}),
    finishDeleteRetired: async (result) => {
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
  await harness.act(() => {
    pending = state.open({
      kind: 'delete-retired-preview', candidates: options.candidates || candidates,
    });
  });
  await harness.act(() => Promise.resolve());
  return { harness, state, host, opener, actions, mounted, pending };
}

test('delete-retired launcher normalizes, newest-sorts, dedupes, and freezes the complete roster', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, controller] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-controller.js'),
  ]);
  const state = createTransactionDialogState();
  const unregister = controller.registerTransactionDialogController(state);
  const source = [
    { ...candidates[1] }, { ...candidates[2] }, { ...candidates[0] }, { ...candidates[3] },
    { ...candidates[0], title: 'duplicate must not replace first' },
  ];
  const pending = controller.openDeleteRetiredPreviewDialog(source);
  const descriptor = state.dialog.value.descriptor;
  assert.equal(descriptor.kind, 'delete-retired-preview');
  assert.deepEqual(descriptor.candidates.map((candidate) => candidate.conv_id), [
    candidates[3].conv_id, candidates[0].conv_id, candidates[1].conv_id, candidates[2].conv_id,
  ]);
  assert.equal(descriptor.candidates[1].retired_by, 'Lead agent');
  assert.equal(descriptor.candidates[2].retired_by, 'agt_lead');
  assert.ok(Object.isFrozen(descriptor));
  assert.ok(Object.isFrozen(descriptor.candidates));
  assert.ok(Object.isFrozen(descriptor.candidates[0]));
  source[3].title = 'mutated after open';
  source.push({ ...candidates[0], conv_id: 'late' });
  assert.equal(descriptor.candidates[0].title, 'Future timestamp');
  assert.equal(descriptor.candidates.length, 4);
  state.close();
  await pending;
  unregister();
});

test('delete-retired actions preserve the explicit cleanup wire contract and refresh only on result close', async (t) => {
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
      return new Response(JSON.stringify({ deleted: 2, outcomes: [] }), {
        status: 200, headers: { 'Content-Type': 'application/json' },
      });
    },
    refresh: async () => { refreshes += 1; },
    notify: () => {},
  });
  const request = Object.freeze({
    agents: Object.freeze(['agt_new', candidates[1].conv_id]), deleteWorktrees: true,
  });
  assert.deepEqual(await actions.deleteRetiredPreview(request), { deleted: 2, outcomes: [] });
  assert.equal(requests[0][0], '/api/cleanup/agents');
  assert.equal(requests[0][1].method, 'POST');
  assert.equal(requests[0][1].credentials, 'same-origin');
  assert.deepEqual(JSON.parse(requests[0][1].body), {
    agents: request.agents, mode: 'delete', delete_worktrees: true,
  });
  assert.equal(refreshes, 0, 'an accepted POST does not refresh beneath the stable result');

  const completion = state.open({ kind: 'delete-retired-preview', candidates });
  const result = { kind: 'delete-retired-preview', response: { deleted: 2 } };
  assert.deepEqual(await actions.finishDeleteRetired(result), result);
  assert.equal(refreshes, 1);
  assert.equal(state.dialog.value, null);
  assert.deepEqual(await completion, result);
});

test('delete-retired submits only checked visible identities and failure returns to an editable retry', async (t) => {
  const first = deferred();
  const second = deferred();
  const requests = [];
  let finishes = 0;
  const opened = await openDeleteRetired(t, {
    actions: {
      deleteRetiredPreview: (request) => {
        requests.push(request);
        return requests.length === 1 ? first.promise : second.promise;
      },
      finishDeleteRetired: async (result) => {
        finishes += 1;
        opened.state.handoff();
        opened.state.finish(result);
        return result;
      },
    },
  });
  const { harness, host, opener } = opened;
  assert.equal(host.querySelectorAll('#delete-retired-modal').length, 1);
  assert.equal(host.querySelector('#delete-retired-count').textContent, '4 of 4 selected');
  assert.equal(harness.document.activeElement.id, 'delete-retired-submit');
  assert.deepEqual(
    [...host.querySelectorAll('#delete-retired-list .title')].map((node) => node.textContent),
    candidates.map((candidate) => candidate.title),
  );

  const age = host.querySelector('#delete-retired-age');
  age.value = '5';
  await harness.act(() => harness.fireEvent(age, 'input'));
  assert.deepEqual(
    [...host.querySelectorAll('#delete-retired-list .title')].map((node) => node.textContent),
    ['Older retired', 'Invalid timestamp'],
    'invalid timestamps are infinitely old while young and future rows fail a positive floor',
  );
  host.querySelector('#delete-retired-select-none').click();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#delete-retired-count').textContent, '0 of 4 selected');

  age.value = '0';
  await harness.act(() => harness.fireEvent(age, 'input'));
  const search = host.querySelector('#delete-retired-search');
  search.value = 'newest';
  await harness.act(() => harness.fireEvent(search, 'input'));
  assert.equal(host.querySelector('#delete-retired-count').textContent, '1 of 4 selected');
  host.querySelector('#delete-retired-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(requests[0], { agents: ['agt_new'], deleteWorktrees: false });
  assert.ok(Object.isFrozen(requests[0]));
  assert.ok(Object.isFrozen(requests[0].agents));
  assert.equal(host.querySelector('#delete-retired-search').disabled, true);
  assert.equal(host.querySelector('#delete-retired-cancel').disabled, true);

  escape(harness);
  const overlay = host.querySelector('#delete-retired-modal');
  overlay.dispatchEvent(new harness.window.Event('mousedown', { bubbles: true }));
  overlay.dispatchEvent(new harness.window.Event('click', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#delete-retired-modal'), 'busy blocks Escape and backdrop dismissal');

  first.reject(new Error('cleanup backend unavailable'));
  await harness.act(() => first.promise.catch(() => {}));
  assert.equal(host.querySelector('[role="alert"]').textContent, 'cleanup backend unavailable');
  assert.equal(host.querySelector('#delete-retired-submit').textContent, 'Retry delete');
  assert.equal(host.querySelector('#delete-retired-search').disabled, false);
  assert.equal(host.querySelector('#delete-retired-age').disabled, false);
  assert.equal(host.querySelector('#delete-retired-wt').disabled, false);

  search.value = 'future';
  await harness.act(() => harness.fireEvent(search, 'input'));
  const worktrees = host.querySelector('#delete-retired-wt');
  worktrees.checked = true;
  await harness.act(() => harness.fireEvent(worktrees, 'change'));
  host.querySelector('#delete-retired-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(requests[1], { agents: ['agt_future'], deleteWorktrees: true });
  assert.notEqual(requests[1], requests[0], 'editable retry freezes a new human-approved attempt');

  second.resolve({
    deleted: 1, skipped: 1, failed: 1,
    outcomes: [
      { conv_id: candidates[3].conv_id, title: 'Future timestamp', result: 'deleted', detail: 'purged' },
      { conv_id: 'failed-1111', title: 'Failed delete', result: 'error', detail: 'disk busy' },
    ],
    warnings: ['one linked worktree was kept'],
  });
  await harness.act(() => second.promise);
  assert.match(host.querySelector('#delete-retired-hint').textContent,
    /1 deleted.*1 skipped.*1 failed/);
  assert.equal(host.querySelectorAll('#delete-retired-list .cleanup-row').length, 2);
  assert.match(host.querySelector('#delete-retired-list').textContent, /disk busy/);
  assert.match(host.querySelector('#delete-retired-error').textContent, /worktree was kept/);
  assert.equal(host.querySelector('#delete-retired-submit').textContent, 'Done');
  assert.equal(host.querySelector('#delete-retired-cancel'), null);
  assert.equal(finishes, 0, 'the per-item result stays mounted until the human closes it');

  host.querySelector('#delete-retired-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(finishes, 1);
  assert.equal(host.querySelector('#delete-retired-modal'), null);
  assert.equal(harness.document.activeElement, opener);
  assert.deepEqual(await opened.pending, {
    kind: 'delete-retired-preview', response: await second.promise,
  });
  await opened.mounted.unmount();
});
