import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';
import { comboPick } from './combobox-helpers.mjs';

// The worktree and branch choices (git-picker.js, tcl-2a1ihf): a big repo has
// hundreds of branches, so these are type-to-filter boxes, not <select>s.

test('worktreeItems key a worktree by its path, name it by its branch, and offer a new one last', async (t) => {
  const harness = await createPreactHarness(t);
  const { worktreeItems } = await harness.importDashboardModule('js/git-picker.js');
  const items = worktreeItems([
    { path: '/repo', branch: 'main', is_main: true },
    { path: '/repo-wt', branch: '' },
    { branch: 'no-path' },
  ], { newKey: '__new__' });
  assert.deepEqual(items.map((item) => [item.key, item.label]), [
    ['wt:/repo', 'main [main]'], ['wt:/repo-wt', '(detached)'], ['__new__', '+ create new worktree…'],
  ]);
  assert.equal(worktreeItems([{ path: '/repo', branch: 'main' }]).length, 1, 'no new choice without newKey');
});

test('a branch is found by typing among hundreds, and a stale one stays, marked', async (t) => {
  const harness = await createPreactHarness(t);
  const { BranchCombobox } = await harness.importDashboardModule('js/git-picker.js');
  const branches = Array.from({ length: 300 }, (_, i) => `feat/topic-${i}`);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const changes = [];
  function Holder({ initial }) {
    const [value, setValue] = harness.hooks.useState(initial);
    return harness.html`<${BranchCombobox} id="base" value=${value} branches=${branches}
      onChange=${(next) => { changes.push(next); setValue(next); }} />`;
  }
  const mounted = await harness.mount(harness.html`<${Holder} initial="gone" />`, host);
  const box = host.querySelector('#base');
  assert.ok(box.classList.contains('missing'), 'a branch the repo no longer has is kept and marked');
  await harness.input(box.querySelector('input[role="combobox"]'), 'topic');
  const shown = box.querySelectorAll('[role="option"]').length;
  assert.ok(shown > 0 && shown <= 30, `a query lists its best matches up to the cap, not all 300 (${shown})`);
  await harness.input(box.querySelector('input[role="combobox"]'), 'topic-217');
  await comboPick(harness, box, 'feat/topic-217');
  assert.deepEqual(changes, ['feat/topic-217']);
  assert.equal(box.dataset.value, 'feat/topic-217');
  assert.equal(box.querySelector('.choice-combobox-clear'), null, 'a base branch cannot be cleared');
  await mounted.unmount();
});
