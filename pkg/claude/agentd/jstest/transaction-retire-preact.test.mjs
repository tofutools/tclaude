import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

async function openRetire(t, options = {}) {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, island] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-island.js'),
  ]);
  const state = createTransactionDialogState();
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const opener = harness.document.body.appendChild(harness.document.createElement('button'));
  opener.id = 'retire-opener';
  opener.focus();
  const actions = {
    close: state.close,
    loadAgentWorktree: async () => ({ kind: 'none', path: '', removable: false }),
    retireAgent: async () => {},
    handoffDangling: async () => {},
    ...options.actions,
  };
  const mounted = await harness.mount(harness.html`
    <${island.TransactionDialogApp}
      state=${state}
      actions=${actions}
      confirmDiscard=${async () => true}
    />
  `, host);
  let pending;
  await harness.act(() => {
    pending = state.open({
      kind: 'retire-agent',
      conv: options.conv || 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
      label: options.label || 'Retire target',
    });
  });
  await harness.act(() => Promise.resolve());
  return { harness, state, host, opener, actions, mounted, pending };
}

test('retire actions preserve raw conv identity, exact queries, and success outputs', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
  ]);
  const state = createTransactionDialogState();
  const requests = [];
  const notices = [];
  let refreshes = 0;
  const actions = createTransactionDialogActions({
    state,
    fetchImpl: async (url, init) => {
      requests.push([url, init]);
      return new Response(JSON.stringify({
        worktree: { action: 'scheduled', detail: 'worktree + branch will be removed after the agent exits' },
      }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    notify: (...args) => notices.push(args),
    refresh: async () => { refreshes += 1; },
    confirm: async () => false,
  });
  const conv = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
  const pending = state.open({ kind: 'retire-agent', conv, label: 'Raw target' });
  const result = await actions.retireAgent({
    conv, label: 'Raw target', shutdown: true, deleteWorktree: true,
    expectedWorktree: '/repo/worktrees/feature & review?#',
    expectedBranch: 'feat/a & b?#',
  });
  assert.equal(requests[0][0], `/api/agents/${conv}/retire?shutdown=1&delete_worktree=1`
    + '&expected_worktree=%2Frepo%2Fworktrees%2Ffeature+%26+review%3F%23'
    + '&expected_branch=feat%2Fa+%26+b%3F%23');
  assert.equal(requests[0][1].method, 'POST');
  assert.equal(requests[0][1].credentials, 'same-origin');
  assert.deepEqual(notices, [[
    'retired + session stopped: Raw target · worktree + branch will be removed after the agent exits',
  ]]);
  assert.equal(refreshes, 1);
  assert.equal(state.dialog.value, null);
  assert.deepEqual(await pending, { ok: true, response: result.response });

  const second = state.open({ kind: 'retire-agent', conv, label: 'Raw target' });
  await actions.retireAgent({ conv, label: 'Raw target', shutdown: false, deleteWorktree: false });
  assert.equal(requests[1][0], `/api/agents/${conv}/retire?shutdown=0`);
  await second;
});

test('retire worktree deletion demands a probed path and never sends one without opt-in', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
  ]);
  const state = createTransactionDialogState();
  const requests = [];
  const actions = createTransactionDialogActions({
    state,
    fetchImpl: async (url) => {
      requests.push(url);
      return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    refresh: async () => {}, notify: () => {}, confirm: async () => false,
  });
  const conv = 'raw-conv';

  // Opting into deletion without a freshly probed path is a client bug, not a
  // request the daemon should have to adjudicate: it must never reach the wire.
  for (const expectedWorktree of [undefined, '', null, 42]) {
    await assert.rejects(
      actions.retireAgent({
        conv, label: 'Target', shutdown: true, deleteWorktree: true,
        expectedWorktree, expectedBranch: 'feature',
      }),
      /freshly probed worktree path/,
    );
  }
  // The branch is half of the identity the operator confirmed, and retire
  // force-deletes it. An absent branch precondition is equally unbound — but
  // '' is a real detached-HEAD value and must NOT be treated as missing.
  for (const expectedBranch of [undefined, null, 42]) {
    await assert.rejects(
      actions.retireAgent({
        conv, label: 'Target', shutdown: true, deleteWorktree: true,
        expectedWorktree: '/repo/wt', expectedBranch,
      }),
      /freshly probed branch/,
    );
  }
  assert.deepEqual(requests, [], 'an unbound deletion opt-in is refused before any request');

  // Keep-worktree retirement stays a two-field request; a stray probed
  // identity must not smuggle a deletion precondition onto it.
  const pending = state.open({ kind: 'retire-agent', conv, label: 'Target' });
  await actions.retireAgent({
    conv, label: 'Target', shutdown: true, deleteWorktree: false,
    expectedWorktree: '/repo/wt', expectedBranch: 'feature',
  });
  assert.deepEqual(requests, [`/api/agents/${conv}/retire?shutdown=1`]);
  await pending;

  // A detached HEAD freezes '' — a bound value that must reach the wire as an
  // explicitly present, empty parameter rather than being dropped.
  const detached = state.open({ kind: 'retire-agent', conv, label: 'Target' });
  await actions.retireAgent({
    conv, label: 'Target', shutdown: true, deleteWorktree: true,
    expectedWorktree: '/repo/wt', expectedBranch: '',
  });
  assert.equal(requests[1],
    `/api/agents/${conv}/retire?shutdown=1&delete_worktree=1&expected_worktree=%2Frepo%2Fwt&expected_branch=`);
  await detached;
});

test('retire retry stays bound to the confirmed worktree after the agent moves', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, { createTransactionDialogActions }, island] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
    harness.importDashboardModule('js/transaction-dialog-island.js'),
  ]);
  const state = createTransactionDialogState();
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const conv = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
  const retireRequests = [];
  // The agent's live claim moves from worktree A to B — and switches branch in
  // place — while the frozen dialog waits for an explicit retry. Any reprobe
  // after the move would observe the new identity.
  let claimed = { path: '/repo/wt-a', branch: 'feature-a' };
  const replies = [
    async () => { throw new Error('transport failed'); },
    async () => new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } }),
  ];
  const actions = createTransactionDialogActions({
    state,
    fetchImpl: (url) => {
      if (url.endsWith('/worktree')) {
        return Promise.resolve(new Response(JSON.stringify({
          kind: 'linked', ...claimed, shared: false, removable: true,
        }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
      }
      retireRequests.push(url);
      return replies.shift()();
    },
    refresh: async () => {}, notify: () => {}, confirm: async () => false,
  });
  const mounted = await harness.mount(harness.html`
    <${island.TransactionDialogApp} state=${state} actions=${actions} confirmDiscard=${async () => true} />
  `, host);
  let completion;
  await harness.act(() => { completion = state.open({
    kind: 'retire-agent', conv, label: 'Moving target',
  }); });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.equal(host.querySelector('#retire-wt').hasAttribute('checked'), true,
    'the probed removable worktree defaults deletion ON');

  host.querySelector('#retire-ok').click();
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.equal(host.querySelector('#retire-error').textContent, 'transport failed');
  assert.equal(host.querySelector('#retire-ok').textContent, 'Retry');

  claimed = { path: '/repo/wt-b', branch: 'feature-b' };
  host.querySelector('#retire-ok').click();
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));

  const expected = `/api/agents/${conv}/retire?shutdown=1&delete_worktree=1`
    + '&expected_worktree=%2Frepo%2Fwt-a&expected_branch=feature-a';
  assert.deepEqual(retireRequests, [expected, expected],
    'the retry names the reviewed identity A, never the agent’s new claim B');
  assert.ok(!retireRequests.some((url) => url.includes('wt-b')),
    'a moved claim can never be retargeted by a retry');
  assert.ok(!retireRequests.some((url) => url.includes('feature-b')),
    'a branch switched in after confirmation can never be retargeted by a retry');
  assert.deepEqual(await completion, { ok: true, response: {} });
  await mounted.unmount();
});

test('retire action errors leave the frozen transaction mounted and dangling handoff is explicit', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
  ]);
  const state = createTransactionDialogState();
  const conv = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
  const descriptor = { kind: 'retire-agent', conv, label: 'Frozen target' };
  const completion = state.open(descriptor);
  const frozenDescriptor = state.dialog.value.descriptor;
  const transport = createTransactionDialogActions({
    state,
    fetchImpl: async () => { throw new Error('network down'); },
    refresh: async () => {}, notify: () => {}, confirm: async () => false,
  });
  await assert.rejects(
    transport.retireAgent({ conv, label: descriptor.label, shutdown: false, deleteWorktree: false }),
    /network down/,
  );
  assert.equal(state.dialog.value.descriptor.conv, conv);

  const server = createTransactionDialogActions({
    state,
    fetchImpl: async () => new Response('retire backend unavailable', { status: 503 }),
    refresh: async () => {}, notify: () => {}, confirm: async () => false,
  });
  await assert.rejects(
    server.retireAgent({ conv, label: descriptor.label, shutdown: true, deleteWorktree: false }),
    /retire backend unavailable/,
  );
  assert.equal(state.dialog.value.descriptor, frozenDescriptor,
    'non-dangling HTTP errors do not close or replace the transaction');

  let confirmedWhileOpen = null;
  const requests = [];
  const notices = [];
  let refreshes = 0;
  const dangling = createTransactionDialogActions({
    state,
    fetchImpl: async (url, init) => {
      requests.push([url, init]);
      if (init.method === 'DELETE') return new Response(null, { status: 204 });
      return new Response(JSON.stringify({ dangling: true, conv_id: 'confirmed-conv-id' }), {
        status: 409, headers: { 'Content-Type': 'application/json' },
      });
    },
    confirm: async (copy) => {
      confirmedWhileOpen = state.dialog.value;
      assert.equal(copy.title, 'Remove dangling agent entry?');
      assert.equal(copy.meta, 'Frozen target');
      return true;
    },
    notify: (...args) => notices.push(args),
    refresh: async () => { refreshes += 1; },
  });
  const outcome = await dangling.retireAgent({
    conv, label: descriptor.label, shutdown: true, deleteWorktree: false,
  });
  assert.deepEqual(outcome, { dangling: true, convID: 'confirmed-conv-id' });
  await dangling.handoffDangling({ ...outcome, conv, label: descriptor.label });
  assert.equal(confirmedWhileOpen, null, 'retire dialog closes before shell confirmation takes ownership');
  assert.equal(requests.at(-1)[0], '/api/agents/confirmed-conv-id');
  assert.equal(requests.at(-1)[1].method, 'DELETE');
  assert.deepEqual(notices, [['removed dangling entry: Frozen target']]);
  assert.equal(refreshes, 1);
  assert.deepEqual(await completion, {
    dangling: true, removed: true, convID: 'confirmed-conv-id', reason: 'removed',
  });
});

test('dangling handoff resolves every non-success branch without leaking ownership', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
  ]);
  const cases = [
    {
      name: 'declined',
      confirm: async () => false,
      fetchImpl: async () => { throw new Error('DELETE must not run'); },
      reason: 'declined', notice: ['dangling entry kept'],
    },
    {
      name: 'confirm failure',
      confirm: async () => { throw new Error('confirm unavailable'); },
      fetchImpl: async () => { throw new Error('DELETE must not run'); },
      reason: 'confirm_failed', notice: ['Remove failed: confirm unavailable', true],
    },
    {
      name: 'delete transport failure',
      confirm: async () => true,
      fetchImpl: async () => { throw new Error('socket closed'); },
      reason: 'transport_failed', notice: ['Remove failed: socket closed', true],
    },
    {
      name: 'delete HTTP failure',
      confirm: async () => true,
      fetchImpl: async () => new Response('delete refused', { status: 503 }),
      reason: 'http_failed', notice: ['Remove failed: delete refused', true],
    },
    {
      name: 'delete HTTP failure with unreadable body',
      confirm: async () => true,
      fetchImpl: async () => ({
        ok: false,
        status: 503,
        text: async () => { throw new Error('body stream broke'); },
      }),
      reason: 'http_failed',
      notice: ['Remove failed: HTTP 503 (response body unreadable)', true],
    },
  ];
  for (const row of cases) {
    await t.test(row.name, async () => {
      const state = createTransactionDialogState();
      const notices = [];
      const pending = state.open({ kind: 'retire-agent', conv: 'raw-conv', label: 'Dangling' });
      const actions = createTransactionDialogActions({
        state, fetchImpl: row.fetchImpl, confirm: row.confirm,
        notify: (...args) => notices.push(args), refresh: async () => {},
      });
      const result = await actions.handoffDangling({
        dangling: true, convID: 'daemon-raw-conv', conv: 'raw-conv', label: 'Dangling',
      });
      assert.deepEqual(result, {
        dangling: true, removed: false, convID: 'daemon-raw-conv', reason: row.reason,
      });
      assert.deepEqual(await pending, result);
      assert.deepEqual(notices, [row.notice]);
      assert.equal(state.dialog.value, null);
      const next = state.open({ kind: 'retire-agent', conv: 'next' });
      assert.ok(state.dialog.value, 'every failed/declined handoff releases ownership after resolving');
      state.close();
      await next;
    });
  }
});

test('notification failures cannot orphan visual handoff ownership', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
  ]);
  for (const row of [
    { name: 'normal success', run: (actions) => actions.retireAgent({
      conv: 'raw-conv', label: 'Target', shutdown: false, deleteWorktree: false,
    }) },
    { name: 'dangling decline', run: (actions) => actions.handoffDangling({
      dangling: true, convID: 'raw-conv', conv: 'raw-conv', label: 'Target',
    }) },
  ]) {
    await t.test(row.name, async () => {
      const state = createTransactionDialogState();
      const pending = state.open({ kind: 'retire-agent', conv: 'raw-conv', label: 'Target' });
      const actions = createTransactionDialogActions({
        state,
        fetchImpl: async () => new Response('{}', {
          status: 200, headers: { 'Content-Type': 'application/json' },
        }),
        confirm: async () => false,
        notify: () => { throw new Error('notification sink unavailable'); },
        refresh: async () => {},
      });
      await row.run(actions);
      const result = await pending;
      assert.ok(result?.ok || (result?.dangling && result.removed === false));
      const next = state.open({ kind: 'retire-agent', conv: 'next' });
      assert.ok(state.dialog.value, 'notification failure must release transaction ownership');
      state.close();
      await next;
    });
  }
});

test('retire controller freezes launcher descriptors and classifies DnD reconciliation', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, controller] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-controller.js'),
  ]);
  const state = createTransactionDialogState();
  const unregister = controller.registerTransactionDialogController(state);
  const pending = controller.openRetireAgentDialog('raw-conv-id', 'Raw label');
  assert.deepEqual(state.dialog.value.descriptor, {
    kind: 'retire-agent', conv: 'raw-conv-id', label: 'Raw label',
  });
  assert.ok(Object.isFrozen(state.dialog.value.descriptor));
  assert.equal(controller.retireResultNeedsReconcile(null), true);
  assert.equal(controller.retireResultNeedsReconcile({ dangling: true, removed: false }), true);
  assert.equal(controller.retireResultNeedsReconcile({ dangling: true, removed: true }), false);
  assert.equal(controller.retireResultNeedsReconcile({ ok: true }), false);
  state.close();
  await pending;
  unregister();
});

test('retire renderer preserves copy, corrected worktree defaults, coupling, and focus', async (t) => {
  const probe = deferred();
  const mounted = await openRetire(t, {
    actions: { loadAgentWorktree: (_conv, options) => {
      assert.equal(options.signal.aborted, false);
      return probe.promise;
    } },
  });
  const { harness, host, opener } = mounted;
  assert.ok(host.querySelector('#retire-modal'));
  assert.equal(host.querySelectorAll('#retire-modal').length, 1);
  assert.equal(host.querySelector('#retire-title .retire-title-regular').textContent, 'Retire this agent?');
  assert.equal(host.querySelector('#retire-title .retire-title-wizard').textContent, 'Banish this familiar?');
  assert.match(host.querySelector('#retire-modal').textContent, /non-destructive soft-delete/);
  assert.match(host.querySelector('#retire-modal .theme-copy-wizard').textContent, /plain conversation/,
    'wizard copy is rendered concurrently so a live theme flip cannot reset state');
  assert.equal(host.querySelector('#retire-wt-row'), null, 'worktree row stays hidden while probing');
  assert.equal(host.querySelector('#retire-shutdown').hasAttribute('checked'), true);
  assert.equal(harness.document.activeElement.id, 'retire-ok');

  probe.resolve({
    kind: 'linked', path: '/tmp/retire-wt', branch: 'feature', shared: false, removable: true,
  });
  await harness.act(() => Promise.resolve());
  const worktree = host.querySelector('#retire-wt');
  assert.ok(worktree);
  assert.equal(worktree.disabled, false);
  assert.equal(worktree.hasAttribute('checked'), true,
    'a removable probe defaults deletion ON while shutdown is ON');
  assert.match(host.querySelector('#retire-wt-label').textContent, /removed after the agent exits/);

  const shutdown = host.querySelector('#retire-shutdown');
  shutdown.checked = false;
  await harness.act(() => harness.fireEvent(shutdown, 'change'));
  assert.equal(worktree.disabled, true);
  assert.equal(worktree.hasAttribute('checked'), false);
  assert.match(host.querySelector('#retire-wt-label').textContent, /requires shutting down/);
  shutdown.checked = true;
  await harness.act(() => harness.fireEvent(shutdown, 'change'));
  assert.equal(worktree.disabled, false);
  assert.equal(worktree.hasAttribute('checked'), true,
    're-enabling shutdown restores the removable default');

  host.querySelector('#retire-cancel').click();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#retire-modal'), null);
  assert.equal(harness.document.activeElement, opener);
  assert.equal(await mounted.pending, null);
  await mounted.mounted.unmount();
});

test('retire worktree probe distinguishes absent, main, and shared states', async (t) => {
  for (const row of [
    { name: 'absent', worktree: { kind: 'none', path: '', removable: false }, visible: false, copy: '' },
    { name: 'main', worktree: { kind: 'main', path: '/repo', branch: 'main', removable: false }, visible: true, copy: 'main worktree' },
    { name: 'shared', worktree: { kind: 'linked', path: '/repo/wt', branch: 'feature', shared: true, removable: false }, visible: true, copy: 'shared with another agent' },
  ]) {
    await t.test(row.name, async (t) => {
      const mounted = await openRetire(t, {
        actions: { loadAgentWorktree: async () => row.worktree },
      });
      await mounted.harness.act(() => Promise.resolve());
      const wtRow = mounted.host.querySelector('#retire-wt-row');
      assert.equal(!!wtRow, row.visible);
      if (row.visible) {
        assert.equal(wtRow.querySelector('input').disabled, true);
        assert.equal(wtRow.querySelector('input').hasAttribute('checked'), false);
        assert.match(wtRow.textContent, new RegExp(row.copy));
      }
      mounted.state.close();
      await mounted.harness.act(() => Promise.resolve());
      await mounted.mounted.unmount();
    });
  }
});

test('retire renderer aborts stale probes and ignores their late generations', async (t) => {
  const first = deferred();
  const second = deferred();
  const signals = [];
  let calls = 0;
  const mounted = await openRetire(t, {
    actions: { loadAgentWorktree: (_conv, { signal }) => {
      signals.push(signal);
      calls += 1;
      return calls === 1 ? first.promise : second.promise;
    } },
  });
  mounted.state.close();
  await mounted.harness.act(() => Promise.resolve());
  assert.equal(signals[0].aborted, true);

  let pendingSecond;
  await mounted.harness.act(() => {
    pendingSecond = mounted.state.open({
      kind: 'retire-agent', conv: 'bbbbbbbb-cccc-dddd-eeee-ffffffffffff', label: 'Second target',
    });
  });
  await mounted.harness.act(() => Promise.resolve());
  assert.equal(calls, 2, 'the reopened keyed dialog starts a fresh probe generation');
  first.resolve({
    kind: 'linked', path: '/stale', branch: 'stale', removable: true,
  });
  await mounted.harness.act(() => Promise.resolve());
  assert.equal(mounted.host.querySelector('#retire-wt-row'), null,
    'a prior generation cannot paint the reopened transaction');
  second.resolve({
    kind: 'linked', path: '/current', branch: 'current', removable: true,
  });
  await mounted.harness.act(() => Promise.resolve());
  assert.match(mounted.host.querySelector('#retire-wt-label').textContent, /\/current/);
  mounted.state.close();
  await mounted.harness.act(() => Promise.resolve());
  assert.equal(await pendingSecond, null);
  await mounted.mounted.unmount();
});

test('retire failure stays inline with frozen choices and explicit retry', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, { createTransactionDialogActions }, island] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
    harness.importDashboardModule('js/transaction-dialog-island.js'),
  ]);
  const state = createTransactionDialogState();
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const requests = [];
  const first = deferred();
  const second = deferred();
  const replies = [
    () => first.promise,
    () => second.promise,
    async () => new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } }),
  ];
  const notices = [];
  let refreshes = 0;
  const actions = createTransactionDialogActions({
    state,
    fetchImpl: (url, init) => {
      requests.push([url, init]);
      if (url.endsWith('/worktree')) {
        return Promise.resolve(new Response(JSON.stringify({ kind: 'none', path: '', removable: false }), {
          status: 200, headers: { 'Content-Type': 'application/json' },
        }));
      }
      return replies.shift()();
    },
    refresh: async () => { refreshes += 1; },
    notify: (...args) => notices.push(args),
    confirm: async () => false,
  });
  const mounted = await harness.mount(harness.html`
    <${island.TransactionDialogApp} state=${state} actions=${actions} confirmDiscard=${async () => true} />
  `, host);
  let completion;
  await harness.act(() => { completion = state.open({
    kind: 'retire-agent', conv: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee', label: 'Retry target',
  }); });
  await harness.act(() => Promise.resolve());
  const shutdown = host.querySelector('#retire-shutdown');
  shutdown.checked = false;
  await harness.act(() => harness.fireEvent(shutdown, 'change'));
  host.querySelector('#retire-ok').click();
  await harness.act(() => Promise.resolve());
  assert.equal(requests.filter(([url]) => url.includes('/retire?')).length, 1);
  assert.equal(host.querySelector('#retire-ok').getAttribute('aria-busy'), 'true');
  assert.equal(host.querySelector('#retire-ok .theme-copy-regular').textContent, 'Retiring…');
  assert.equal(host.querySelector('#retire-ok .theme-copy-wizard').textContent, 'Banishing…');
  assert.equal(host.querySelector('#retire-cancel').disabled, true);

  const escape = new harness.window.Event('keydown', { bubbles: true });
  Object.defineProperty(escape, 'key', { value: 'Escape' });
  harness.document.dispatchEvent(escape);
  const overlay = host.querySelector('#retire-modal');
  overlay.dispatchEvent(new harness.window.Event('mousedown', { bubbles: true }));
  overlay.dispatchEvent(new harness.window.Event('click', { bubbles: true }));
  host.querySelector('#retire-ok').click();
  assert.equal(requests.filter(([url]) => url.includes('/retire?')).length, 1,
    'busy modal blocks dismissal and duplicate submit');

  first.resolve(new Response('backend refused', { status: 503 }));
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.ok(host.querySelector('#retire-modal'), 'HTTP failure stays mounted');
  assert.equal(host.querySelector('#retire-error').getAttribute('role'), 'alert');
  assert.equal(host.querySelector('#retire-error').textContent, 'backend refused');
  assert.equal(host.querySelector('#retire-shutdown').checked, false);
  assert.equal(host.querySelector('#retire-shutdown').disabled, true,
    'the first submitted choice is frozen for explicit retry');
  assert.equal(host.querySelector('#retire-ok').textContent, 'Retry');

  host.querySelector('#retire-ok').click();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#retire-ok .theme-copy-regular').textContent, 'Retiring…');
  assert.equal(host.querySelector('#retire-ok .theme-copy-wizard').textContent, 'Banishing…');
  second.reject(new Error('transport failed again'));
  await harness.act(() => second.promise.catch(() => {}));
  assert.equal(host.querySelector('#retire-error').textContent, 'transport failed again');
  assert.ok(host.querySelector('#retire-modal'), 'transport failure also stays mounted');
  host.querySelector('#retire-ok').click();
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  const retireRequests = requests.filter(([url]) => url.includes('/retire?'));
  assert.equal(retireRequests.length, 3);
  assert.deepEqual(retireRequests.map(([url]) => url), [
    '/api/agents/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/retire?shutdown=0',
    '/api/agents/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/retire?shutdown=0',
    '/api/agents/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/retire?shutdown=0',
  ]);
  assert.equal(host.querySelector('#retire-modal'), null);
  assert.deepEqual(notices, [['retired: Retry target']]);
  assert.equal(refreshes, 1);
  assert.deepEqual(await completion, { ok: true, response: {} });
  await mounted.unmount();
});

test('concrete retire dialog yields Escape to a higher painted overlay', async (t) => {
  const mounted = await openRetire(t);
  const blocker = mounted.harness.document.body.appendChild(mounted.harness.document.createElement('div'));
  blocker.id = 'higher-overlay';
  blocker.className = 'modal-overlay show';
  blocker.style.zIndex = '999';
  mounted.host.querySelector('#retire-modal').style.zIndex = '100';
  const escape = () => {
    const event = new mounted.harness.window.Event('keydown', { bubbles: true });
    Object.defineProperty(event, 'key', { value: 'Escape' });
    mounted.harness.document.dispatchEvent(event);
  };
  escape();
  await mounted.harness.act(() => Promise.resolve());
  assert.ok(mounted.host.querySelector('#retire-modal'));
  blocker.remove();
  escape();
  await mounted.harness.act(() => Promise.resolve());
  assert.equal(mounted.host.querySelector('#retire-modal'), null);
  await mounted.mounted.unmount();
});
