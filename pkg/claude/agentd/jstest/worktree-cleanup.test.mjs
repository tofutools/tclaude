import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function worktree(path, overrides = {}) {
  return {
    path,
    branch: 'feature',
    category: 'orphan',
    is_main: false,
    checked: true,
    dirty: false,
    agents: [],
    reason: 'safe to remove',
    ...overrides,
  };
}

function initialScan() {
  return {
    repoRoots: ['/repo'],
    worktrees: [
      worktree('/repo', { branch: 'main', category: 'main', is_main: true, checked: false }),
      worktree('/repo-orphan'),
      worktree('/repo-agent', {
        branch: 'agent-topic', category: 'agent', checked: false,
        agents: [{ title: 'Builder', conv_id: 'builder-conv' }],
        reason: 'belongs to agent Builder — deleting breaks its resume',
      }),
      worktree('/repo-dirty', { branch: 'dirty-topic', checked: false, dirty: true }),
    ],
  };
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

async function flush(harness, turns = 8) {
  await harness.act(async () => {
    for (let turn = 0; turn < turns; turn += 1) await Promise.resolve();
  });
}

async function mountCleanup(t, actionOverrides = {}, group = 'alpha') {
  const harness = await createPreactHarness(t);
  const [island, { createWorktreeCleanupState }] = await Promise.all([
    harness.importDashboardModule('js/worktree-cleanup-island.js'),
    harness.importDashboardModule('js/worktree-cleanup-state.js'),
  ]);
  const state = createWorktreeCleanupState();
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const actions = {
    scan: async () => initialScan(),
    cleanup: async () => ({ removed: 0, branches: 0, skipped: 0, failed: 0, outcomes: [] }),
    ...actionOverrides,
  };
  await harness.mount(harness.html`<${island.WorktreeCleanupApp}
    state=${state} actions=${actions}
  />`, host);
  const pending = state.open(group);
  await flush(harness);
  return { harness, state, host, actions, pending };
}

function checkbox(host, path) {
  return host.querySelector(`input[data-path="${path}"]`);
}

function isChecked(host, path) {
  const control = checkbox(host, path);
  if (!control) return false;
  return control.checked === undefined ? control.hasAttribute('checked') : control.checked === true;
}

test('Preact cleanup owner preserves safety, full-bucket toggles, and visible selection controls', async (t) => {
  const opened = await mountCleanup(t);
  const { harness, host, pending } = opened;
  assert.match(host.querySelector('#worktree-cleanup-title').textContent, /alpha/);
  assert.equal(host.querySelector('#worktree-cleanup-count').textContent.trim(), '1 of 3 selected');
  assert.equal(checkbox(host, '/repo').disabled, true, 'main worktree is visible but never selectable');
  assert.equal(isChecked(host, '/repo'), false);
  assert.match(host.querySelector('#worktree-cleanup-categories').textContent, /orphans 1\/2/);
  assert.match(host.querySelector('#worktree-cleanup-categories').textContent, /agent-bound.*0\/1/);

  host.querySelector('[data-cat="agent"]').click();
  await flush(harness);
  assert.equal(isChecked(host, '/repo-agent'), true,
    'category chips toggle their complete bucket independent of filtering');
  host.querySelector('[data-dirty="1"]').click();
  await flush(harness);
  assert.equal(isChecked(host, '/repo-dirty'), true);
  assert.equal(host.querySelector('#worktree-cleanup-count').textContent.trim(), '3 of 3 selected');

  const search = host.querySelector('#worktree-cleanup-search');
  search.value = 'orphan';
  await harness.act(() => harness.fireEvent(search, 'input'));
  assert.equal(host.querySelectorAll('#worktree-cleanup-list input[type="checkbox"]').length, 1);
  host.querySelector('#worktree-cleanup-select-none').click();
  await flush(harness);
  assert.equal(isChecked(host, '/repo-orphan'), false);
  assert.equal(host.querySelector('#worktree-cleanup-count').textContent.trim(), '2 of 3 selected',
    'select none affects only the filtered visible removable set');
  search.value = '';
  await harness.act(() => harness.fireEvent(search, 'input'));
  assert.equal(isChecked(host, '/repo-agent'), true);
  assert.equal(isChecked(host, '/repo-dirty'), true);

  host.querySelector('#worktree-cleanup-cancel').click();
  assert.equal(await pending, null);
  await flush(harness);
});

test('rescan preserves present exact-path choices but forgets them after successful absence', async (t) => {
  let scans = 0;
  const opened = await mountCleanup(t, {
    scan: async () => {
      scans += 1;
      if (scans === 1) return initialScan();
      if (scans === 2) return {
        repoRoots: ['/repo'],
        worktrees: [
          worktree('/repo-orphan', { category: 'live', checked: true, reason: 'in use by a running agent' }),
          worktree('/repo-agent', { category: 'retired', checked: true }),
          worktree('/repo-orphan-child', { category: 'orphan', checked: false }),
        ],
      };
      if (scans === 3) return {
        repoRoots: ['/repo'],
        worktrees: [
          worktree('/repo-agent', { category: 'retired', checked: true }),
          worktree('/repo-orphan-child', { category: 'orphan', checked: false }),
        ],
      };
      return {
        repoRoots: ['/repo'],
        worktrees: [
          worktree('/repo-orphan', {
            category: 'agent', checked: false, dirty: true,
            agents: [{ title: 'Replacement', conv_id: 'replacement-conv' }],
            reason: 'belongs to agent Replacement — deleting breaks its resume',
          }),
          worktree('/repo-agent', { category: 'retired', checked: true }),
          worktree('/repo-orphan-child', { category: 'orphan', checked: false }),
        ],
      };
    },
  });
  const { harness, host, pending } = opened;
  const orphan = checkbox(host, '/repo-orphan');
  orphan.checked = false;
  await harness.act(() => harness.fireEvent(orphan, 'change'));
  host.querySelector('#worktree-cleanup-rescan').click();
  await flush(harness);

  assert.equal(isChecked(host, '/repo-orphan'), false,
    'the exact manually touched path keeps its explicit opt-out');
  assert.match(host.querySelector('[data-path="/repo-orphan"]').textContent, /live/,
    'fresh server classification is retained');
  assert.equal(isChecked(host, '/repo-agent'), true,
    'an untouched row accepts the fresh server smart default');
  assert.equal(isChecked(host, '/repo-orphan-child'), false,
    'a similar but new path does not inherit a touched prefix choice');

  orphan.checked = true;
  await harness.act(() => harness.fireEvent(orphan, 'change'));
  host.querySelector('#worktree-cleanup-rescan').click();
  await flush(harness);
  assert.equal(checkbox(host, '/repo-orphan'), null,
    'a successful rescan can establish that the touched path is absent');

  host.querySelector('#worktree-cleanup-rescan').click();
  await flush(harness);
  assert.equal(isChecked(host, '/repo-orphan'), false,
    'a replacement at the same path uses its new safe server default');
  const replacement = host.querySelector('[data-path="/repo-orphan"]');
  assert.match(replacement.querySelector('.cat-agent').textContent, /agent/);
  assert.match(replacement.querySelector('.dirty').textContent, /uncommitted/);
  assert.match(replacement.textContent, /Replacement/,
    'replacement classification and safety metadata come from the fresh scan');
  host.querySelector('#worktree-cleanup-cancel').click();
  await pending;
  await flush(harness);
});

test('failed rescan does not erase a touched choice', async (t) => {
  let scans = 0;
  const opened = await mountCleanup(t, {
    scan: async () => {
      scans += 1;
      if (scans === 1) return initialScan();
      if (scans === 2) throw new Error('temporary scan failure');
      return {
        repoRoots: ['/repo'],
        worktrees: [worktree('/repo-orphan', { checked: true })],
      };
    },
  });
  const { harness, host, pending } = opened;
  const orphan = checkbox(host, '/repo-orphan');
  orphan.checked = false;
  await harness.act(() => harness.fireEvent(orphan, 'change'));

  host.querySelector('#worktree-cleanup-rescan').click();
  await flush(harness);
  assert.match(host.querySelector('#worktree-cleanup-error').textContent, /temporary scan failure/);

  host.querySelector('#worktree-cleanup-rescan').click();
  await flush(harness);
  assert.equal(isChecked(host, '/repo-orphan'), false,
    'failure did not establish absence, so the exact touched choice survives');
  host.querySelector('#worktree-cleanup-cancel').click();
  await pending;
  await flush(harness);
});

test('failed cleanup freezes an exact retry and successful partial outcomes remain until Done', async (t) => {
  const requests = [];
  const opened = await mountCleanup(t, {
    cleanup: async (request) => {
      requests.push(request);
      if (requests.length === 1) throw new Error('server recheck failed');
      return {
        removed: 1, branches: 1, skipped: 1, failed: 1,
        outcomes: [
          { path: '/repo-orphan', branch: 'feature', result: 'removed_with_branch', detail: 'gone' },
          { path: '/repo', branch: 'main', result: 'skipped', detail: 'main repo — never removed' },
          { path: '/repo-dirty', branch: 'dirty-topic', result: 'failed', detail: 'git refused' },
        ],
      };
    },
  });
  const { harness, host, pending } = opened;
  let settled = false;
  pending.finally(() => { settled = true; });
  host.querySelector('#worktree-cleanup-submit').click();
  await flush(harness);
  assert.match(host.querySelector('#worktree-cleanup-error').textContent, /server recheck failed/);
  assert.equal(host.querySelector('#worktree-cleanup-search').disabled, true,
    'the approved path set freezes after the first destructive attempt');
  assert.match(host.querySelector('#worktree-cleanup-submit').textContent, /Retry Remove/);

  host.querySelector('#worktree-cleanup-submit').click();
  await flush(harness);
  assert.equal(requests.length, 2);
  assert.equal(requests[1], requests[0], 'retry reuses the same frozen request identity');
  assert.deepEqual(requests[0], { paths: ['/repo-orphan'], deleteBranches: true });
  assert.match(host.querySelector('#worktree-cleanup-hint').textContent,
    /removed 1 worktree.*1 skipped.*1 failed/);
  assert.match(host.querySelector('#worktree-cleanup-list').textContent, /main repo — never removed/);
  assert.match(host.querySelector('#worktree-cleanup-list').textContent, /git refused/);
  assert.equal(settled, false, 'HTTP 200 does not close the result phase');
  assert.equal(host.querySelector('#worktree-cleanup-cancel'), null);
  assert.equal(harness.document.activeElement, host.querySelector('#worktree-cleanup-submit'),
    'the result phase moves modal focus to Done');
  host.querySelector('#worktree-cleanup-submit').click();
  assert.equal((await pending).response.failed, 1);
  await flush(harness);
});

test('a synchronous double activation cannot issue duplicate destructive cleanup requests', async (t) => {
  const first = deferred();
  const second = deferred();
  const requests = [];
  const opened = await mountCleanup(t, {
    cleanup: (request) => {
      requests.push(request);
      return requests.length === 1 ? first.promise : second.promise;
    },
  });
  const { harness, host, pending } = opened;
  const submit = host.querySelector('#worktree-cleanup-submit');
  submit.click();
  submit.click();
  assert.equal(requests.length, 1, 'the synchronous submit lock admits only one POST');

  first.reject(new Error('temporary cleanup failure'));
  await harness.act(() => first.promise.catch(() => {}));
  host.querySelector('#worktree-cleanup-submit').click();
  assert.equal(requests.length, 2, 'a failed attempt re-arms one exact retry');
  second.resolve({ removed: 1, branches: 0, skipped: 0, failed: 0, outcomes: [
    { path: '/repo-orphan', result: 'removed', detail: 'gone' },
  ] });
  await harness.act(() => second.promise);
  host.querySelector('#worktree-cleanup-submit').click();
  await pending;
  await flush(harness);
});

test('TCL-487 handoff lock remains reserved through every Preact cleanup close path', async (t) => {
  for (const closePath of ['Escape', 'backdrop', 'result-done', 'failure-cancel']) {
    await t.test(closePath, async (subtest) => {
      let cleanupCalls = 0;
      const opened = await mountCleanup(subtest, {
        cleanup: async () => {
          cleanupCalls += 1;
          if (closePath === 'failure-cancel') throw new Error('server recheck failed');
          return {
            removed: 1, branches: 0, skipped: 0, failed: 0,
            outcomes: [{ path: '/repo-orphan', result: 'removed', detail: 'gone' }],
          };
        },
      });
      const { harness, state: worktreeState, host } = opened;
      // Close the direct setup launch; the TCL-487 action below becomes the
      // sole launcher for this subtest's real Preact owner.
      worktreeState.close();
      await opened.pending;
      const [{ createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
        harness.importDashboardModule('js/transaction-dialog-state.js'),
        harness.importDashboardModule('js/transaction-dialog-actions.js'),
      ]);
      const transactionState = createTransactionDialogState();
      const transactionPending = transactionState.open({ kind: 'cleanup', mode: 'agents', candidates: [] });
      const transactionActions = createTransactionDialogActions({
        state: transactionState,
        openWorktreeCleanup: worktreeState.open,
      });
      const handoff = transactionActions.handoffCleanupWorktrees({ group: 'alpha' });
      let settled = false;
      handoff.finally(() => { settled = true; });
      await flush(harness);

      assert.equal(settled, false);
      assert.equal(transactionState.dialog.value, null, 'transaction dialog unpaints during handoff');
      assert.equal(await transactionState.open({ kind: 'delete-agent', agent: 'foreign' }), null,
        'the transaction lock remains reserved while cleanup is visible');
      const overlay = host.querySelector('#worktree-cleanup-modal');
      if (closePath === 'Escape') {
        const event = new harness.window.Event('keydown', { bubbles: true });
        Object.defineProperty(event, 'key', { value: 'Escape' });
        harness.document.dispatchEvent(event);
      } else if (closePath === 'backdrop') {
        await harness.act(() => harness.fireEvent(overlay, 'mousedown'));
      } else {
        host.querySelector('#worktree-cleanup-submit').click();
        await flush(harness);
        assert.equal(cleanupCalls, 1);
        assert.equal(settled, false);
        assert.equal(await transactionState.open({ kind: 'delete-agent', agent: 'blocked' }), null);
        if (closePath === 'failure-cancel') {
          assert.match(host.querySelector('#worktree-cleanup-error').textContent, /server recheck failed/);
          host.querySelector('#worktree-cleanup-cancel').click();
        } else {
          assert.match(host.querySelector('#worktree-cleanup-list').textContent, /gone/);
          host.querySelector('#worktree-cleanup-submit').click();
        }
      }

      await handoff;
      assert.equal(settled, true);
      assert.deepEqual(await transactionPending, {
        kind: 'cleanup-worktrees', descriptor: { group: 'alpha' },
      });
      await flush(harness);
    });
  }
});
