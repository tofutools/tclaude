import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('worktree cleanup state owns one promise through its actual close', async (t) => {
  const harness = await createPreactHarness(t);
  const { createWorktreeCleanupState } = await harness.importDashboardModule(
    'js/worktree-cleanup-state.js',
  );
  const state = createWorktreeCleanupState();
  const pending = state.open('alpha');
  let settled = false;
  pending.finally(() => { settled = true; });
  assert.equal(state.dialog.value.descriptor.group, 'alpha');
  assert.equal(settled, false);
  assert.equal(await state.open('beta'), null, 'a competing cleanup cannot replace the owner');
  state.finish({ response: { removed: 1 } });
  assert.deepEqual(await pending, { response: { removed: 1 } });
  assert.equal(settled, true);
  assert.equal(state.dialog.value, null);

  const disposed = state.open('gamma');
  state.dispose();
  assert.equal(await disposed, null);
});
