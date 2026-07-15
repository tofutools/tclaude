import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('worktree actions preserve scan scopes, explicit cleanup wire fields, and partial outcomes', async (t) => {
  const harness = await createPreactHarness(t);
  const { createWorktreeCleanupActions } = await harness.importDashboardModule(
    'js/worktree-cleanup-actions.js',
  );
  const calls = [];
  const notices = [];
  let refreshes = 0;
  const actions = createWorktreeCleanupActions({
    fetchImpl: async (url, init = {}) => {
      calls.push([url, init]);
      if (init.method === 'POST') {
        return new Response(JSON.stringify({
          removed: 1, branches: 1, skipped: 1, failed: 1,
          outcomes: [
            { path: '/removed', branch: 'feature', result: 'removed_with_branch', detail: 'gone' },
            { path: '/main', result: 'skipped', detail: 'main repo — never removed' },
            { path: '/failed', result: 'failed', detail: 'git refused' },
          ],
        }), { status: 200 });
      }
      return new Response(JSON.stringify({
        repo_roots: ['/repo'], worktrees: [{ path: '/repo-wt', checked: true }],
      }), { status: 200 });
    },
    refresh: async () => { refreshes += 1; },
    notify: (...args) => notices.push(args),
  });
  assert.deepEqual(await actions.scan('alpha/beta'), {
    repoRoots: ['/repo'], worktrees: [{ path: '/repo-wt', checked: true }],
  });
  assert.equal(calls[0][0], '/api/groups/alpha%2Fbeta/worktrees');
  await actions.scan('');
  assert.equal(calls[1][0], '/api/worktrees/cleanup');

  const request = Object.freeze({ paths: Object.freeze(['/removed', '/main', '/failed']), deleteBranches: true });
  const result = await actions.cleanup(request);
  assert.deepEqual(JSON.parse(calls[2][1].body), {
    paths: ['/removed', '/main', '/failed'], delete_branches: true,
  });
  assert.equal(result.outcomes[1].result, 'skipped');
  assert.equal(result.outcomes[2].detail, 'git refused');
  assert.equal(refreshes, 1);
  assert.deepEqual(notices, [['removed 1 worktree (+1 branch), 1 skipped, 1 failed', true]]);
});

test('worktree actions surface HTTP and plain-text server recheck failures', async (t) => {
  const harness = await createPreactHarness(t);
  const { createWorktreeCleanupActions } = await harness.importDashboardModule(
    'js/worktree-cleanup-actions.js',
  );
  const scan = createWorktreeCleanupActions({
    fetchImpl: async () => new Response(JSON.stringify({ error: 'repo scan unavailable' }), { status: 503 }),
  });
  await assert.rejects(scan.scan('alpha'), /repo scan unavailable/);
  const cleanup = createWorktreeCleanupActions({
    fetchImpl: async () => new Response('server recheck failed', { status: 409 }),
  });
  await assert.rejects(cleanup.cleanup({ paths: ['/repo-wt'], deleteBranches: false }),
    /server recheck failed/);
});

test('accepted cleanup outcomes do not wait for an unrelated dashboard refresh', async (t) => {
  const harness = await createPreactHarness(t);
  const { createWorktreeCleanupActions } = await harness.importDashboardModule(
    'js/worktree-cleanup-actions.js',
  );
  let releaseRefresh;
  const refreshPending = new Promise((resolve) => { releaseRefresh = resolve; });
  const actions = createWorktreeCleanupActions({
    fetchImpl: async () => new Response(JSON.stringify({
      removed: 1, branches: 0, skipped: 0, failed: 0,
      outcomes: [{ path: '/repo-wt', result: 'removed', detail: 'gone' }],
    }), { status: 200 }),
    refresh: () => refreshPending,
  });

  const result = await Promise.race([
    actions.cleanup({ paths: ['/repo-wt'], deleteBranches: false }),
    new Promise((_, reject) => setTimeout(
      () => reject(new Error('accepted cleanup was blocked behind refresh')), 250,
    )),
  ]);
  assert.equal(result.outcomes[0].detail, 'gone');
  releaseRefresh();
});
