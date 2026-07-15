import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function installWorktreeCleanupDOM(harness) {
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
}

function scanResponse() {
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
}

async function flush(harness, turns = 8) {
  await harness.act(async () => {
    for (let turn = 0; turn < turns; turn += 1) await Promise.resolve();
  });
}

test('worktree cleanup renders a successful scan response', async (t) => {
  const harness = await createPreactHarness(t);
  installWorktreeCleanupDOM(harness);

  const previousFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    assert.equal(url, '/api/groups/alpha/worktrees');
    return scanResponse();
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
  await flush(harness);

  assert.match(harness.document.querySelector('#worktree-cleanup-list').textContent, /feature/);
  assert.match(harness.document.querySelector('#worktree-cleanup-list').textContent, /\/repo-feature/);
  assert.match(harness.document.querySelector('#worktree-cleanup-categories').textContent, /orphans 1\/1/);
  assert.equal(harness.document.querySelector('#worktree-cleanup-count').textContent, '1 of 1 selected');
  assert.equal(settled, false, 'the driver retains ownership while its modal is visible');
  harness.document.querySelector('#worktree-cleanup-cancel').click();
  await closed;
  assert.equal(settled, true);
});

test('transaction handoff keeps the real worktree modal exclusive through every close path', async (t) => {
  for (const closePath of ['Escape', 'backdrop', 'success', 'failure']) {
    await t.test(closePath, async (subtest) => {
      const harness = await createPreactHarness(subtest);
      installWorktreeCleanupDOM(harness);
      const previousFetch = globalThis.fetch;
      let postCount = 0;
      globalThis.fetch = async (url, init = {}) => {
        if (url === '/api/groups/alpha/worktrees') return scanResponse();
        if (url === '/api/worktrees/cleanup' && init.method === 'POST') {
          postCount += 1;
          if (closePath === 'failure') {
            return new Response('server recheck failed', { status: 409 });
          }
          return new Response(JSON.stringify({ removed: 1 }), {
            status: 200, headers: { 'Content-Type': 'application/json' },
          });
        }
        // Successful cleanup triggers the ordinary dashboard refresh after the
        // modal has closed. Its result is not part of the ownership contract.
        return new Response(JSON.stringify({}), {
          status: 200, headers: { 'Content-Type': 'application/json' },
        });
      };
      subtest.after(() => { globalThis.fetch = previousFetch; });

      await harness.replaceDashboardModule('js/dashboard.js', `
        export let lastSnapshot = null;
        export function setLastSnapshot(value) { lastSnapshot = value; }
        export function webTerminalDefault() { return false; }
        export function sudoBadge() { return ''; }
      `);
      const [refreshModule, { createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
        harness.importDashboardModule('js/refresh.js'),
        harness.importDashboardModule('js/transaction-dialog-state.js'),
        harness.importDashboardModule('js/transaction-dialog-actions.js'),
      ]);
      const state = createTransactionDialogState();
      const actions = createTransactionDialogActions({
        state,
        openWorktreeCleanup: refreshModule.openWorktreeCleanup,
      });
      const pending = state.open({ kind: 'cleanup', mode: 'agents', candidates: [] });
      let settled = false;
      const handoff = actions.handoffCleanupWorktrees({ group: 'alpha' });
      handoff.finally(() => { settled = true; });
      await flush(harness);

      assert.equal(settled, false);
      assert.equal(state.dialog.value, null, 'the Preact dialog unpaints during handoff');
      assert.equal(await state.open({ kind: 'delete-agent', agent: 'foreign' }), null,
        'the transaction lock remains reserved while the legacy modal is visible');

      const overlay = harness.document.querySelector('#worktree-cleanup-modal');
      if (closePath === 'Escape') {
        const event = new harness.window.Event('keydown', { bubbles: true });
        Object.defineProperty(event, 'key', { value: 'Escape' });
        harness.document.dispatchEvent(event);
      } else if (closePath === 'backdrop') {
        harness.fireEvent(overlay, 'click');
      } else {
        harness.document.querySelector('#worktree-cleanup-submit').click();
        await flush(harness);
        assert.equal(postCount, 1);
        if (closePath === 'failure') {
          assert.equal(settled, false, 'a failed removal keeps the modal and lock active');
          assert.match(harness.document.querySelector('#worktree-cleanup-error').textContent,
            /server recheck failed/);
          assert.equal(await state.open({ kind: 'delete-agent', agent: 'still-blocked' }), null);
          harness.document.querySelector('#worktree-cleanup-cancel').click();
        }
      }

      await handoff;
      assert.equal(settled, true);
      assert.deepEqual(await pending, {
        kind: 'cleanup-worktrees', descriptor: { group: 'alpha' },
      });
    });
  }
});
