import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('worktree model preserves classifications, safety gates, and exact-path rescan choices', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/worktree-cleanup-model.js');
  const fresh = [
    { path: '/repo', branch: 'main', category: 'main', is_main: true, checked: true },
    { path: '/repo-wt', branch: 'old', category: 'orphan', checked: false, dirty: true },
    { path: '/repo-wt-child', branch: 'new', category: 'agent', checked: true },
  ];
  const choices = new Map([
    ['/repo', true],
    ['/repo-wt', true],
    ['/repo-w', true],
  ]);
  const reconciled = model.reconcileWorktreeCandidates(fresh, choices);

  assert.equal(reconciled[0].checked, false, 'main is never selectable, even if server or choice says yes');
  assert.equal(reconciled[1].checked, true, 'the exact touched path keeps its explicit choice');
  assert.equal(reconciled[1].category, 'orphan');
  assert.equal(reconciled[1].dirty, true);
  assert.equal(reconciled[2].checked, true, 'a merely similar path keeps its fresh server default');
  assert.ok(Object.isFrozen(reconciled));
  assert.ok(Object.isFrozen(reconciled[1]));

  assert.deepEqual(model.visibleWorktrees(reconciled, 'old').map((row) => row.path), ['/repo-wt']);
  assert.deepEqual(model.categoryWorktrees(reconciled, 'agent').map((row) => row.path), ['/repo-wt-child']);
  assert.deepEqual(model.dirtyWorktrees(reconciled).map((row) => row.path), ['/repo-wt']);
  assert.deepEqual(model.freezeWorktreeCleanupRequest(reconciled, false), {
    paths: ['/repo-wt', '/repo-wt-child'], deleteBranches: false,
  });

  model.reconcileWorktreeCandidates([
    { path: '/repo-other', category: 'orphan', checked: true },
  ], choices);
  assert.equal(choices.has('/repo-wt'), false,
    'a successful snapshot forgets choices for absent exact paths');

  const returned = model.reconcileWorktreeCandidates([{
    path: '/repo-wt', category: 'agent', checked: false, dirty: true,
    agents: [{ title: 'Replacement', conv_id: 'replacement-conv' }],
  }], choices);
  assert.equal(returned[0].checked, false,
    'a replacement at a forgotten path takes its fresh server default');
  assert.equal(returned[0].category, 'agent', 'fresh server classification still wins');
  assert.equal(returned[0].dirty, true);
  assert.equal(returned[0].agents[0].title, 'Replacement');
});

test('worktree model filters by path, branch, and agent identity and summarizes partial results', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/worktree-cleanup-model.js');
  const [candidate] = model.normalizeWorktreeCandidates([{
    path: '/repo/topic', branch: 'feature/one', category: 'agent',
    agents: [{ title: 'Build Agent', conv_id: 'conv-1234' }],
  }]);
  assert.equal(model.worktreeMatches(candidate, 'topic'), true);
  assert.equal(model.worktreeMatches(candidate, 'FEATURE/ONE'), true);
  assert.equal(model.worktreeMatches(candidate, 'build agent'), true);
  assert.equal(model.worktreeMatches(candidate, 'conv-1234'), true);
  assert.equal(model.worktreeMatches(candidate, 'missing'), false);
  assert.equal(model.worktreeCleanupSummary({ removed: 2, branches: 1, skipped: 1, failed: 1 }),
    'removed 2 worktrees (+1 branch), 1 skipped, 1 failed');
});
