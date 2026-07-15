import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('worktree cleanup renders a successful scan response', async (t) => {
  const harness = await createPreactHarness(t);
  harness.window.location = { search: '' };
  harness.document.body.innerHTML = `
    <div id="worktree-cleanup-modal">
      <h3 id="worktree-cleanup-title"></h3>
      <p id="worktree-cleanup-hint"></p>
      <button id="worktree-cleanup-select-all"></button>
      <button id="worktree-cleanup-select-none"></button>
      <button id="worktree-cleanup-rescan"></button>
      <input id="worktree-cleanup-search" />
      <span id="worktree-cleanup-count"></span>
      <div id="worktree-cleanup-categories"></div>
      <div id="worktree-cleanup-list"></div>
      <input id="worktree-cleanup-branches" type="checkbox" />
      <div id="worktree-cleanup-error"></div>
      <button id="worktree-cleanup-cancel"></button>
      <button id="worktree-cleanup-submit"></button>
    </div>
  `;

  const previousFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    assert.equal(url, '/api/groups/alpha/worktrees');
    return new Response(JSON.stringify({
      repo_roots: ['/repo'],
      worktrees: [
        {
          path: '/repo', branch: 'main', category: 'main', is_main: true,
          checked: false, dirty: false, agents: [], reason: 'main repo',
        },
        {
          path: '/repo-feature', branch: 'feature', category: 'orphan', is_main: false,
          checked: true, dirty: false, agents: [], reason: 'safe to remove',
        },
      ],
    }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  t.after(() => { globalThis.fetch = previousFetch; });

  // refresh.js and dashboard.js deliberately form a runtime cycle. Replace
  // only the copied dashboard entry point so importing the production cleanup
  // driver does not bootstrap every unrelated island into this focused DOM.
  await harness.replaceDashboardModule('js/dashboard.js', `
    export let lastSnapshot = null;
    export function setLastSnapshot(value) { lastSnapshot = value; }
    export function webTerminalDefault() { return false; }
    export function sudoBadge() { return ''; }
  `);
  const { openWorktreeCleanup } = await harness.importDashboardModule('js/refresh.js');
  let settled = false;
  const closed = openWorktreeCleanup('alpha');
  closed.finally(() => { settled = true; });
  await harness.act(async () => {
    for (let turn = 0; turn < 6; turn += 1) await Promise.resolve();
  });

  assert.match(harness.document.querySelector('#worktree-cleanup-list').textContent, /feature/);
  assert.match(harness.document.querySelector('#worktree-cleanup-list').textContent, /\/repo-feature/);
  assert.match(harness.document.querySelector('#worktree-cleanup-categories').textContent, /orphans 1\/1/);
  assert.equal(harness.document.querySelector('#worktree-cleanup-count').textContent, '1 of 1 selected');
  assert.equal(settled, false, 'the driver retains ownership while its modal is visible');
  harness.document.querySelector('#worktree-cleanup-cancel').click();
  await closed;
  assert.equal(settled, true);
});
